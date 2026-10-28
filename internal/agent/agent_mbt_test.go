package agent

import (
	"context"
	"testing"
	"time"

	"github.com/ardanlabs/kronk/sdk/kronk/model"
	"github.com/rs/zerolog"
	"pgregory.net/rapid"

	"github.com/trollLemon/MPHarness/internal/config"
	"github.com/trollLemon/MPHarness/internal/lxd"
)

type mbtScenario struct {
	maxIterations int
	window        int
	responses     []mbtResponse
}

type mbtResponse struct {
	content      string
	finishReason string
	toolCalls    []model.ResponseToolCall
	usage        *model.Usage
}

func genMBTScenario(t *rapid.T) mbtScenario {
	maxIter := rapid.IntRange(1, 10).Draw(t, "maxIterations")
	nResp := rapid.IntRange(1, 20).Draw(t, "numResponses")

	sc := mbtScenario{
		maxIterations: maxIter,
		window:        rapid.IntRange(4096, 65536).Draw(t, "window"),
		responses:     make([]mbtResponse, nResp),
	}
	for i := range sc.responses {
		sc.responses[i] = genMBTResponse(t)
	}
	return sc
}

func genMBTResponse(t *rapid.T) mbtResponse {
	fr := rapid.SampledFrom([]string{
		model.FinishReasonStop,
		model.FinishReasonTool,
		model.FinishReasonLength,
	}).Draw(t, "finishReason")

	var toolCalls []model.ResponseToolCall
	if fr == model.FinishReasonTool {
		n := rapid.IntRange(1, 2).Draw(t, "numToolCalls")
		toolCalls = make([]model.ResponseToolCall, n)
		for j := range toolCalls {
			name := rapid.SampledFrom([]string{"vm_exec", "vm_info", "task_status"}).Draw(t, "toolName")
			switch name {
			case "task_status":
				status := rapid.SampledFrom([]string{"Completed", "Failed", "Error"}).Draw(t, "taskStatus")
				toolCalls[j] = model.ResponseToolCall{
					ID:   "ts",
					Type: "function",
					Function: model.ResponseToolCallFunction{
						Name:      "task_status",
						Arguments: model.ToolCallArguments{"status": status, "reason": "mbt test"},
					},
				}
			default:
				toolCalls[j] = model.ResponseToolCall{
					ID:   "c1",
					Type: "function",
					Function: model.ResponseToolCallFunction{
						Name:      name,
						Arguments: model.ToolCallArguments{"command": "echo x"},
					},
				}
			}
		}
	}

	pt := rapid.IntRange(5, 1000).Draw(t, "promptTokens")
	ct := rapid.IntRange(5, 1000).Draw(t, "completionTokens")
	tt := rapid.IntRange(pt+ct, pt+ct+500).Draw(t, "totalTokens")

	return mbtResponse{
		content:      rapid.StringN(0, 20, 100).Draw(t, "content"),
		finishReason: fr,
		toolCalls:    toolCalls,
		usage:        &model.Usage{TotalTokens: tt, PromptTokens: pt, CompletionTokens: ct},
	}
}

func runMBT(t *rapid.T, sc mbtScenario) *Agent {
	responses := make([]model.ChatResponse, len(sc.responses))
	for i, r := range sc.responses {
		responses[i] = chatResp(r.content, "", r.finishReason, r.toolCalls, r.usage)
	}

	mock := &mockKronk{
		responses:    responses,
		contextWidth: sc.window,
	}

	cfg := config.Config{
		VM:         config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:      "m",
		Prompt:     "task",
		Compaction: config.CompactionConfig{Enabled: false},
	}

	agt := NewAgent(zerolog.Nop(), mock, Options{
		MaxIterations: sc.maxIterations,
		ChatTimeout:   time.Second,
		TotalTimeout:  30 * time.Second,
		LLM:           defaultLLMConfig(),
		RunID:         "mbt",
	})

	cli := lxd.NewWithServer(fakeServer{}, zerolog.Nop(), nil)
	_, _ = agt.Execute(context.Background(), cfg, cli)
	return agt
}

func TestMBTMaxIterationsInvariant(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sc := genMBTScenario(t)
		a := runMBT(t, sc)

		if a.iteration > a.maxIterations {
			t.Fatalf("iteration %d exceeds maxIterations %d", a.iteration, a.maxIterations)
		}
	})
}

