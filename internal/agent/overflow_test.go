package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ardanlabs/kronk/sdk/kronk/model"
	"github.com/rs/zerolog"

	"github.com/trollLemon/MPHarness/internal/config"
	"github.com/trollLemon/MPHarness/internal/output"
)

func overflowErr(prompt, window int) error {
	return fmt.Errorf("kronk chat iteration 1: %w: input tokens [%d] exceed context window [%d]",
		model.ErrInvalidRequest, prompt, window)
}

func TestContextOverflow(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"kronk overflow", overflowErr(9000, 8192), true},
		{"unwrapped overflow", fmt.Errorf("input tokens [9000] exceed context window [8192]"), false},
		{"other invalid request", fmt.Errorf("%w: messages[0].content[0]: file inputs are not currently supported", model.ErrInvalidRequest), false},
		{"unrelated error", errors.New("connection reset"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			overflow, ok := asContextOverflow(tt.err)
			if ok != tt.want {
				t.Fatalf("asContextOverflow(%v) = %v, want %v", tt.err, ok, tt.want)
			}
			if ok && !errors.Is(overflow, errContextOverflow) {
				t.Fatalf("overflow error does not match the sentinel: %v", overflow)
			}
		})
	}
}

func TestContextOverflowDetails(t *testing.T) {
	overflow, ok := asContextOverflow(overflowErr(11376, 8192))
	if !ok {
		t.Fatal("overflow error was not parsed")
	}
	if overflow.prompt != 11376 || overflow.window != 8192 {
		t.Fatalf("got prompt=%d window=%d, want 11376/8192", overflow.prompt, overflow.window)
	}
	if !errors.Is(overflow, model.ErrInvalidRequest) {
		t.Fatal("typed overflow must preserve the kronk cause")
	}
	if _, ok := asContextOverflow(errors.New("other")); ok {
		t.Fatal("unrelated error must not parse")
	}
}

