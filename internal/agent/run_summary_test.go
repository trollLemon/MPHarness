package agent

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ardanlabs/kronk/sdk/kronk/model"
	"github.com/rs/zerolog"

	"github.com/trollLemon/MPHarness/internal/config"
	"github.com/trollLemon/MPHarness/internal/multipass"
	mphotel "github.com/trollLemon/MPHarness/internal/otel"
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

func TestLogRunSummaryEmitsEveryField(t *testing.T) {
	var buf bytes.Buffer
	ctx := mphotel.WithRunIdentity(t.Context(), "baseline:01J9", "baseline")
	a := &Agent{
		runID:              "01J9",
		log:                zerolog.New(&buf),
		krn:                &mockKronk{contextWidth: 8192},
		compactionsApplied: 3,
		totals: runTotals{
			Iterations:       12,
			PromptTokens:     40000,
			CompletionTokens: 9000,
			ReclaimedTokens:  3000,
			ToolCalls:        30,
			ToolFailures:     2,
		},
	}

	a.logRunSummary(ctx, outcomeOK, 90*time.Second)

	summary := onlyEvent(t, decodeEvents(t, &buf), eventRunSummary)
	if summary["outcome"] != string(outcomeOK) {
		t.Errorf("outcome = %v, want %q", summary["outcome"], outcomeOK)
	}
	for key, want := range map[string]float64{
		"iterations": 12, "compactions": 3, "dur_ms": 90000, "avg_iter_ms": 7500,
		"prompt_tokens": 40000, "completion_tokens": 9000, "reclaimed_tokens": 3000,
		"tool_calls": 30, "tool_failures": 2, "ctx_window": 8192,
	} {
		if got := summary[key]; got != want {
			t.Errorf("run_summary %s = %v, want %v", key, got, want)
		}
	}
}

func TestLogRunSummaryWithoutIterations(t *testing.T) {
	var buf bytes.Buffer
	a := &Agent{log: zerolog.New(&buf), krn: &mockKronk{}}

	a.logRunSummary(t.Context(), outcomeError, time.Second)

	summary := onlyEvent(t, decodeEvents(t, &buf), eventRunSummary)
	if summary["avg_iter_ms"] != float64(0) {
		t.Errorf("avg_iter_ms = %v, want 0 when no iteration ran", summary["avg_iter_ms"])
	}
}

// The summary is assembled from a.totals, so a total that is never
// accumulated shows up as a correct-looking zero.
func TestExecuteAccumulatesRunTotals(t *testing.T) {
	toolCall := []model.ResponseToolCall{
		{ID: "c1", Type: "function", Function: model.ResponseToolCallFunction{Name: "multipass_info", Arguments: model.ToolCallArguments{}}},
	}
	tests := []struct {
		name           string
		responses      []model.ChatResponse
		wantIterations float64
		wantPrompt     float64
		wantToolCalls  float64
	}{
		{
			name: "single turn, no tools",
			responses: []model.ChatResponse{chatResp("done", "", model.FinishReasonStop, nil,
				&model.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120})},
			wantIterations: 1,
			wantPrompt:     100,
		},
		{
			name: "tool turn then finish",
			responses: []model.ChatResponse{
				chatResp("", "", model.FinishReasonTool, toolCall, &model.Usage{PromptTokens: 100, TotalTokens: 110}),
				chatResp("done", "", model.FinishReasonStop, nil, &model.Usage{PromptTokens: 150, TotalTokens: 160}),
			},
			wantIterations: 2,
			wantPrompt:     250,
			wantToolCalls:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			mock := &mockKronk{contextWidth: 32768, responses: tt.responses}
			a := NewAgent(zerolog.New(&buf), mock, Options{
				MaxIterations: 3, ChatTimeout: time.Second, TotalTimeout: 30 * time.Second,
				LLM: defaultLLMConfig(), RunID: "01J9",
			})
			conf := config.Config{VM: config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"}, Prompt: "task"}
			ctx := mphotel.WithRunIdentity(t.Context(), "baseline:01J9", "baseline")
			if err := a.Execute(ctx, conf, multipass.New(zerolog.Nop())); err != nil {
				t.Fatalf("Execute: %v", err)
			}

			summary := onlyEvent(t, decodeEvents(t, &buf), eventRunSummary)
			if got := summary["iterations"]; got != tt.wantIterations {
				t.Errorf("iterations = %v, want %v", got, tt.wantIterations)
			}
			if got := summary["prompt_tokens"]; got != tt.wantPrompt {
				t.Errorf("prompt_tokens = %v, want %v", got, tt.wantPrompt)
			}
			if got := summary["tool_calls"]; got != tt.wantToolCalls {
				t.Errorf("tool_calls = %v, want %v", got, tt.wantToolCalls)
			}
		})
	}
}

