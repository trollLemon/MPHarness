package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	mphotel "github.com/trollLemon/MPHarness/internal/otel"
	"github.com/trollLemon/MPHarness/internal/output"
)

func resetAgentMetrics() {
	contextTokensGauge, contextWindowGauge = nil, nil
	iterationDurationHist, chatDurationHist, prefillTTFTHist = nil, nil, nil
	prefillCachedHist, agentTokensHist, reasoningTokensHist, tpsHist = nil, nil, nil, nil
	toolDurationHist, toolCallsCounter, toolFailuresCounter = nil, nil, nil
	compactionsCounter, compactionDurationHist, tokensReclaimedHist = nil, nil, nil
	runDurationGauge, runTokensGauge, runIterationsGauge = nil, nil, nil
	runToolCallsGauge, runToolFailuresGauge, runCompactionsGauge, runFinishedGauge = nil, nil, nil, nil
}

func newManualReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		otel.SetMeterProvider(prev)
		resetAgentMetrics()
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

type windowEnforcingKronk struct {
	*mockKronk
}

func promptTokens(req model.D) int {
	n := 0
	if msgs, ok := req["messages"].([]model.D); ok {
		for _, m := range msgs {
			if c, _ := m["content"].(string); c != "" {
				n += len(c) / bytesPerToken
			}
		}
	}
	if tools, ok := req["tools"].([]model.D); ok {
		for _, d := range tools {
			b, _ := json.Marshal(d)
			n += len(b) / bytesPerToken
		}
	}
	return n
}

func (w *windowEnforcingKronk) Chat(ctx context.Context, req model.D) (model.ChatResponse, error) {
	n := promptTokens(req)
	if n > w.contextWidth {
		return model.ChatResponse{}, fmt.Errorf("input tokens [%d] exceed context window [%d]", n, w.contextWidth)
	}
	resp, err := w.mockKronk.Chat(ctx, req)
	if err == nil && len(resp.Choices) > 0 {
		completion := 0
		if resp.Usage != nil {
			completion = resp.Usage.CompletionTokens
		}
		resp.Usage = &model.Usage{PromptTokens: n, CompletionTokens: completion, TotalTokens: n + completion}
	}
	return resp, err
}

