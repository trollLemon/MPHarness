package agent

import (
	"bytes"
	"context"
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

func TestCompactionFiresAtThreshold(t *testing.T) {
	first := chatResp("step", "", model.FinishReasonTool, []model.ResponseToolCall{
		{ID: "c1", Type: "function", Function: model.ResponseToolCallFunction{Name: "multipass_info", Arguments: model.ToolCallArguments{}}},
	}, &model.Usage{PromptTokens: 7000, CompletionTokens: 500, TotalTokens: 7500})
	second := chatResp("final", "", model.FinishReasonStop, nil, &model.Usage{PromptTokens: 1000, CompletionTokens: 50, TotalTokens: 1050})
	mock := &mockKronk{
		contextWidth: 8192,
		responses:    []model.ChatResponse{first, second},
	}
	agt := NewAgent(zerolog.Nop(), mock, 5, time.Second, 30*time.Second, defaultLLMConfig(), "run-compact-1")
	conf := config.Config{
		VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:  "m",
		Prompt: "do the thing",
		Compaction: config.CompactionConfig{
			Enabled: true, Threshold: 0.8, MaxSummaryTokens: 512,
			RecentTurns: 1, MinMessages: 2, MaxAttempts: 3,
		},
	}
	if err := agt.Execute(context.Background(), conf, multipass.New(zerolog.Nop())); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if mock.calls < 3 {
		t.Fatalf("want compaction summary call + 2 iterations, got %d chat calls", mock.calls)
	}
}

func TestCompactionDisabledSkips(t *testing.T) {
	mock := &mockKronk{
		contextWidth: 8192,
		responses: []model.ChatResponse{
			chatResp("final", "", model.FinishReasonStop, nil, &model.Usage{TotalTokens: 7500}),
		},
	}
	agt := NewAgent(zerolog.Nop(), mock, 3, time.Second, 30*time.Second, defaultLLMConfig(), "run-no-compact")
	conf := config.Config{
		VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:  "m",
		Prompt: "task",
		Compaction: config.CompactionConfig{
			Enabled: false, Threshold: 0.8, MaxSummaryTokens: 512,
			RecentTurns: 1, MinMessages: 2, MaxAttempts: 3,
		},
	}
	if err := agt.Execute(context.Background(), conf, multipass.New(zerolog.Nop())); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if mock.calls != 1 {
		t.Fatalf("disabled compaction must not add chat calls, got %d", mock.calls)
	}
}

func TestCompactionGaugeDropsAfterApply(t *testing.T) {
	reader := newManualReader(t)
	mock := &mockKronk{
		contextWidth: 8192,
		responses: []model.ChatResponse{
			chatResp("step", "", model.FinishReasonTool, []model.ResponseToolCall{
				{ID: "c1", Type: "function", Function: model.ResponseToolCallFunction{Name: "multipass_info", Arguments: model.ToolCallArguments{}}},
			}, &model.Usage{TotalTokens: 7500}),
			chatResp("final", "", model.FinishReasonStop, nil, &model.Usage{TotalTokens: 1050}),
		},
	}
	agt := NewAgent(zerolog.Nop(), mock, 5, time.Second, 30*time.Second, defaultLLMConfig(), "run-gauge")
	conf := config.Config{
		VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:  "m",
		Prompt: "task",
		Compaction: config.CompactionConfig{
			Enabled: true, Threshold: 0.8, MaxSummaryTokens: 512,
			RecentTurns: 1, MinMessages: 2, MaxAttempts: 3,
		},
	}
	if err := agt.Execute(context.Background(), conf, multipass.New(zerolog.Nop())); err != nil {
		t.Fatalf("execute: %v", err)
	}
	handovers := 0
	for _, req := range mock.captured {
		if isCompactionRequest(req) {
			handovers++
		}
	}
	if handovers == 0 {
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

func TestCompactionFailuresStopAfterMaxAttempts(t *testing.T) {
	makeToolResp := func() model.ChatResponse {
		return chatResp("step", "", model.FinishReasonTool, []model.ResponseToolCall{
			{ID: "c1", Type: "function", Function: model.ResponseToolCallFunction{Name: "multipass_info", Arguments: model.ToolCallArguments{}}},
		}, &model.Usage{TotalTokens: 7500})
	}
	mock := &mockKronk{
		contextWidth: 8192,
		responses: []model.ChatResponse{
			makeToolResp(), makeToolResp(), makeToolResp(),
			makeToolResp(), makeToolResp(), makeToolResp(),
			chatResp("final", "", model.FinishReasonStop, nil, nil),
		},
		failHandover: true,
	}
	agt := NewAgent(zerolog.Nop(), mock, 7, time.Second, 30*time.Second, defaultLLMConfig(), "run-fail")
	conf := config.Config{
		VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:  "m",
		Prompt: "task",
		Compaction: config.CompactionConfig{
			Enabled: true, Threshold: 0.8, MaxSummaryTokens: 512,
			RecentTurns: 1, MinMessages: 2, MaxAttempts: 3,
		},
	}
	if err := agt.Execute(context.Background(), conf, multipass.New(zerolog.Nop())); err != nil {
		t.Fatalf("compaction failure must not end the run: %v", err)
	}
	handovers := 0
	for _, req := range mock.captured {
		if isCompactionRequest(req) {
			handovers++
		}
	}
	if handovers != 3 {
		t.Fatalf("want exactly maxAttempts handover calls, got %d", handovers)
	}
}

func TestRunStatsLogged(t *testing.T) {
	var buf bytes.Buffer
	newManualReader(t)
	mock := &mockKronk{
		contextWidth: 8192,
		responses: []model.ChatResponse{
			chatResp(strings.Repeat("z", 3000), "", model.FinishReasonTool, []model.ResponseToolCall{
				{ID: "c1", Type: "function", Function: model.ResponseToolCallFunction{Name: "multipass_info", Arguments: model.ToolCallArguments{}}},
			}, &model.Usage{TotalTokens: 7500}),
			chatResp("final", "", model.FinishReasonStop, nil, &model.Usage{TotalTokens: 1050}),
		},
	}
	agt := NewAgent(zerolog.New(&buf), mock, 5, time.Second, 30*time.Second, defaultLLMConfig(), "run-stats")
	conf := config.Config{
		VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:  "m",
		Prompt: "task",
		Compaction: config.CompactionConfig{
			Enabled: true, Threshold: 0.8, MaxSummaryTokens: 512,
			RecentTurns: 0, MinMessages: 2, MaxAttempts: 3,
		},
	}
	if err := agt.Execute(context.Background(), conf, multipass.New(zerolog.Nop())); err != nil {
		t.Fatalf("execute: %v", err)
	}
	line := buf.String()
	for _, want := range []string{`"run complete"`, `"compactions_applied":1`, `"context_used_percent"`} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %q in run stats log", want)
		}
	}
}