func TestBuildContextOverflowContent(t *testing.T) {
	tests := []struct {
		name     string
		tool     string
		captures bool
		wantSubs []string
		reject   []string
	}{
		{
			name: "vm_exec with captures", tool: "vm_exec", captures: true,
			wantSubs: []string{`"status":"FAILED"`, "9351", "4096", "capture: true", "output_search", "output_read"},
		},
		{
			name: "vm_exec without captures", tool: "vm_exec", captures: false,
			wantSubs: []string{`"status":"FAILED"`, "9351", "4096", "narrower"},
			reject:   []string{"capture: true", "output_search"},
		},
		{
			name: "output_read narrows the read", tool: "output_read", captures: true,
			wantSubs: []string{`"status":"FAILED"`, "fewer lines", "offset", "limit"},
		},
		{
			name: "unknown tool narrows", tool: "vm_info", captures: true,
			wantSubs: []string{`"status":"FAILED"`, "Narrow the request"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildContextOverflowContent(tt.tool, 9351, 4096, tt.captures)
			for _, want := range tt.wantSubs {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in %q", want, got)
				}
			}
			for _, reject := range tt.reject {
				if strings.Contains(got, reject) {
					t.Errorf("unexpected %q in %q", reject, got)
				}
			}
			if len(got) > 400 {
				t.Errorf("replacement is %d bytes, want a brief message", len(got))
			}
			var envelope struct {
				Status string `json:"status"`
				Data   struct {
					Error string `json:"error"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(got), &envelope); err != nil {
				t.Fatalf("replacement is not JSON: %v", err)
			}
			if envelope.Status != "FAILED" || envelope.Data.Error == "" {
				t.Fatalf("replacement is not a tool error envelope: %+v", envelope)
			}
		})
	}
}

func TestDiscardLargestToolResultPicksLargestFirst(t *testing.T) {
	agt := &Agent{
		toolResultBudget: 1024,
		conversation: []model.D{
			{"role": "system", "content": "sys"},
			{"role": "tool", "name": "vm_exec", "content": strings.Repeat("a", 5000)},
			{"role": "tool", "name": "output_read", "content": strings.Repeat("b", 8000)},
		},
	}

	tool, size, reclaimed, ok := agt.discardLargestToolResult()
	if !ok || tool != "output_read" || size != 8000 || reclaimed <= 0 {
		t.Fatalf("first discard = %q %d %d %v, want output_read/8000/reclaimed", tool, size, reclaimed, ok)
	}
	if got := agt.conversation[2]["content"].(string); !strings.Contains(got, "fewer lines") {
		t.Fatalf("output_read replacement lost its hint: %q", got)
	}
	if got := agt.conversation[1]["content"].(string); got != strings.Repeat("a", 5000) {
		t.Fatal("the smaller result must not be touched first")
	}

	tool, size, _, ok = agt.discardLargestToolResult()
	if !ok || tool != "vm_exec" || size != 5000 {
		t.Fatalf("second discard = %q %d %v, want vm_exec/5000", tool, size, ok)
	}

	if _, _, _, ok := agt.discardLargestToolResult(); ok {
		t.Fatal("nothing worth reclaiming should remain")
	}
}

func TestRunIterationRecoversFromContextOverflow(t *testing.T) {
	var buf bytes.Buffer
	cfg := config.Config{
		VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:  "m",
		Prompt: "task",
	}
	agt := NewAgent(zerolog.New(&buf), &mockKronk{contextWidth: 8192}, Options{
		MaxIterations: 2,
		ChatTimeout:   time.Second,
		TotalTimeout:  5 * time.Second,
		LLM:           defaultLLMConfig(),
		RunID:         "run-overflow",
	})
	agt.toolDocs = buildToolDocuments(cfg)
	agt.toolResultBudget = 1024
	agt.conversation = append(buildInitialConversation(cfg),
		model.D{"role": "assistant", "tool_calls": []model.D{{
			"id": "c1", "type": "function",
			"function": model.D{"name": "vm_exec", "arguments": `{"command":"ls"}`},
		}}},
		model.D{"role": "tool", "tool_call_id": "c1", "name": "vm_exec", "content": strings.Repeat("a", 8000)},
	)

	var requests []model.D
	calls := 0
	agt.krn = &mockKronk{contextWidth: 8192, chatFunc: func(_ context.Context, req model.D) (model.ChatResponse, error) {
		requests = append(requests, req)
		calls++
		if calls == 1 {
			return model.ChatResponse{}, overflowErr(9000, 8192)
		}
		return chatResp("done", "", model.FinishReasonStop, nil, &model.Usage{TotalTokens: 100}), nil
	}}

	if err := agt.runIteration(context.Background(), newTestClient(t), cfg); err != nil {
		t.Fatalf("runIteration: %v", err)
	}
	if calls != 2 {
		t.Fatalf("chat calls = %d, want 2 (overflow then retry)", calls)
	}
	if agt.done {
		t.Fatal("a finish without task_status must not end the run before the nudge")
	}

	second, _ := json.Marshal(requests[1])
	if strings.Contains(string(second), strings.Repeat("a", 100)) {
		t.Fatal("the oversized result survived into the retry request")
	}
	if !strings.Contains(string(second), "was discarded") {
		t.Fatalf("retry request is missing the replacement error: %s", second)
	}

	event := onlyEvent(t, decodeEvents(t, &buf), eventContextOverflow)
	if event["outcome"] != "recovered" {
		t.Errorf("outcome = %v, want recovered", event["outcome"])
	}
	if event["replaced"] != float64(1) {
		t.Errorf("replaced = %v, want 1", event["replaced"])
	}
	if event["prompt_tokens"] != float64(9000) || event["ctx_window"] != float64(8192) {
		t.Errorf("overflow measurements = %v/%v, want 9000/8192", event["prompt_tokens"], event["ctx_window"])
	}
}

func TestRunIterationDiscardsUntilTheRequestFits(t *testing.T) {
	cfg := config.Config{
		VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:  "m",
		Prompt: "task",
	}
	agt := NewAgent(zerolog.Nop(), &mockKronk{contextWidth: 8192}, Options{
		MaxIterations: 2,
		ChatTimeout:   time.Second,
		TotalTimeout:  5 * time.Second,
		LLM:           defaultLLMConfig(),
		RunID:         "run-overflow-twice",
	})
	agt.toolDocs = buildToolDocuments(cfg)
	agt.toolResultBudget = 1024
	agt.conversation = append(buildInitialConversation(cfg),
		model.D{"role": "tool", "tool_call_id": "c1", "name": "vm_exec", "content": strings.Repeat("a", 8000)},
		model.D{"role": "tool", "tool_call_id": "c2", "name": "vm_exec", "content": strings.Repeat("b", 6000)},
	)

	requests := make([]model.D, 0, 3)
	calls := 0
	agt.krn = &mockKronk{contextWidth: 8192, chatFunc: func(_ context.Context, req model.D) (model.ChatResponse, error) {
		requests = append(requests, req)
		calls++
		if calls < 3 {
			return model.ChatResponse{}, overflowErr(9000-calls*1000, 8192)
		}
		return chatResp("done", "", model.FinishReasonStop, nil, nil), nil
	}}

	if err := agt.runIteration(context.Background(), newTestClient(t), cfg); err != nil {
		t.Fatalf("runIteration: %v", err)
	}
	if calls != 3 {
		t.Fatalf("chat calls = %d, want 3", calls)
	}
	last, _ := json.Marshal(requests[2])
	if strings.Contains(string(last), strings.Repeat("a", 100)) || strings.Contains(string(last), strings.Repeat("b", 100)) {
		t.Fatal("both oversized results must be gone by the third request")
	}
}

func TestRunIterationReportsExhaustedOverflow(t *testing.T) {
	var buf bytes.Buffer
	cfg := config.Config{
		VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:  "m",
		Prompt: "task",
	}
	agt := NewAgent(zerolog.New(&buf), &mockKronk{contextWidth: 8192}, Options{
		MaxIterations: 2,
		ChatTimeout:   time.Second,
		TotalTimeout:  5 * time.Second,
		LLM:           defaultLLMConfig(),
		RunID:         "run-overflow-exhausted",
	})
	agt.toolDocs = buildToolDocuments(cfg)
	agt.toolResultBudget = 1024
	agt.conversation = append(buildInitialConversation(cfg),
		model.D{"role": "tool", "tool_call_id": "c1", "name": "vm_exec", "content": "small"},
	)
	agt.krn = &mockKronk{contextWidth: 8192, chatFunc: func(context.Context, model.D) (model.ChatResponse, error) {
		return model.ChatResponse{}, overflowErr(9000, 8192)
	}}

	err := agt.runIteration(context.Background(), newTestClient(t), cfg)
	if err == nil {
		t.Fatal("an unrecoverable overflow must fail the iteration")
	}
	if !errors.Is(err, model.ErrInvalidRequest) {
		t.Errorf("error must preserve the kronk cause: %v", err)
	}
	if !errors.Is(err, errContextOverflow) {
		t.Errorf("error must match the overflow sentinel: %v", err)
	}
	if !strings.Contains(err.Error(), "context overflow") {
		t.Errorf("error must name the repair attempt: %v", err)
	}

	event := onlyEvent(t, decodeEvents(t, &buf), eventContextOverflow)
	if event["outcome"] != "exhausted" {
		t.Errorf("outcome = %v, want exhausted", event["outcome"])
	}
	if event["replaced"] != float64(0) {
		t.Errorf("replaced = %v, want 0", event["replaced"])
	}
}

func TestExecuteToolCallsRejectsOversizedVMCExecOutput(t *testing.T) {
	tests := []struct {
		name          string
		captures      bool
		output        string
		exit          int
		wantOutputs   int
		wantSubs      []string
		reject        []string
		wantFailures  int
		wantSucceeded bool
		wantTooLarge  bool
	}{
		{
			name: "captures available", captures: true, output: strings.Repeat("x", 5000), wantOutputs: 1,
			wantSubs:     []string{`"status":"FAILED"`, "capture: true"},
			reject:       []string{strings.Repeat("x", 100)},
			wantFailures: 1, wantTooLarge: true,
		},
		{
			name: "captures unavailable", captures: false, output: strings.Repeat("x", 5000), wantOutputs: 1,
			wantSubs:     []string{`"status":"FAILED"`, "narrower"},
			reject:       []string{strings.Repeat("x", 100), "capture: true"},
			wantFailures: 1, wantTooLarge: true,
		},
		{
			name: "under budget stays inline", captures: true, output: strings.Repeat("x", 500), wantOutputs: 1,
			wantSubs:      []string{`"status":"SUCCESS"`, strings.Repeat("x", 100)},
			wantSucceeded: true,
		},
		{
			name: "failed command with large output", captures: true, output: strings.Repeat("x", 5000), exit: 1,
			wantSubs:     []string{`"status":"FAILED"`, "was discarded", "capture: true"},
			reject:       []string{strings.Repeat("x", 100)},
			wantFailures: 1, wantTooLarge: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			cfg := config.Config{
				VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
				Model:  "m",
				Prompt: "task",
				Output: config.OutputConfig{Enabled: tt.captures, Mode: "auto"},
			}
			agt := NewAgent(zerolog.New(&buf), &mockKronk{}, Options{
				MaxIterations: 2,
				ChatTimeout:   time.Second,
				TotalTimeout:  5 * time.Second,
				LLM:           defaultLLMConfig(),
				RunID:         "run-reject",
			})
			agt.toolResultBudget = 1024
			cli := newScriptedTestClient(t, func(command string) (string, int) {
				switch {
				case strings.Contains(command, "cloud-init"):
					return "", 0
				case strings.HasPrefix(command, "id -u"), strings.HasPrefix(command, "id -g"):
					return "1000\n", 0
				case strings.HasPrefix(command, "getent passwd"):
					return "ubuntu:x:1000:1000:Ubuntu:/home/ubuntu:/bin/bash\n", 0
				case command == "ls":
					return tt.output, tt.exit
				default:
					return "", 0
				}
			})
			if tt.captures {
				agt.store = output.NewStore(cli, cfg.VM.Name, "/tmp/mph-output/run-reject", cfg.Output, int64(agt.toolResultBudget))
			}

			calls := []model.ResponseToolCall{{
				ID: "c1", Type: "function",
				Function: model.ResponseToolCallFunction{
					Name: "vm_exec", Arguments: model.ToolCallArguments{"command": "ls"},
				},
			}}
			responses, outputs := agt.executeToolCalls(context.Background(), cli, cfg, calls)
			if len(responses) != 1 {
				t.Fatalf("responses = %d, want 1", len(responses))
			}
			if len(outputs) != tt.wantOutputs {
				t.Fatalf("command outputs = %d, want %d", len(outputs), tt.wantOutputs)
			}
			if agt.totals.ToolFailures != tt.wantFailures {
				t.Errorf("tool failures = %d, want %d", agt.totals.ToolFailures, tt.wantFailures)
			}
			content, _ := responses[0]["content"].(string)
			for _, want := range tt.wantSubs {
				if !strings.Contains(content, want) {
					t.Errorf("missing %q in %q", want, content)
				}
			}
			for _, reject := range tt.reject {
				if strings.Contains(content, reject) {
					t.Errorf("unexpected payload in %q", content)
				}
			}

			events := decodeEvents(t, &buf)
			succeeded := eventsOf(events, eventToolSucceeded)
			if got := len(succeeded) == 1; got != tt.wantSucceeded {
				t.Errorf("tool_succeeded events = %d, want succeeded=%v", len(succeeded), tt.wantSucceeded)
			}
			if failed := eventsOf(events, eventToolFailed); len(failed) != tt.wantFailures {
				t.Errorf("tool_failed events = %d, want %d", len(failed), tt.wantFailures)
			}
			rejections := eventsOf(events, eventToolOutputTooLarge)
			if !tt.wantTooLarge {
				if len(rejections) != 0 {
					t.Errorf("under-budget output must not be rejected: %v", rejections)
				}
			} else {
				event := onlyEvent(t, events, eventToolOutputTooLarge)
				if event["bytes"].(float64) <= 1024 || event["budget"] != float64(1024) {
					t.Errorf("size event = %v, want bytes>1024 budget=1024", event)
				}
			}
		})
	}
}
