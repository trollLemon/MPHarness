package agent

import (
	"context"
	"errors"
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

func TestClassifyOutcome(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		exhaust bool
		want    runOutcome
	}{
		{"clean finish", nil, false, outcomeOK},
		{"budget exhausted", nil, true, outcomeBudgetExhausted},
		{"error", errors.New("boom"), false, outcomeError},
		{"timeout wins over budget", context.DeadlineExceeded, true, outcomeTimeout},
		{"deadline error", context.DeadlineExceeded, false, outcomeTimeout},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyOutcome(context.Background(), tt.err, tt.exhaust); got != tt.want {
				t.Errorf("classifyOutcome() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRecordRunSummaryEmitsEveryGaugeAndZeroesContext(t *testing.T) {
	rd := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rd))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	resetAgentMetrics()
	t.Cleanup(resetAgentMetrics)
	initAgentMetrics()

	ctx := withRunIdentity(t.Context(), "baseline:01J9", "baseline")
	a := &Agent{runID: "01J9", runName: "baseline", log: zerolog.Nop(), compactionsApplied: 3}

	a.recordRunSummary(ctx, runTotals{
		Iterations:       12,
		PromptTokens:     40000,
		CompletionTokens: 9000,
		ReclaimedTokens:  3000,
		ToolCalls:        map[string]int{"multipass_exec": 30},
		ToolFailures:     map[string]int{"multipass_exec": 2},
	}, outcomeOK, 90*time.Second)

	var rm metricdata.ResourceMetrics
	if err := rd.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}

	names := map[string]bool{}
	contextValues := []float64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			names[m.Name] = true
			if m.Name == "mph.context.tokens" {
				g, ok := m.Data.(metricdata.Gauge[int64])
				if !ok {
					t.Fatalf("mph.context.tokens is %T, want a gauge", m.Data)
				}
				for _, dp := range g.DataPoints {
					contextValues = append(contextValues, float64(dp.Value))
				}
			}
		}
	}

	for _, want := range []string{
		"mph.run.duration",
		"mph.run.tokens",
		"mph.run.iterations",
		"mph.run.tool_calls",
		"mph.run.tool_failures",
		"mph.run.compactions",
		"mph.run.finished",
	} {
		if !names[want] {
			t.Errorf("run summary gauge %q missing; got %v", want, names)
		}
	}

	// The terminal zero is the whole point: a finished run must not freeze its
	// context line at its last value.
	var zeroed bool
	for _, v := range contextValues {
		if v == 0 {
			zeroed = true
		}
	}
	if !zeroed {
		t.Errorf("mph.context.tokens = %v, want a terminal 0", contextValues)
	}
}

// The gauges are recorded once from a.totals, so a total that is never
// accumulated shows up as a correct-looking zero.
func TestExecuteAccumulatesRunTotals(t *testing.T) {
	tests := []struct {
		name           string
		responses      []model.ChatResponse
		wantIterations int64
		wantPrompt     int64
		wantToolCalls  string
		wantToolFails  string
	}{
		{
			name: "single turn, no tools",
			responses: []model.ChatResponse{chatResp("done", "", model.FinishReasonStop, nil,
				&model.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120})},
			wantIterations: 1,
			wantPrompt:     100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rd := sdkmetric.NewManualReader()
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rd))
			prev := otel.GetMeterProvider()
			otel.SetMeterProvider(mp)
			t.Cleanup(func() { otel.SetMeterProvider(prev) })
			resetAgentMetrics()
			t.Cleanup(resetAgentMetrics)

			mock := &mockKronk{contextWidth: 32768, responses: tt.responses}
			a := NewAgent(zerolog.Nop(), mock, Options{
				MaxIterations: 3, ChatTimeout: time.Second, TotalTimeout: 30 * time.Second,
				LLM: defaultLLMConfig(), RunID: "01J9", RunLabel: "baseline:01J9", RunName: "baseline",
			})
			conf := config.Config{VM: config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"}, Prompt: "task"}
			if err := a.Execute(t.Context(), conf, multipass.New(zerolog.Nop())); err != nil {
				t.Fatalf("Execute: %v", err)
			}

			got := map[string]int64{}
			var rm metricdata.ResourceMetrics
			if err := rd.Collect(t.Context(), &rm); err != nil {
				t.Fatalf("collect: %v", err)
			}
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					switch m.Name {
					case "mph.run.iterations", "mph.run.tokens", "mph.run.tool_calls":
						for _, dp := range m.Data.(metricdata.Gauge[int64]).DataPoints {
							tt2, _ := dp.Attributes.Value("tokens.type")
							got[m.Name+"/"+tt2.AsString()] = dp.Value
						}
					}
				}
			}

			if got["mph.run.iterations/"] != tt.wantIterations {
				t.Errorf("mph.run.iterations = %d, want %d", got["mph.run.iterations/"], tt.wantIterations)
			}
			if got["mph.run.tokens/prompt"] != tt.wantPrompt {
				t.Errorf("mph.run.tokens{prompt} = %d, want %d", got["mph.run.tokens/prompt"], tt.wantPrompt)
			}
		})
	}
}
