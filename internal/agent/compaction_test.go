package agent

import (
	"bytes"
	"context"
	"github.com/trollLemon/MPHarness/internal/output"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ardanlabs/kronk/sdk/kronk/model"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/trollLemon/MPHarness/internal/config"
	"github.com/trollLemon/MPHarness/internal/multipass"
)

func newManualReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		otel.SetMeterProvider(prev)
		tokensHist, tpsHist, iterationsCounter = nil, nil, nil
		toolDurationHist, toolCallsCounter, toolFailuresCounter = nil, nil, nil
		contextWindowGauge, contextTokensGauge = nil, nil
		compactionsCounter, compactionDurationHist, tokensReclaimedHist = nil, nil, nil
	})
	return reader
}

func lastGaugeValue(t *testing.T, reader *sdkmetric.ManualReader, name string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("%s is not Gauge[int64]: %T", name, m.Data)
			}
			if len(g.DataPoints) == 0 {
				t.Fatalf("%s has no points", name)
			}
			return g.DataPoints[len(g.DataPoints)-1].Value
		}
	}
	t.Fatalf("metric %s not found", name)
	return 0
}

func bigToolStep(content string, total int) model.ChatResponse {
	return chatResp(content, "", model.FinishReasonTool, []model.ResponseToolCall{
		{ID: "c1", Type: "function", Function: model.ResponseToolCallFunction{Name: "multipass_info", Arguments: model.ToolCallArguments{}}},
	}, &model.Usage{TotalTokens: total})
}

func countHandovers(mock *mockKronk) int {
	handovers := 0
	for _, req := range mock.captured {
		if isCompactionRequest(req) {
			handovers++
		}
	}
	return handovers
}

func enabledCompaction() config.CompactionConfig {
	return config.CompactionConfig{
		Enabled: true, Threshold: 0.8, MaxSummaryTokens: 512,
		RecentTurns: 1, MinMessages: 2, MaxAttempts: 3,
	}
}