func TestExecuteAccumulatesReclaimedTokens(t *testing.T) {
	var buf bytes.Buffer
	big := strings.Repeat("r", 4000)
	mock := &mockKronk{contextWidth: 8192, responses: []model.ChatResponse{
		bigToolStep(big, 7500), bigToolStep(big, 7500),
		chatResp("final", "", model.FinishReasonStop, nil, &model.Usage{TotalTokens: 800}),
	}}
	a := NewAgent(zerolog.New(&buf), mock, Options{
		MaxIterations: 4, ChatTimeout: time.Second, TotalTimeout: 30 * time.Second,
		LLM: defaultLLMConfig(), RunID: "01J9",
	})
	conf := config.Config{
		VM:         config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Prompt:     "task",
		Compaction: enabledCompaction(),
	}
	ctx := mphotel.WithRunIdentity(t.Context(), "baseline:01J9", "baseline")
	if err := a.Execute(ctx, conf, multipass.New(zerolog.Nop())); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if a.compactionsApplied == 0 {
		t.Fatal("no compaction applied; test setup no longer forces one")
	}

	summary := onlyEvent(t, decodeEvents(t, &buf), eventRunSummary)
	if got, _ := summary["reclaimed_tokens"].(float64); got <= 0 {
		t.Errorf("reclaimed_tokens = %v, want a positive total", summary["reclaimed_tokens"])
	}
	if got := summary["compactions"]; got != float64(a.compactionsApplied) {
		t.Errorf("compactions = %v, want %d", got, a.compactionsApplied)
	}
}

// Iterations are counted before the round runs, so a run that aborts mid-round
// still reports the attempt. Otherwise iterations and outcome="error" describe
// runs of different lengths.
func TestExecuteCountsAbortedIteration(t *testing.T) {
	var buf bytes.Buffer
	mock := &mockKronk{
		contextWidth: 32768,
		errs:         []error{errors.New("backend down")},
		responses:    []model.ChatResponse{chatResp("done", "", model.FinishReasonStop, nil, &model.Usage{TotalTokens: 120})},
	}
	a := NewAgent(zerolog.New(&buf), mock, Options{
		MaxIterations: 3, ChatTimeout: time.Second, TotalTimeout: 30 * time.Second,
		LLM: defaultLLMConfig(), RunID: "01J9",
	})
	conf := config.Config{VM: config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"}, Prompt: "task"}
	ctx := mphotel.WithRunIdentity(t.Context(), "baseline:01J9", "baseline")

	if err := a.Execute(ctx, conf, multipass.New(zerolog.Nop())); err == nil {
		t.Fatal("Execute succeeded against a failing backend; test setup is wrong")
	}

	summary := onlyEvent(t, decodeEvents(t, &buf), eventRunSummary)
	if got := summary["iterations"]; got != float64(1) {
		t.Errorf("iterations = %v, want 1 for the aborted round", got)
	}
	if got := summary["outcome"]; got != string(outcomeError) {
		t.Errorf("outcome = %v, want %q", got, outcomeError)
	}
}