func enabledCompaction() config.CompactionConfig {
	return config.CompactionConfig{
		Enabled: true, Threshold: 0.8, MaxSummaryTokens: 512,
		MinMessages: 2, MaxAttempts: 3,
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
				MinMessages: 2, MaxAttempts: 3,
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
			agt := NewAgent(zerolog.New(&buf), mock, Options{
				MaxIterations: tt.maxIter,
				ChatTimeout:   time.Second,
				TotalTimeout:  30 * time.Second,
				LLM:           defaultLLMConfig(),
				RunID:         tt.runID,
			})
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
	agt := NewAgent(zerolog.Nop(), mock, Options{
		MaxIterations: 5,
		ChatTimeout:   time.Second,
		TotalTimeout:  30 * time.Second,
		LLM:           defaultLLMConfig(),
		RunID:         "run-gauge",
	})
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
	agt := NewAgent(zerolog.Nop(), mock, Options{
		MaxIterations: 5,
		ChatTimeout:   time.Second,
		TotalTimeout:  30 * time.Second,
		LLM:           defaultLLMConfig(),
		RunID:         "run-revert",
	})
	conf := config.Config{
		VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:  "m",
		Prompt: "task",
		Compaction: config.CompactionConfig{
			Enabled: true, Threshold: 0.8, MaxSummaryTokens: 512,
			MinMessages: 2, MaxAttempts: 3,
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

func TestAppliedCompactionResetsAttempts(t *testing.T) {
	mock := &mockKronk{contextWidth: 8192}
	agt := NewAgent(zerolog.Nop(), mock, Options{
		MaxIterations: 5,
		ChatTimeout:   time.Second,
		TotalTimeout:  30 * time.Second,
		LLM:           defaultLLMConfig(),
		RunID:         "run-reset",
	})
	conf := config.Config{
		VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:  "m",
		Prompt: "task",
		Compaction: config.CompactionConfig{
			Enabled: true, Threshold: 0.8, MaxSummaryTokens: 512,
			MinMessages: 2, MaxAttempts: 3,
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

func TestPostCompactionContextKeepsToolSchemaOverhead(t *testing.T) {
	big := strings.Repeat("s", 4000)
	base := []model.D{
		{"role": "system", "content": "sys"},
		{"role": "user", "content": "task"},
		{"role": "assistant", "content": big},
		{"role": "user", "content": big},
		{"role": "assistant", "content": big},
	}

	tests := []struct {
		name         string
		schemas      int
		wantOverhead bool
	}{
		{name: "no tool schemas", schemas: 0},
		{name: "one small schema", schemas: 1},
		// Big enough that dropping the schemas from the post-compaction figure
		// would be obvious rather than a rounding difference.
		{name: "eight large schemas", schemas: 8, wantOverhead: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockKronk{contextWidth: 8192, responses: []model.ChatResponse{
				chatResp("a faithful summary of the work so far", "", model.FinishReasonStop, nil, nil)}}
			agt := NewAgent(zerolog.Nop(), mock, Options{
				MaxIterations: 5, ChatTimeout: time.Second, TotalTimeout: 30 * time.Second,
				LLM: defaultLLMConfig(), RunID: "run-overhead",
			})
			conf := config.Config{
				VM:    config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
				Model: "m", Prompt: "task", Compaction: enabledCompaction(),
			}
			for i := 0; i < tt.schemas; i++ {
				agt.toolDocs = append(agt.toolDocs, model.D{"type": "function", "function": model.D{
					"name": "multipass_tool", "description": strings.Repeat("d", 2000),
				}})
			}
			schemaJSON, err := json.Marshal(agt.toolDocs)
			if err != nil {
				t.Fatalf("marshal tool docs: %v", err)
			}
			schemaTokens := int64(agt.estimateTokens(context.Background(), string(schemaJSON)))

			agt.conversation, agt.contextTokens, agt.toolResultBudget = base, 7500, 4096
			if outcome := agt.maybeCompact(context.Background(), conf); outcome != "applied" {
				t.Fatalf("want applied, got %q", outcome)
			}
			if !tt.wantOverhead {
				return
			}
			if agt.contextTokens <= schemaTokens {
				t.Fatalf("post-compaction context %d omits the ~%d tokens of tool schemas every request still pays",
					agt.contextTokens, schemaTokens)
			}
		})
	}
}

func TestCompactionSeesToolResultBeforeNextModelCall(t *testing.T) {
	const window = 32768
	big := strings.Repeat("z", 12000)

	cfg := config.Config{
		VM:         config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:      "m",
		Prompt:     "task",
		Compaction: enabledCompaction(),
		Output:     config.OutputConfig{Enabled: true, Mode: "auto", MaxCommandSize: 1 << 20, MaxTotalSize: 1 << 26, SearchMaxMatches: 200},
	}
	// The mock has to report what the conversation it was handed actually costs,
	// or the guard is deciding on numbers no real run produces.
	initial := buildInitialConversation(cfg)
	agt := NewAgent(zerolog.Nop(), &mockKronk{contextWidth: window}, Options{
		MaxIterations: 3,
		ChatTimeout:   time.Second,
		TotalTimeout:  30 * time.Second,
		LLM:           defaultLLMConfig(),
		RunID:         "run-grow",
	})
	agt.toolDocs = buildToolDocuments(cfg)
	agt.toolResultBudget = toolResultBudgetBytes(cfg, window)
	// ~24000 tokens of history, so the context is already near the 0.80
	// threshold and the one tool result is what tips it over.
	const fillerBytes = 4800
	const fillerTurns = 10
	filler := strings.Repeat("h", fillerBytes)
	for i := 0; i < fillerTurns; i++ {
		agt.conversation = append(agt.conversation,
			model.D{"role": "assistant", "content": filler},
			model.D{"role": "user", "content": filler},
		)
	}
	// Reported usage is the message content the request carried.
	reported := int64((2*fillerTurns*fillerBytes + len(initial[0]["content"].(string))) / bytesPerToken)
	threshold := int64(cfg.Compaction.Threshold * float64(window))

	mock := &mockKronk{contextWidth: window, responses: []model.ChatResponse{
		chatResp("", "", model.FinishReasonTool, []model.ResponseToolCall{{
			ID: "c1", Type: "function",
			Function: model.ResponseToolCallFunction{
				Name: "multipass_exec", Arguments: model.ToolCallArguments{"command": "cat big", "capture": true},
			},
		}}, &model.Usage{TotalTokens: int(reported), PromptTokens: int(reported) - 10, CompletionTokens: 10}),
	}}
	agt.krn = mock

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
	// Cutoff must match what NewAgent passes the store in production.
	store := output.NewStore(f, "vm", "/tmp/mph-output/run1", cfg.Output, int64(agt.toolResultBudget), nil)
	agt.store = store

	if reported >= threshold {
		t.Fatalf("setup: reported %d must sit below the %d threshold, or there is nothing to react to", reported, threshold)
	}
	if err := agt.runIteration(context.Background(), multipass.New(zerolog.Nop()), cfg); err != nil {
		t.Fatalf("runIteration: %v", err)
	}

	// The reply was fine to send, but the tool result it triggered is what
	// pushes the real context over. If the measurement stops at the reported
	// usage, the next iteration calls the model before compacting.
	if agt.contextTokens <= reported {
		t.Fatalf("context %d must include the tool result appended after the reply reported %d, or the next model call goes out over budget", agt.contextTokens, reported)
	}
	if agt.contextTokens < threshold {
		t.Fatalf("context %d must reach the %d threshold for the next call to be oversized", agt.contextTokens, threshold)
	}
	if outcome := agt.maybeCompact(context.Background(), cfg); outcome != "applied" {
		t.Fatalf("compaction must fire before the next model call, got %q", outcome)
	}
	if agt.contextTokens >= threshold {
		t.Fatalf("compaction did not reclaim context: %d still over budget", agt.contextTokens)
	}
}

func TestCompactionAppliesBeforeWindowIsExceeded(t *testing.T) {
	base := config.Config{
		VM:    config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model: "m",
		Prompt: "Inside the Multipass VM:\n" +
			"1. Run uname -a and note the kernel release field.\n" +
			"2. Write that release string into /tmp/steps.txt.\n" +
			"3. Append the output of hostname as a second line.\n",
		LLM: config.LLMConfig{Temperature: 0.0, ToolChoice: "auto"},
		Output: config.OutputConfig{
			Enabled: true, Mode: "auto", InlineMaxSize: 0,
			MaxCommandSize: 64 << 20, MaxTotalSize: 512 << 20, SearchMaxMatches: 200,
		},
	}
	tests := []struct {
		name      string
		window    int
		threshold float64
		stepBytes int
		steps     int
	}{
		{name: "small window", window: 4096, threshold: 0.71, stepBytes: 1400, steps: 8},
		// An auto-tuned window, which is what testing/compaction.yaml asks for.
		{name: "auto tuned window", window: 32768, threshold: 0.71, stepBytes: 6000, steps: 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			cfg.LLM.ContextWindow = tt.window
			cfg.Compaction = config.CompactionConfig{
				Enabled: true, Threshold: tt.threshold, MaxSummaryTokens: 512,
				MinMessages: 6, MaxAttempts: 3,
			}
			step := strings.Repeat("k", tt.stepBytes)
			responses := make([]model.ChatResponse, 0, tt.steps+1)
			for i := 0; i < tt.steps; i++ {
				responses = append(responses, bigToolStep(step, 0))
			}
			responses = append(responses, chatResp("done", "", model.FinishReasonStop, nil, &model.Usage{TotalTokens: 400}))

			mock := &windowEnforcingKronk{&mockKronk{contextWidth: tt.window, responses: responses}}
			agt := NewAgent(zerolog.Nop(), mock, Options{
				MaxIterations: tt.steps + 4, ChatTimeout: time.Second, TotalTimeout: 30 * time.Second,
				LLM: cfg.LLM, RunID: "run-window",
			})
			if err := agt.Execute(context.Background(), cfg, multipass.New(zerolog.Nop())); err != nil {
				t.Fatalf("run grew past the window without compacting: %v (handovers=%d)", err, countHandovers(mock.mockKronk))
			}
			if agt.compactionsApplied == 0 {
				t.Fatalf("no compaction applied; handovers=%d", countHandovers(mock.mockKronk))
			}
		})
	}
}

// slowSummarizerKronk makes the summarizer's chat call take measurable time, so
// a compaction that lands in the histogram is large enough to tell seconds from
// milliseconds.
type slowSummarizerKronk struct {
	*mockKronk
}

func (s *slowSummarizerKronk) Chat(ctx context.Context, req model.D) (model.ChatResponse, error) {
	if isCompactionRequest(req) {
		time.Sleep(50 * time.Millisecond)
		return chatResp("work summary", "", model.FinishReasonStop, nil, nil), nil
	}
	return s.mockKronk.Chat(ctx, req)
}

// The compaction histogram is named without a unit, so a surviving query that
// still expects milliseconds would read 1000x wrong instead of failing.
func TestCompactionDurationIsRecordedInSeconds(t *testing.T) {
	reader := newManualReader(t)
	initAgentMetrics()

	big := strings.Repeat("s", 4000)
	mock := &slowSummarizerKronk{mockKronk: &mockKronk{contextWidth: 8192, responses: []model.ChatResponse{
		bigToolStep(big, 7500), chatResp("final", "", model.FinishReasonStop, nil, &model.Usage{TotalTokens: 800}),
	}}}
	agent := NewAgent(zerolog.Nop(), mock, Options{
		MaxIterations: 3, ChatTimeout: time.Second, TotalTimeout: 30 * time.Second,
		LLM: defaultLLMConfig(), RunID: "run-compact-seconds",
	})
	conf := config.Config{
		VM:         config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Prompt:     "task",
		Compaction: enabledCompaction(),
	}
	ctx := mphotel.WithRunIdentity(context.Background(), "baseline:run-compact-seconds", "baseline")
	if err := agent.Execute(ctx, conf, multipass.New(zerolog.Nop())); err != nil {
		t.Fatalf("execute: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var found bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "mph.context.compaction.duration" {
				continue
			}
			found = true
			for _, dp := range m.Data.(metricdata.Histogram[float64]).DataPoints {
				// The summarizer sleeps 50ms, so seconds land near 0.05 and
				// milliseconds land near 50.
				if dp.Sum > 5 {
					t.Errorf("mph.context.compaction.duration sum = %v, too large to be seconds; the unit looks like milliseconds", dp.Sum)
				}
			}
		}
	}
	if !found {
		t.Fatal("mph.context.compaction.duration was never recorded")
	}
}