func TestExecuteCompaction(t *testing.T) {
	big := strings.Repeat("s", 4000)
	final := chatResp("final", "", model.FinishReasonStop, nil, &model.Usage{TotalTokens: 1050})
	tests := []struct {
		name         string
		runID        string
		responses    []model.ChatResponse
		failHandover bool
		compaction   config.CompactionConfig
		maxIter      int
		check        func(t *testing.T, mock *mockKronk, logs string)
	}{
		{
			name:       "fires at threshold",
			runID:      "run-compact-1",
			responses:  []model.ChatResponse{bigToolStep(big, 4000), bigToolStep(big, 7500), final},
			compaction: enabledCompaction(),
			maxIter:    5,
			check: func(t *testing.T, mock *mockKronk, _ string) {
				if mock.calls < 3 {
					t.Fatalf("want compaction summary call + 2 iterations, got %d chat calls", mock.calls)
				}
			},
		},
		{
			name:      "disabled skips",
			runID:     "run-no-compact",
			responses: []model.ChatResponse{chatResp("final", "", model.FinishReasonStop, nil, &model.Usage{TotalTokens: 7500})},
			compaction: config.CompactionConfig{
				Enabled: false, Threshold: 0.8, MaxSummaryTokens: 512,
				RecentTurns: 1, MinMessages: 2, MaxAttempts: 3,
			},
			maxIter: 3,
			check: func(t *testing.T, mock *mockKronk, _ string) {
				if mock.calls != 1 {
					t.Fatalf("disabled compaction must not add chat calls, got %d", mock.calls)
				}
			},
		},
		{
			name:  "failures stop after max attempts",
			runID: "run-fail",
			responses: []model.ChatResponse{
				bigToolStep(big, 7500), bigToolStep(big, 7500), bigToolStep(big, 7500),
				bigToolStep(big, 7500), bigToolStep(big, 7500), bigToolStep(big, 7500),
				chatResp("final", "", model.FinishReasonStop, nil, nil),
			},
			failHandover: true,
			compaction:   enabledCompaction(),
			maxIter:      7,
			check: func(t *testing.T, mock *mockKronk, _ string) {
				if got := countHandovers(mock); got != 3 {
					t.Fatalf("want exactly maxAttempts handover calls, got %d", got)
				}
			},
		},
		{
			name:  "run stats logged",
			runID: "run-stats",
			responses: []model.ChatResponse{
				bigToolStep(strings.Repeat("z", 3000), 7500),
				chatResp("final", "", model.FinishReasonStop, nil, &model.Usage{TotalTokens: 1050}),
			},
			compaction: func() config.CompactionConfig {
				c := enabledCompaction()
				c.RecentTurns = 0
				return c
			}(),
			maxIter: 5,
			check: func(t *testing.T, _ *mockKronk, logs string) {
				for _, want := range []string{`"run complete"`, `"compacted":1`, `"ctx_pct"`} {
					if !strings.Contains(logs, want) {
						t.Errorf("missing %q in run stats log", want)
					}
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			newManualReader(t)
			mock := &mockKronk{contextWidth: 8192, responses: tt.responses, failHandover: tt.failHandover}
			agt := NewAgent(zerolog.New(&buf), mock, tt.maxIter, time.Second, 30*time.Second, defaultLLMConfig(), tt.runID)
			conf := config.Config{
				VM:         config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
				Model:      "m",
				Prompt:     "task",
				Compaction: tt.compaction,
			}
			if err := agt.Execute(context.Background(), conf, multipass.New(zerolog.Nop())); err != nil {
				t.Fatalf("compaction failure must not end the run: %v", err)
			}
			tt.check(t, mock, buf.String())
		})
	}
}

func TestCompactionGaugeDropsAfterApply(t *testing.T) {
	reader := newManualReader(t)
	big := strings.Repeat("s", 4000)
	mock := &mockKronk{
		contextWidth: 8192,
		responses: []model.ChatResponse{
			bigToolStep(big, 4000),
			bigToolStep(big, 7500),
			chatResp("final", "", model.FinishReasonStop, nil, &model.Usage{TotalTokens: 1050}),
		},
	}
	agt := NewAgent(zerolog.Nop(), mock, 5, time.Second, 30*time.Second, defaultLLMConfig(), "run-gauge")
	conf := config.Config{
		VM:         config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:      "m",
		Prompt:     "task",
		Compaction: enabledCompaction(),
	}
	if err := agt.Execute(context.Background(), conf, multipass.New(zerolog.Nop())); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if countHandovers(mock) == 0 {
		t.Fatalf("no compaction summary call ran; gauge drop would not prove compaction")
	}
	last := lastGaugeValue(t, reader, "mph.context.tokens")
	if last >= 7500 {
		t.Fatalf("post-compaction gauge %d must drop below pre-compaction 7500", last)
	}
	if last < 0 {
		t.Fatalf("gauge must never go negative, got %d", last)
	}
}

func TestRevertedDoesNotBurnAttempts(t *testing.T) {
	mock := &mockKronk{contextWidth: 8192}
	agt := NewAgent(zerolog.Nop(), mock, 5, time.Second, 30*time.Second, defaultLLMConfig(), "run-revert")
	conf := config.Config{
		VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:  "m",
		Prompt: "task",
		Compaction: config.CompactionConfig{
			Enabled: true, Threshold: 0.8, MaxSummaryTokens: 512,
			RecentTurns: 1, MinMessages: 2, MaxAttempts: 3,
		},
	}
	tiny := []model.D{
		{"role": "system", "content": "sys"},
		{"role": "user", "content": "task"},
	}
	agt.conversation, agt.contextTokens, agt.toolResultBudget = tiny, 7500, 4096
	outcome := agt.maybeCompact(context.Background(), conf)
	if outcome != "reverted" {
		t.Fatalf("want reverted for tiny history, got %q", outcome)
	}
	if agt.compactionAttempts != 0 {
		t.Fatalf("revert must not burn the failure budget, attempts=%d", agt.compactionAttempts)
	}
	if mock.calls != 0 {
		t.Fatalf("pre-check revert must skip the summarizer call, got %d calls", mock.calls)
	}
}

// A compaction that succeeds proves the summarizer and handover are healthy, so
// the failure budget must be restored. Otherwise a single failure anywhere in
// a long run permanently disables compaction after MaxAttempts.
func TestAppliedCompactionResetsAttempts(t *testing.T) {
	mock := &mockKronk{contextWidth: 8192}
	agt := NewAgent(zerolog.Nop(), mock, 5, time.Second, 30*time.Second, defaultLLMConfig(), "run-reset")
	conf := config.Config{
		VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:  "m",
		Prompt: "task",
		Compaction: config.CompactionConfig{
			Enabled: true, Threshold: 0.8, MaxSummaryTokens: 512,
			RecentTurns: 1, MinMessages: 2, MaxAttempts: 3,
		},
	}
	big := strings.Repeat("s", 4000)
	conv := []model.D{
		{"role": "system", "content": "sys"},
		{"role": "user", "content": "task"},
		{"role": "assistant", "content": big},
		{"role": "user", "content": big},
		{"role": "assistant", "content": big},
	}
	mock.responses = []model.ChatResponse{chatResp("a faithful summary of the work so far", "", model.FinishReasonStop, nil, nil)}

	agt.conversation, agt.contextTokens, agt.toolResultBudget = conv, 7500, 4096
	agt.compactionAttempts = 2
	outcome := agt.maybeCompact(context.Background(), conf)
	if outcome != "applied" {
		t.Fatalf("want applied, got %q", outcome)
	}
	if agt.compactionAttempts != 0 {
		t.Fatalf("a successful compaction must restore the failure budget, attempts=%d", agt.compactionAttempts)
	}
}

// The bug this guards: context was measured from the model's reported usage
// alone, which cannot include the tool results appended after the reply. Since
// maybeCompact runs before the chat call, that made the model get called with a
// context already past the threshold: the tool result landed, the measurement
// still said "under budget", so the next call went out oversized and compaction
// only reacted one full turn late.
func TestCompactionSeesToolResultBeforeNextModelCall(t *testing.T) {
	const window = 8192
	const usedBeforeReply = 5700 // 70% of the window: legal to call the model
	threshold := int64(usedBeforeReply + 1)

	mock := &mockKronk{contextWidth: window, responses: []model.ChatResponse{
		chatResp("", "", model.FinishReasonTool, []model.ResponseToolCall{{
			ID: "c1", Type: "function",
			Function: model.ResponseToolCallFunction{
				Name: "multipass_exec", Arguments: model.ToolCallArguments{"command": "cat big", "capture": true},
			},
		}}, &model.Usage{TotalTokens: usedBeforeReply, PromptTokens: usedBeforeReply - 10, CompletionTokens: 10}),
	}}
	// ~5000 tokens of tool output, which the reported usage cannot contain.
	big := strings.Repeat("z", 20000)

	cfg := config.Config{
		VM:         config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:      "m",
		Prompt:     "task",
		Compaction: enabledCompaction(),
		Output:     config.OutputConfig{Enabled: true, Mode: "auto", MaxCommandSize: 1 << 20, MaxTotalSize: 1 << 26, SearchMaxMatches: 200},
	}
	f := &fakeOutputExec{fn: func(name, cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "grep -a -c"):
			return "0", nil
		case strings.Contains(cmd, "__MPH_EXIT_"):
			return exitMarker(cmd) + "0", nil
		case strings.Contains(cmd, "cat "):
			return big, nil
		case strings.Contains(cmd, "wc -c"):
			return strconv.Itoa(len(big)), nil
		case strings.Contains(cmd, "wc -l"):
			return "1", nil
		}
		return "", nil
	}}
	store := output.NewStore(f, "vm", "/tmp/mph-output/run1", cfg.Output, 1<<20, nil)
	agt := NewAgent(zerolog.Nop(), mock, 3, time.Second, 30*time.Second, defaultLLMConfig(), "run-grow")
	agt.conversation = buildInitialConversation(cfg)
	agt.toolDocs = buildToolDocuments(cfg)
	agt.toolResultBudget = toolResultBudgetBytes(cfg, window)
	agt.store = store

	if err := agt.runIteration(context.Background(), multipass.New(zerolog.Nop()), cfg); err != nil {
		t.Fatalf("runIteration: %v", err)
	}

	// The reply was fine to send, but the tool result it triggered is what
	// pushes the real context over. If the measurement stops at the reported
	// usage, the next iteration calls the model before compacting.
	if agt.contextTokens <= threshold {
		t.Fatalf("context %d must include the tool result appended after the reply reported %d, or the next model call goes out over budget", agt.contextTokens, usedBeforeReply)
	}
	if outcome := agt.maybeCompact(context.Background(), cfg); outcome != "applied" {
		t.Fatalf("compaction must fire before the next model call, got %q", outcome)
	}
	if agt.contextTokens >= threshold {
		t.Fatalf("compaction did not reclaim context: %d still over budget", agt.contextTokens)
	}
}