func TestMBTToolCallAndToolErrorCounts(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sc := genMBTScenario(t)
		a := runMBT(t, sc)

		totalToolCalls := a.totals.ToolCalls
		totalFailures := a.totals.ToolFailures
		if totalFailures > totalToolCalls {
			t.Fatalf("tool failures %d > tool calls %d", totalFailures, totalToolCalls)
		}
	})
}

func TestMBTTerminatesAtMaxIterations(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		maxIter := rapid.IntRange(1, 8).Draw(t, "maxIterations")
		extra := rapid.IntRange(0, 5).Draw(t, "extraResponses")

		nResp := maxIter + extra
		resps := make([]mbtResponse, nResp)
		for i := range resps {
			resps[i] = mbtResponse{
				content:      "",
				finishReason: model.FinishReasonTool,
				toolCalls: []model.ResponseToolCall{{
					ID:   "c1",
					Type: "function",
					Function: model.ResponseToolCallFunction{
						Name:      "vm_exec",
						Arguments: model.ToolCallArguments{"command": "echo x"},
					},
				}},
				usage: &model.Usage{TotalTokens: 50, PromptTokens: 25, CompletionTokens: 25},
			}
		}

		sc := mbtScenario{
			maxIterations: maxIter,
			window:        16384,
			responses:     resps,
		}

		a := runMBT(t, sc)

		if a.iteration != a.maxIterations {
			t.Fatalf("iteration %d, want maxIterations %d", a.iteration, a.maxIterations)
		}
		if a.totals.Iterations != a.maxIterations {
			t.Fatalf("total iterations %d, want %d", a.totals.Iterations, a.maxIterations)
		}
	})
}

func TestMBTTerminatesOnTaskStatus(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		maxIter := rapid.IntRange(3, 10).Draw(t, "maxIterations")
		tsPosition := rapid.IntRange(0, maxIter-1).Draw(t, "tsPosition")

		resps := make([]mbtResponse, maxIter+2)
		for i := range resps {
			if i == tsPosition {
				status := rapid.SampledFrom([]string{"Completed", "Failed", "Error"}).Draw(t, "status")
				resps[i] = mbtResponse{
					content:      "summary",
					finishReason: model.FinishReasonTool,
					toolCalls: []model.ResponseToolCall{{
						ID:   "ts",
						Type: "function",
						Function: model.ResponseToolCallFunction{
							Name:      "task_status",
							Arguments: model.ToolCallArguments{"status": status, "reason": "mbt test"},
						},
					}},
					usage: &model.Usage{TotalTokens: 50, PromptTokens: 25, CompletionTokens: 25},
				}
			} else {
				resps[i] = mbtResponse{
					content:      "",
					finishReason: model.FinishReasonTool,
					toolCalls: []model.ResponseToolCall{{
						ID:   "c1",
						Type: "function",
						Function: model.ResponseToolCallFunction{
							Name:      "vm_exec",
							Arguments: model.ToolCallArguments{"command": "echo x"},
						},
					}},
					usage: &model.Usage{TotalTokens: 50, PromptTokens: 25, CompletionTokens: 25},
				}
			}
		}

		sc := mbtScenario{
			maxIterations: maxIter,
			window:        16384,
			responses:     resps,
		}

		a := runMBT(t, sc)

		// The agent runs iterations 0..tsPosition inclusive, then the loop
		// post-statement increments iteration to tsPosition+1.
		wantIter := tsPosition + 1
		if a.iteration != wantIter {
			t.Fatalf("iteration %d, want %d (stop at tsPosition %d)", a.iteration, wantIter, tsPosition)
		}
		if !a.done {
			t.Errorf("agent must be done after task_status")
		}
		if a.finalRunStatus.CompletedStatus == "" {
			t.Errorf("finalRunStatus must be set after task_status")
		}
	})
}

func TestMBTTracking(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sc := genMBTScenario(t)
		a := runMBT(t, sc)

		consumed := a.totals.Iterations
		if consumed == 0 {
			return
		}
		if consumed > len(sc.responses) {
			consumed = len(sc.responses)
		}

		var wantPrompt, wantCompletion int64
		for i := 0; i < consumed; i++ {
			if u := sc.responses[i].usage; u != nil {
				wantPrompt += int64(u.PromptTokens)
				wantCompletion += int64(u.CompletionTokens)
			}
		}

		if a.totals.PromptTokens != wantPrompt {
			t.Errorf("prompt tokens %d, want %d", a.totals.PromptTokens, wantPrompt)
		}
		if a.totals.CompletionTokens != wantCompletion {
			t.Errorf("completion tokens %d, want %d", a.totals.CompletionTokens, wantCompletion)
		}
	})
}
