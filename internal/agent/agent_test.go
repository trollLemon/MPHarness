package agent_test

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
	"pgregory.net/rapid"

	"github.com/trollLemon/MPHarness/internal/agent"
	"github.com/trollLemon/MPHarness/internal/config"
	"github.com/trollLemon/MPHarness/internal/textutil"
)

type loopScenario struct {
	maxIterations int
	window        int
	responses     []loopResponse
	compaction    config.CompactionConfig
	truncation    config.TruncationConfig
	agent         config.AgentConfig
	output        config.OutputConfig
	allowed       map[string]bool
	prompt        string
	contentDir    string
	vmName        string
	llm           config.LLMConfig
	scripted      func(command string) (string, int)
	chatFunc      func(context.Context, model.D) (model.ChatResponse, error)
	handover      func(context.Context, model.D) (model.ChatResponse, error)
	failHandover  bool
	overflowAt    int
	overflowErr   error
	tokenizeErr   error
	tokenizeZero  bool
	enforceWindow bool
}

type loopResponse struct {
	content      string
	reasoning    string
	finishReason string
	toolCalls    []model.ResponseToolCall
	usage        *model.Usage
	delta        bool
	noChoices    bool
	nilMessage   bool
}

type loopRun struct {
	status   *agent.TaskStatusOutcome
	err      error
	summary  map[string]any
	events   []map[string]any
	mock     *mockKronk
	enforcer *windowEnforcingKronk
}

const loopConfigYAML = `vm:
  name: vm
  cpu: 1
  ram: 1G
  disk: 5G
model: m
prompt: task
`

// validateConversation checks the protocol invariant every request must hold:
// each assistant tool call has exactly one matching tool result, in order, with
// the same id and tool name. This is independent of how the agent chooses which
// calls to run.
func validateConversation(t testFatal, req model.D) {
	msgs, _ := req["messages"].([]model.D)
	if len(msgs) < 2 {
		t.Fatalf("request has %d messages, want at least system and user", len(msgs))
	}
	if msgs[0]["role"] != "system" {
		t.Fatalf("first message role = %v, want system", msgs[0]["role"])
	}
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		switch m["role"] {
		case "assistant":
			calls, _ := m["tool_calls"].([]model.D)
			if len(calls) == 0 {
				continue
			}
			for j, call := range calls {
				idx := i + 1 + j
				if idx >= len(msgs) {
					t.Fatalf("assistant tool call %d has no tool result: %v", j, calls)
				}
				result := msgs[idx]
				if result["role"] != "tool" {
					t.Fatalf("message after tool call %d has role %v, want tool", j, result["role"])
				}
				if result["tool_call_id"] != call["id"] {
					t.Fatalf("tool result id = %v, want %v", result["tool_call_id"], call["id"])
				}
				fn, _ := call["function"].(model.D)
				if result["name"] != fn["name"] {
					t.Fatalf("tool result name = %v, want %v", result["name"], fn["name"])
				}
			}
			i += len(calls)
		case "tool":
			t.Fatalf("tool result at message %d has no preceding assistant tool call", i)
		}
	}
}

func runScenario(t testFatal, sc loopScenario) loopRun {
	responses := make([]model.ChatResponse, len(sc.responses))
	for i, r := range sc.responses {
		switch {
		case r.noChoices:
			responses[i] = model.ChatResponse{}
		case r.nilMessage:
			responses[i] = model.ChatResponse{Choices: []model.Choice{{}}}
		case r.delta:
			responses[i] = deltaResp(r.content, r.reasoning, r.finishReason)
		default:
			responses[i] = chatResp(r.content, r.reasoning, r.finishReason, r.toolCalls, r.usage)
		}
	}

	mock := &mockKronk{
		responses:    responses,
		contextWidth: sc.window,
		failHandover: sc.failHandover,
		handover:     sc.handover,
		chatFunc:     sc.chatFunc,
		overflowAt:   sc.overflowAt,
		overflowErr:  sc.overflowErr,
		tokenizeErr:  sc.tokenizeErr,
		tokenizeZero: sc.tokenizeZero,
	}

	var krn agent.Kronk = mock
	var enforcer *windowEnforcingKronk
	if sc.enforceWindow {
		enforcer = &windowEnforcingKronk{mockKronk: mock}
		krn = enforcer
	}

	var buf bytes.Buffer
	agt := agent.NewAgent(zerolog.New(&buf), krn, agent.Options{
		MaxIterations: sc.maxIterations,
		ChatTimeout:   time.Second,
		TotalTimeout:  30 * time.Second,
		LLM:           sc.llm,
		RunID:         "loop",
	})

	base, err := config.Parse([]byte(loopConfigYAML))
	if err != nil {
		t.Fatalf("parse base config: %v", err)
	}
	vmName := sc.vmName
	if vmName == "" {
		vmName = "vm"
	}
	prompt := sc.prompt
	if prompt == "" {
		prompt = "task"
	}
	conf := base
	conf.VM = config.VMConfig{Name: vmName, CPU: 1, RAM: "1G", Disk: "5G"}
	conf.Prompt = prompt
	conf.ContentDir = sc.contentDir
	conf.AllowedCommands = sc.allowed
	conf.Compaction = sc.compaction
	conf.Agent = sc.agent
	conf.Output = sc.output
	conf.LLM = sc.llm
	if sc.truncation != (config.TruncationConfig{}) {
		conf.Truncation = sc.truncation
	}

	scripted := sc.scripted
	if scripted == nil {
		scripted = identityScripted
	}
	cli := newScriptedTestClient(t, scripted)

	status, err := agt.Execute(context.Background(), conf, cli)
	for _, req := range mock.captured {
		if isCompactionRequest(req) {
			continue
		}
		validateConversation(t, req)
	}
	events := decodeEvents(t, &buf)
	return loopRun{
		status:   status,
		err:      err,
		summary:  onlyEvent(t, events, eventRunSummary),
		events:   events,
		mock:     mock,
		enforcer: enforcer,
	}
}

func genLoopScenario(t *rapid.T) loopScenario {
	maxIter := rapid.IntRange(1, 10).Draw(t, "maxIterations")
	nResp := rapid.IntRange(1, 20).Draw(t, "numResponses")

	sc := loopScenario{
		maxIterations: maxIter,
		window:        rapid.IntRange(0, 65536).Draw(t, "window"),
		responses:     make([]loopResponse, nResp),
		llm:           defaultLLMConfig(),
	}
	for i := range sc.responses {
		sc.responses[i] = genLoopResponse(t)
	}
	return sc
}

func genLoopResponse(t *rapid.T) loopResponse {
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
				status := rapid.SampledFrom([]string{agent.TaskStatusCompleted, agent.TaskStatusFailed, agent.TaskStatusError}).Draw(t, "taskStatus")
				toolCalls[j] = taskStatusCall("ts", status, "scenario test")
			case "vm_info":
				toolCalls[j] = vmInfoCall("c1")
			default:
				toolCalls[j] = vmExecCall("c1", "echo x")
			}
		}
	}

	pt := rapid.IntRange(5, 1000).Draw(t, "promptTokens")
	ct := rapid.IntRange(5, 1000).Draw(t, "completionTokens")
	tt := rapid.IntRange(pt+ct, pt+ct+500).Draw(t, "totalTokens")

	return loopResponse{
		content:      rapid.StringN(0, 20, 100).Draw(t, "content"),
		finishReason: fr,
		toolCalls:    toolCalls,
		usage:        &model.Usage{TotalTokens: tt, PromptTokens: pt, CompletionTokens: ct},
	}
}

func vmExecCall(id, command string) model.ResponseToolCall {
	return model.ResponseToolCall{ID: id, Type: "function", Function: model.ResponseToolCallFunction{
		Name: "vm_exec", Arguments: model.ToolCallArguments{"command": command},
	}}
}

func vmInfoCall(id string) model.ResponseToolCall {
	return model.ResponseToolCall{ID: id, Type: "function", Function: model.ResponseToolCallFunction{
		Name: "vm_info", Arguments: model.ToolCallArguments{},
	}}
}

func taskStatusCall(id, status, reason string) model.ResponseToolCall {
	return model.ResponseToolCall{ID: id, Type: "function", Function: model.ResponseToolCallFunction{
		Name: "task_status", Arguments: model.ToolCallArguments{"status": status, "reason": reason},
	}}
}

func totalUsage(total int) *model.Usage {
	return &model.Usage{TotalTokens: total, PromptTokens: total}
}

func overflowErr(prompt, window int) error {
	return fmt.Errorf("kronk chat iteration 1: %w: input tokens [%d] exceed context window [%d]",
		model.ErrInvalidRequest, prompt, window)
}

type windowEnforcingKronk struct {
	*mockKronk
	overWindow int
}

func promptTokens(req model.D) int {
	n := 0
	if msgs, ok := req["messages"].([]model.D); ok {
		for _, m := range msgs {
			if c, _ := m["content"].(string); c != "" {
				n += len(c) / testBytesPerToken
			}
		}
	}
	if tools, ok := req["tools"].([]model.D); ok {
		for _, d := range tools {
			b, _ := json.Marshal(d)
			n += len(b) / testBytesPerToken
		}
	}
	return n
}

func (w *windowEnforcingKronk) Chat(ctx context.Context, req model.D) (model.ChatResponse, error) {
	n := promptTokens(req)
	if n > w.contextWidth {
		if !isCompactionRequest(req) {
			w.overWindow++
		}
		return model.ChatResponse{}, overflowErr(n, w.contextWidth)
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

func sumUsages(usages []*model.Usage) (prompt, completion int64) {
	for _, u := range usages {
		if u == nil {
			continue
		}
		prompt += int64(u.PromptTokens)
		completion += int64(u.CompletionTokens)
	}
	return
}

func TestAgentLoopInitialPrompt(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		prompt := rapid.StringN(1, 40, 100).Draw(t, "prompt")
		contentDir := rapid.SampledFrom([]string{"", "/tmp/content"}).Draw(t, "contentDir")
		allowed := rapid.SampledFrom([]map[string]bool{nil, {"ls": true, "cat": true}}).Draw(t, "allowed")
		outputEnabled := rapid.Bool().Draw(t, "outputEnabled")

		sc := loopScenario{
			maxIterations: 1,
			window:        8192,
			vmName:        "secret-vm",
			prompt:        prompt,
			contentDir:    contentDir,
			allowed:       allowed,
			output:        config.OutputConfig{Enabled: outputEnabled},
			responses: []loopResponse{{
				finishReason: model.FinishReasonStop,
				usage:        totalUsage(10),
			}},
			llm: defaultLLMConfig(),
		}
		run := runScenario(t, sc)

		req := run.mock.captured[0]
		msgs, _ := req["messages"].([]model.D)
		if len(msgs) < 2 {
			t.Fatalf("initial request has %d messages, want at least 2", len(msgs))
		}
		if msgs[0]["role"] != "system" || msgs[1]["role"] != "user" {
			t.Fatalf("initial roles = %v/%v, want system/user", msgs[0]["role"], msgs[1]["role"])
		}
		system, _ := msgs[0]["content"].(string)
		user, _ := msgs[1]["content"].(string)
		if !strings.Contains(user, strings.TrimSpace(prompt)) {
			t.Errorf("user prompt missing task %q: %q", prompt, user)
		}
		if !strings.Contains(user, "Allowed commands:") {
			t.Errorf("user prompt missing allowed-commands header: %q", user)
		}
		if len(allowed) > 0 {
			for name := range allowed {
				if !strings.Contains(user, name) {
					t.Errorf("user prompt missing allowed command %q: %q", name, user)
				}
			}
		} else if !strings.Contains(user, "(all)") {
			t.Errorf("user prompt must allow all commands: %q", user)
		}
		if contentDir != "" && !strings.Contains(system, "Additional Content") {
			t.Errorf("system prompt missing content-dir suffix: %q", system)
		}
		for _, leaked := range []string{"secret-vm", "16G", "100G", "noble", "VM Configuration"} {
			if strings.Contains(system, leaked) {
				t.Errorf("system prompt leaked VM detail %q: %q", leaked, system)
			}
		}
		tools, _ := req["tools"].([]model.D)
		names := map[string]bool{}
		for _, d := range tools {
			fn, _ := d["function"].(model.D)
			name, _ := fn["name"].(string)
			names[name] = true
		}
		for _, want := range []string{"vm_exec", "vm_info", "task_status"} {
			if !names[want] {
				t.Errorf("missing tool %q in %v", want, names)
			}
		}
		for _, outputTool := range []string{"output_search", "output_read"} {
			if names[outputTool] != outputEnabled {
				t.Errorf("tool %q present=%v, want %v", outputTool, names[outputTool], outputEnabled)
			}
		}
	})
}

func TestAgentLoopRequestShape(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		temp := rapid.SampledFrom([]float64{0, 0.7, 0.8}).Draw(t, "temperature")
		topP := rapid.Float64Range(0, 1).Draw(t, "topP")
		topK := rapid.IntRange(0, 40).Draw(t, "topK")
		maxTokens := rapid.IntRange(0, 4096).Draw(t, "maxOutputTokens")

		sc := loopScenario{
			maxIterations: 3,
			window:        rapid.IntRange(0, 65536).Draw(t, "window"),
			responses: []loopResponse{
				{finishReason: model.FinishReasonTool, toolCalls: []model.ResponseToolCall{vmExecCall("c1", "echo x")}, usage: totalUsage(100)},
				{finishReason: model.FinishReasonTool, toolCalls: []model.ResponseToolCall{taskStatusCall("ts", agent.TaskStatusCompleted, "done")}, usage: totalUsage(100)},
			},
			llm: config.LLMConfig{Temperature: temp, TopP: topP, TopK: topK, MaxOutputTokens: maxTokens},
		}
		run := runScenario(t, sc)
		if run.err != nil {
			t.Fatalf("Execute: %v", run.err)
		}

		wantTopK := any(nil)
		switch {
		case topK > 0:
			wantTopK = topK
		case temp == 0:
			wantTopK = 1
		}
		wantMaxTokens := maxTokens
		if wantMaxTokens == 0 {
			wantMaxTokens = config.DefaultLLMMaxOutputTokens
		}

		agentRequests := 0
		for _, req := range run.mock.captured {
			if isCompactionRequest(req) {
				continue
			}
			agentRequests++
			if _, ok := req["tool_choice"]; ok {
				t.Error("tool_choice must not be sent: the model decides when to call tools")
			}
			if req["parallel_tool_calls"] != true {
				t.Errorf("parallel_tool_calls = %v, want true", req["parallel_tool_calls"])
			}
			if req["reasoning_effort"] != model.ReasoningEffortMedium {
				t.Errorf("reasoning_effort = %v, want medium", req["reasoning_effort"])
			}
			if req["temperature"] != temp {
				t.Errorf("temperature = %v, want %v", req["temperature"], temp)
			}
			if req["top_p"] != topP {
				t.Errorf("top_p = %v, want %v", req["top_p"], topP)
			}
			if req["max_tokens"] != wantMaxTokens {
				t.Errorf("max_tokens = %v, want %d", req["max_tokens"], wantMaxTokens)
			}
			gotTopK, hasTopK := req["top_k"]
			if wantTopK == nil {
				if hasTopK {
					t.Errorf("top_k = %v, want omitted for sampling without top_k", gotTopK)
				}
			} else if gotTopK != wantTopK {
				t.Errorf("top_k = %v, want %v", gotTopK, wantTopK)
			}
			msgs, _ := req["messages"].([]model.D)
			if len(msgs) < 2 || msgs[0]["role"] != "system" {
				t.Errorf("request messages must start with system and user: %v", msgs)
			}
			tools, _ := req["tools"].([]model.D)
			if len(tools) == 0 {
				t.Error("request has no tool documents")
			}
		}
		if agentRequests != 2 {
			t.Fatalf("agent requests = %d, want 2", agentRequests)
		}
	})
}

func TestAgentLoopNudge(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		length := rapid.Bool().Draw(t, "lengthNudge")
		delta := rapid.Bool().Draw(t, "delta")
		content := rapid.StringN(0, 200, 800).Draw(t, "content")
		if rapid.Bool().Draw(t, "longContent") {
			content = strings.Repeat("c", config.DefaultNudgeBytes+rapid.IntRange(1, 500).Draw(t, "contentExtra"))
		}
		reasoning := rapid.StringN(0, 200, 800).Draw(t, "reasoning")
		if rapid.Bool().Draw(t, "longReasoning") {
			reasoning = strings.Repeat("r", config.DefaultNudgeBytes+rapid.IntRange(1, 500).Draw(t, "reasoningExtra"))
		}

		finish := model.FinishReasonStop
		if length {
			finish = model.FinishReasonLength
		}
		sc := loopScenario{
			maxIterations: 3,
			window:        8192,
			responses: []loopResponse{
				{content: content, reasoning: reasoning, finishReason: finish, usage: totalUsage(100), delta: delta},
				{finishReason: model.FinishReasonTool, toolCalls: []model.ResponseToolCall{taskStatusCall("ts", agent.TaskStatusCompleted, "done")}, usage: totalUsage(100)},
			},
			llm: defaultLLMConfig(),
		}
		run := runScenario(t, sc)
		if run.err != nil {
			t.Fatalf("Execute: %v", run.err)
		}
		if run.status == nil || run.status.CompletedStatus != agent.TaskStatusCompleted {
			t.Fatalf("status = %+v, want Completed", run.status)
		}

		req := run.mock.captured[1]
		msgs, _ := req["messages"].([]model.D)
		last := msgs[len(msgs)-1]
		if last["role"] != "user" {
			t.Fatalf("last role = %v, want user", last["role"])
		}
		nudge, _ := last["content"].(string)
		if length {
			if !strings.HasPrefix(nudge, "Your previous response hit the token limit") {
				t.Errorf("length nudge = %q", nudge)
			}
		} else if !strings.Contains(nudge, "task_status") {
			t.Errorf("task_status nudge = %q", nudge)
		}

		hasAssistant := false
		for _, m := range msgs[:len(msgs)-1] {
			if m["role"] != "assistant" {
				continue
			}
			hasAssistant = true
			if content != "" {
				got, _ := m["content"].(string)
				if want := textutil.Truncate(content, config.DefaultNudgeBytes); got != want {
					t.Errorf("assistant content = %q, want %q", got, want)
				}
				if len(content) > config.DefaultNudgeBytes && !strings.Contains(got, "…(truncated") {
					t.Errorf("long content was not truncated: %q", got)
				}
			}
			if reasoning != "" {
				got, _ := m["reasoning_content"].(string)
				if want := textutil.Truncate(reasoning, config.DefaultNudgeBytes); got != want {
					t.Errorf("assistant reasoning = %q, want %q", got, want)
				}
				if len(reasoning) > config.DefaultNudgeBytes && !strings.Contains(got, "…(truncated") {
					t.Errorf("long reasoning was not truncated: %q", got)
				}
			}
		}
		if (content != "" || reasoning != "") && !hasAssistant {
			t.Error("nudge must replay the assistant turn when it carried text")
		}
	})
}

func TestAgentLoopNoChoicesEndsRun(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		kind := rapid.SampledFrom([]string{"no_choices", "nil_message"}).Draw(t, "malformedResponse")
		resp := loopResponse{}
		if kind == "no_choices" {
			resp.noChoices = true
		} else {
			resp.nilMessage = true
		}
		sc := loopScenario{
			maxIterations: rapid.IntRange(2, 5).Draw(t, "maxIterations"),
			window:        8192,
			responses:     []loopResponse{resp},
			llm:           defaultLLMConfig(),
		}
		run := runScenario(t, sc)
		if run.err != nil {
			t.Fatalf("Execute: %v", run.err)
		}
		if run.status == nil || *run.status != (agent.TaskStatusOutcome{}) {
			t.Fatalf("status = %+v, want an empty outcome", run.status)
		}
		if got := run.summary["iterations"]; got != float64(1) {
			t.Errorf("iterations = %v, want 1", got)
		}
		if got := run.summary["outcome"]; got != "error" {
			t.Errorf("outcome = %v, want error", got)
		}
		if run.mock.agentCalls != 1 {
			t.Errorf("chat calls = %d, want 1", run.mock.agentCalls)
		}
	})
}

func TestAgentLoopMaxIterationsInvariant(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sc := genLoopScenario(t)
		run := runScenario(t, sc)
		iterations := run.summary["iterations"].(float64)
		if iterations > float64(sc.maxIterations) {
			t.Fatalf("iterations %v exceed maxIterations %d", iterations, sc.maxIterations)
		}
	})
}

func TestAgentLoopTerminatesAtMaxIterations(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		maxIter := rapid.IntRange(1, 8).Draw(t, "maxIterations")
		extra := rapid.IntRange(0, 5).Draw(t, "extraResponses")

		responses := make([]loopResponse, maxIter+extra)
		for i := range responses {
			responses[i] = loopResponse{
				finishReason: model.FinishReasonTool,
				toolCalls:    []model.ResponseToolCall{vmExecCall("c1", "echo x")},
				usage:        totalUsage(50),
			}
		}

		run := runScenario(t, loopScenario{
			maxIterations: maxIter,
			window:        16384,
			responses:     responses,
			llm:           defaultLLMConfig(),
		})
		if run.err != nil {
			t.Fatalf("Execute: %v", run.err)
		}
		if run.status == nil || *run.status != (agent.TaskStatusOutcome{}) {
			t.Fatalf("status = %+v, want an empty outcome", run.status)
		}
		if got := run.summary["iterations"]; got != float64(maxIter) {
			t.Errorf("iterations = %v, want %d", got, maxIter)
		}
		if got := run.summary["outcome"]; got != "budget_exhausted" {
			t.Errorf("outcome = %v, want budget_exhausted", got)
		}
		if run.mock.agentCalls != maxIter {
			t.Errorf("chat calls = %d, want %d", run.mock.agentCalls, maxIter)
		}
		if run.mock.handoverCalls != 0 {
			t.Errorf("handover calls = %d, want 0", run.mock.handoverCalls)
		}
	})
}

func TestAgentLoopTerminatesOnTaskStatus(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		maxIter := rapid.IntRange(3, 10).Draw(t, "maxIterations")
		tsPosition := rapid.IntRange(0, maxIter-1).Draw(t, "tsPosition")
		status := rapid.SampledFrom([]string{agent.TaskStatusCompleted, agent.TaskStatusFailed, agent.TaskStatusError}).Draw(t, "status")
		reason := rapid.SampledFrom([]string{"done", "all checks passed", "command failed"}).Draw(t, "reason")
		tsFirst := rapid.Bool().Draw(t, "taskStatusFirstInBatch")

		responses := make([]loopResponse, maxIter+2)
		for i := range responses {
			switch {
			case i == tsPosition && tsFirst:
				responses[i] = loopResponse{
					finishReason: model.FinishReasonTool,
					toolCalls:    []model.ResponseToolCall{taskStatusCall("ts", status, reason), vmExecCall("later", "echo x")},
					usage:        totalUsage(50),
				}
			case i == tsPosition:
				responses[i] = loopResponse{
					finishReason: model.FinishReasonTool,
					toolCalls:    []model.ResponseToolCall{vmExecCall("before", "echo x"), taskStatusCall("ts", status, reason)},
					usage:        totalUsage(50),
				}
			default:
				responses[i] = loopResponse{
					finishReason: model.FinishReasonTool,
					toolCalls:    []model.ResponseToolCall{vmExecCall("c1", "echo x")},
					usage:        totalUsage(50),
				}
			}
		}

		run := runScenario(t, loopScenario{
			maxIterations: maxIter,
			window:        16384,
			responses:     responses,
			llm:           defaultLLMConfig(),
		})
		if run.err != nil {
			t.Fatalf("Execute: %v", run.err)
		}
		want := agent.TaskStatusOutcome{CompletedStatus: status, Reason: reason}
		if run.status == nil || *run.status != want {
			t.Fatalf("status = %+v, want %+v", run.status, want)
		}
		wantIter := tsPosition + 1
		if got := run.summary["iterations"]; got != float64(wantIter) {
			t.Errorf("iterations = %v, want %d", got, wantIter)
		}
		if got := run.summary["status"]; got != status {
			t.Errorf("summary status = %v, want %q", got, status)
		}
		if got := run.summary["reason"]; got != reason {
			t.Errorf("summary reason = %v, want %q", got, reason)
		}
		wantOutcome := map[string]string{
			agent.TaskStatusCompleted: "ok",
			agent.TaskStatusFailed:    "failed",
			agent.TaskStatusError:     "error",
		}[status]
		if got := run.summary["outcome"]; got != wantOutcome {
			t.Errorf("outcome = %v, want %q", got, wantOutcome)
		}
		wantCalls := wantIter
		if !tsFirst {
			wantCalls++
		}
		if got := run.summary["tool_calls"]; got != float64(wantCalls) {
			t.Errorf("tool_calls = %v, want %d", got, wantCalls)
		}
		if run.mock.agentCalls != wantIter {
			t.Errorf("chat calls = %d, want %d", run.mock.agentCalls, wantIter)
		}
		if run.mock.handoverCalls != 0 {
			t.Errorf("handover calls = %d, want 0", run.mock.handoverCalls)
		}
	})
}

func TestAgentLoopInvalidTaskStatusRetries(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		kind := rapid.SampledFrom([]string{"bad_status", "missing_reason", "blank_reason"}).Draw(t, "invalidKind")
		var bad model.ResponseToolCall
		switch kind {
		case "bad_status":
			bad = taskStatusCall("ts", "Done", "x")
		case "missing_reason":
			bad = taskStatusCall("ts", agent.TaskStatusCompleted, "")
		default:
			bad = taskStatusCall("ts", agent.TaskStatusCompleted, "   ")
		}
		good := taskStatusCall("ts2", agent.TaskStatusCompleted, "all checks passed")

		run := runScenario(t, loopScenario{
			maxIterations: 3,
			window:        8192,
			responses: []loopResponse{
				{finishReason: model.FinishReasonTool, toolCalls: []model.ResponseToolCall{bad}, usage: totalUsage(100)},
				{finishReason: model.FinishReasonTool, toolCalls: []model.ResponseToolCall{good}, usage: totalUsage(100)},
			},
			llm: defaultLLMConfig(),
		})
		if run.err != nil {
			t.Fatalf("Execute: %v", run.err)
		}
		want := agent.TaskStatusOutcome{CompletedStatus: agent.TaskStatusCompleted, Reason: "all checks passed"}
		if run.status == nil || *run.status != want {
			t.Fatalf("status = %+v, want %+v", run.status, want)
		}
		if got := run.summary["iterations"]; got != float64(2) {
			t.Errorf("iterations = %v, want 2", got)
		}
		if got := run.summary["tool_calls"]; got != float64(2) {
			t.Errorf("tool_calls = %v, want 2", got)
		}
		if got := run.summary["tool_failures"]; got != float64(1) {
			t.Errorf("tool_failures = %v, want 1", got)
		}
		req := run.mock.captured[1]
		msgs, _ := req["messages"].([]model.D)
		found := false
		for _, m := range msgs {
			if m["role"] != "tool" || m["name"] != "task_status" {
				continue
			}
			found = true
			content, _ := m["content"].(string)
			if !strings.Contains(content, `"status":"FAILED"`) {
				t.Errorf("invalid task_status response = %q, want a tool error envelope", content)
			}
		}
		if !found {
			t.Error("retry request is missing the invalid task_status tool result")
		}
	})
}

func TestAgentLoopToolCallAndToolErrorCounts(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sc := genLoopScenario(t)
		run := runScenario(t, sc)

		toolCalls := run.summary["tool_calls"].(float64)
		toolFailures := run.summary["tool_failures"].(float64)
		if toolFailures > toolCalls {
			t.Fatalf("tool failures %v > tool calls %v", toolFailures, toolCalls)
		}

		callEvents := eventsOf(run.events, eventAgentToolCall)
		failedEvents := eventsOf(run.events, eventToolFailed)
		if got := len(failedEvents); got != int(toolFailures) {
			t.Errorf("tool_failed events = %d, want %v", got, toolFailures)
		}
		if got, want := len(eventsOf(run.events, eventToolSucceeded)), int(toolCalls-toolFailures); got != want {
			t.Errorf("tool_succeeded events = %d, want %d", got, want)
		}
		if len(callEvents) < int(toolCalls) {
			t.Errorf("agent_tool_call events = %d, want at least the %v executed calls", len(callEvents), toolCalls)
		}
		for _, e := range callEvents {
			if e["id"] == nil || e["id"] == "" {
				t.Errorf("agent_tool_call missing id: %v", e)
			}
			if _, ok := e["iter"].(float64); !ok {
				t.Errorf("agent_tool_call missing numeric iter: %v", e)
			}
		}
		for _, e := range failedEvents {
			if _, ok := e["dur_ms"].(float64); !ok {
				t.Errorf("tool_failed missing numeric dur_ms: %v", e)
			}
		}
		if got := len(eventsOf(run.events, eventIteration)); got != int(run.summary["iterations"].(float64)) {
			t.Errorf("iteration events = %d, want %v", got, run.summary["iterations"])
		}
		withUsage := 0
		for _, u := range run.mock.usages {
			if u != nil {
				withUsage++
			}
		}
		if got := len(eventsOf(run.events, eventTokenUsage)); got != withUsage {
			t.Errorf("token_usage events = %d, want %d", got, withUsage)
		}
	})
}

func TestAgentLoopTracking(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sc := genLoopScenario(t)
		run := runScenario(t, sc)

		if got := run.summary["iterations"]; got != float64(run.mock.agentCalls) {
			t.Errorf("iterations = %v, chat calls = %d", got, run.mock.agentCalls)
		}
		wantPrompt, wantCompletion := sumUsages(run.mock.usages)
		if got := run.summary["prompt_tokens"]; got != float64(wantPrompt) {
			t.Errorf("prompt_tokens = %v, want %d", got, wantPrompt)
		}
		if got := run.summary["completion_tokens"]; got != float64(wantCompletion) {
			t.Errorf("completion_tokens = %v, want %d", got, wantCompletion)
		}
		if got := run.summary["compactions"]; got != float64(0) {
			t.Errorf("compactions = %v, want 0", got)
		}
		if got := run.summary["reclaimed_tokens"]; got != float64(0) {
			t.Errorf("reclaimed_tokens = %v, want 0", got)
		}
	})
}

func TestAgentLoopRunSummary(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sc := genLoopScenario(t)
		run := runScenario(t, sc)

		if got := len(eventsOf(run.events, eventRunSummary)); got != 1 {
			t.Fatalf("run_summary events = %d, want 1", got)
		}
		iterations := run.summary["iterations"].(float64)
		if iterations > float64(sc.maxIterations) {
			t.Errorf("iterations = %v, maxIterations = %d", iterations, sc.maxIterations)
		}
		if got := run.summary["tool_failures"].(float64); got > run.summary["tool_calls"].(float64) {
			t.Errorf("tool_failures = %v > tool_calls = %v", got, run.summary["tool_calls"])
		}

		wantStatus, wantReason := "", ""
		if run.status != nil {
			wantStatus, wantReason = run.status.CompletedStatus, run.status.Reason
		}
		if got := run.summary["status"]; got != wantStatus {
			t.Errorf("status = %v, want %q", got, wantStatus)
		}
		if got := run.summary["reason"]; got != wantReason {
			t.Errorf("reason = %v, want %q", got, wantReason)
		}

		outcome, _ := run.summary["outcome"].(string)
		switch outcome {
		case "timeout":
			if run.err == nil || !errors.Is(run.err, context.DeadlineExceeded) {
				t.Errorf("outcome = timeout with err %v", run.err)
			}
		case "error":
			if wantStatus == agent.TaskStatusCompleted || wantStatus == agent.TaskStatusFailed {
				t.Errorf("outcome = error with status %q", wantStatus)
			}
		case "ok":
			if run.err != nil || wantStatus != agent.TaskStatusCompleted {
				t.Errorf("outcome = ok with err %v and status %q", run.err, wantStatus)
			}
		case "failed":
			if run.err != nil || wantStatus != agent.TaskStatusFailed {
				t.Errorf("outcome = failed with err %v and status %q", run.err, wantStatus)
			}
		case "budget_exhausted":
			if run.err != nil || wantStatus != "" || iterations != float64(sc.maxIterations) {
				t.Errorf("outcome = budget_exhausted with err %v, status %q, iterations %v/%d",
					run.err, wantStatus, iterations, sc.maxIterations)
			}
		default:
			t.Errorf("unknown outcome %q", outcome)
		}

		if got := run.summary["avg_iter_ms"].(float64); iterations > 0 {
			want := float64(int64(run.summary["dur_ms"].(float64)) / int64(iterations))
			if got != want {
				t.Errorf("avg_iter_ms = %v, want %v", got, want)
			}
		} else if got != 0 {
			t.Errorf("avg_iter_ms = %v, want 0 with no iterations", got)
		}

		if sc.window > 0 {
			if got := run.summary["ctx_window"]; got != float64(sc.window) {
				t.Errorf("ctx_window = %v, want %d", got, sc.window)
			}
		} else if _, ok := run.summary["ctx_window"]; ok {
			t.Error("ctx_window must be omitted when the model reports no window")
		}
	})
}

func TestAgentLoopRunSummaryTimeoutAndBackendError(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantOutcome string
		wantTimeout bool
	}{
		{"timeout", context.DeadlineExceeded, "timeout", true},
		{"backend error", errors.New("backend down"), "error", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := runScenario(t, loopScenario{
				maxIterations: 3,
				window:        8192,
				chatFunc: func(context.Context, model.D) (model.ChatResponse, error) {
					return model.ChatResponse{}, tt.err
				},
				llm: defaultLLMConfig(),
			})
			if run.err == nil {
				t.Fatal("Execute succeeded against a failing backend")
			}
			if got := errors.Is(run.err, context.DeadlineExceeded); got != tt.wantTimeout {
				t.Errorf("timeout = %v, want %v (err %v)", got, tt.wantTimeout, run.err)
			}
			if run.status != nil {
				t.Errorf("status = %+v, want nil", run.status)
			}
			if got := run.summary["iterations"]; got != float64(1) {
				t.Errorf("iterations = %v, want 1 for the aborted round", got)
			}
			if got := run.summary["outcome"]; got != tt.wantOutcome {
				t.Errorf("outcome = %v, want %q", got, tt.wantOutcome)
			}
		})
	}
}

func TestAgentLoopCompaction(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		window := rapid.IntRange(2048, 16384).Draw(t, "window")
		threshold := rapid.Float64Range(0.6, 0.9).Draw(t, "threshold")
		extra := rapid.IntRange(1, 1000).Draw(t, "extraTokens")
		enabled := rapid.Bool().Draw(t, "enabled")
		failHandover := rapid.Bool().Draw(t, "failHandover")

		total := int(threshold*float64(window)) + extra
		sc := loopScenario{
			maxIterations: 3,
			window:        window,
			responses: []loopResponse{
				{finishReason: model.FinishReasonTool, toolCalls: []model.ResponseToolCall{vmExecCall("c1", "echo x")}, usage: totalUsage(total)},
				{finishReason: model.FinishReasonTool, toolCalls: []model.ResponseToolCall{taskStatusCall("ts", agent.TaskStatusCompleted, "done")}, usage: totalUsage(100)},
			},
			compaction: config.CompactionConfig{
				Enabled: enabled, Threshold: threshold, MaxSummaryTokens: 512, MaxAttempts: 3,
			},
			failHandover: failHandover,
			llm:          defaultLLMConfig(),
		}
		run := runScenario(t, sc)
		if run.err != nil {
			t.Fatalf("compaction must not end the run: %v", run.err)
		}
		if run.status == nil || run.status.CompletedStatus != agent.TaskStatusCompleted {
			t.Fatalf("status = %+v, want Completed", run.status)
		}
		if got := run.summary["iterations"]; got != float64(2) {
			t.Errorf("iterations = %v, want 2", got)
		}

		switch {
		case !enabled:
			if run.mock.handoverCalls != 0 {
				t.Errorf("disabled compaction made %d handover calls", run.mock.handoverCalls)
			}
			if got := run.summary["compactions"]; got != float64(0) {
				t.Errorf("compactions = %v, want 0", got)
			}
			if got := run.summary["reclaimed_tokens"]; got != float64(0) {
				t.Errorf("reclaimed_tokens = %v, want 0", got)
			}
		case failHandover:
			if run.mock.handoverCalls != 1 {
				t.Errorf("handover calls = %d, want 1", run.mock.handoverCalls)
			}
			if got := run.summary["compactions"]; got != float64(0) {
				t.Errorf("compactions = %v, want 0 after a failed handover", got)
			}
			events := eventsOf(run.events, eventContextCompaction)
			if len(events) != 1 || events[0]["outcome"] != "failed" {
				t.Errorf("context_compaction events = %v, want one failed", events)
			}
			if _, ok := events[0]["error"]; !ok {
				t.Error("failed compaction must log its reason")
			}
			if got := len(eventsOf(run.events, eventContextCompactionApplied)); got != 0 {
				t.Errorf("applied events = %d, want 0", got)
			}
		default:
			if run.mock.handoverCalls != 1 {
				t.Errorf("handover calls = %d, want 1", run.mock.handoverCalls)
			}
			if got := run.summary["compactions"]; got != float64(1) {
				t.Errorf("compactions = %v, want 1", got)
			}
			events := eventsOf(run.events, eventContextCompaction)
			if len(events) != 1 || events[0]["outcome"] != "applied" {
				t.Fatalf("context_compaction events = %v, want one applied", events)
			}
			if got, want := run.summary["reclaimed_tokens"], events[0]["reclaimed"]; got != want {
				t.Errorf("reclaimed_tokens = %v, want event reclaimed %v", got, want)
			}
			if len(run.mock.captured) < 3 || !isCompactionRequest(run.mock.captured[1]) {
				t.Fatalf("handover request was not captured between agent calls: %d calls", len(run.mock.captured))
			}
			handoverMsgs, _ := run.mock.captured[1]["messages"].([]model.D)
			if len(handoverMsgs) < 2 {
				t.Fatalf("handover request has %d messages", len(handoverMsgs))
			}
			history, _ := handoverMsgs[1]["content"].(string)
			if !strings.Contains(history, "tool result vm_exec") {
				t.Errorf("handover history did not see the tool result: %q", history)
			}
			system, _ := handoverMsgs[0]["content"].(string)
			for _, want := range []string{"## Done", "## In progress", "## Not started", "handover"} {
				if !strings.Contains(system, want) {
					t.Errorf("compaction system prompt missing %q", want)
				}
			}
			nextMsgs, _ := run.mock.captured[2]["messages"].([]model.D)
			if len(nextMsgs) < 2 {
				t.Fatalf("post-compaction request has %d messages", len(nextMsgs))
			}
			user, _ := nextMsgs[1]["content"].(string)
			for _, want := range []string{"## Task", "## Work completed so far", "## Active output handles"} {
				if !strings.Contains(user, want) {
					t.Errorf("post-compaction user message missing %q: %q", want, user)
				}
			}
			if got := len(eventsOf(run.events, eventContextCompactionApplied)); got != 1 {
				t.Errorf("applied events = %d, want 1", got)
			}
		}
	})
}

func TestAgentLoopCompactionFailuresDoNotEndRun(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 2).Draw(t, "handoverOutcomes")
		outcomes := make([]string, n)
		for i := range outcomes {
			outcomes[i] = rapid.SampledFrom([]string{"valid", "short", "empty", "error"}).Draw(t, "outcome")
		}

		idx := 0
		handover := func(context.Context, model.D) (model.ChatResponse, error) {
			if idx >= len(outcomes) {
				return chatResp("", "", model.FinishReasonStop, nil, nil), nil
			}
			outcome := outcomes[idx]
			idx++
			switch outcome {
			case "valid":
				return chatResp(validHandoverSummary, "", model.FinishReasonStop, nil, nil), nil
			case "short":
				return chatResp("too short", "", model.FinishReasonStop, nil, nil), nil
			case "empty":
				return chatResp("", "", model.FinishReasonStop, nil, nil), nil
			default:
				return model.ChatResponse{}, context.DeadlineExceeded
			}
		}

		run := runScenario(t, loopScenario{
			maxIterations: 3,
			window:        8192,
			responses: []loopResponse{
				{finishReason: model.FinishReasonTool, toolCalls: []model.ResponseToolCall{vmExecCall("c1", "echo x")}, usage: totalUsage(7000)},
				{finishReason: model.FinishReasonTool, toolCalls: []model.ResponseToolCall{taskStatusCall("ts", agent.TaskStatusCompleted, "done")}, usage: totalUsage(100)},
			},
			compaction: config.CompactionConfig{Enabled: true, Threshold: 0.8, MaxSummaryTokens: 512, MaxAttempts: 3},
			handover:   handover,
			llm:        defaultLLMConfig(),
		})
		if run.err != nil {
			t.Fatalf("compaction failure must not end the run: %v", run.err)
		}
		if run.status == nil || run.status.CompletedStatus != agent.TaskStatusCompleted {
			t.Fatalf("status = %+v, want Completed", run.status)
		}

		wantHandovers := 2
		if outcomes[0] == "valid" || outcomes[0] == "error" {
			wantHandovers = 1
		}
		if run.mock.handoverCalls != wantHandovers {
			t.Errorf("handover calls = %d, want %d for outcomes %v", run.mock.handoverCalls, wantHandovers, outcomes)
		}
		wantApplied := outcomes[0] == "valid" || ((outcomes[0] == "short" || outcomes[0] == "empty") && len(outcomes) > 1 && outcomes[1] == "valid")
		gotApplied := run.summary["compactions"].(float64) == 1
		if gotApplied != wantApplied {
			t.Errorf("applied = %v, want %v for outcomes %v", gotApplied, wantApplied, outcomes)
		}
		events := eventsOf(run.events, eventContextCompaction)
		if len(events) != 1 {
			t.Fatalf("context_compaction events = %d, want 1", len(events))
		}
		if gotApplied {
			if got, want := run.summary["reclaimed_tokens"], events[0]["reclaimed"]; got != want {
				t.Errorf("reclaimed_tokens = %v, want event reclaimed %v", got, want)
			}
		} else if got := run.summary["reclaimed_tokens"]; got != float64(0) {
			t.Errorf("reclaimed_tokens = %v, want 0 when compaction failed", got)
		}
		wantOutcome := "failed"
		if wantApplied {
			wantOutcome = "applied"
		}
		if events[0]["outcome"] != wantOutcome {
			t.Errorf("context_compaction outcome = %v, want %q", events[0]["outcome"], wantOutcome)
		}
		if !wantApplied {
			if _, ok := events[0]["error"]; !ok {
				t.Error("failed compaction must log its reason")
			}
		}
		if got := len(eventsOf(run.events, eventContextCompactionApplied)); got != map[bool]int{true: 1, false: 0}[wantApplied] {
			t.Errorf("applied events = %d, want %d", got, map[bool]int{true: 1, false: 0}[wantApplied])
		}
	})
}

func TestAgentLoopCompactionHandoverShapes(t *testing.T) {
	tests := []struct {
		name string
		resp model.ChatResponse
	}{
		{"no choices", model.ChatResponse{}},
		{"nil message", model.ChatResponse{Choices: []model.Choice{{}}}},
		{"whitespace", chatResp("   ", "", model.FinishReasonStop, nil, nil)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handovers := 0
			run := runScenario(t, loopScenario{
				maxIterations: 3,
				window:        8192,
				responses: []loopResponse{
					{finishReason: model.FinishReasonTool, toolCalls: []model.ResponseToolCall{vmExecCall("c1", "echo x")}, usage: totalUsage(7000)},
					{finishReason: model.FinishReasonTool, toolCalls: []model.ResponseToolCall{taskStatusCall("ts", agent.TaskStatusCompleted, "done")}, usage: totalUsage(100)},
				},
				compaction: config.CompactionConfig{Enabled: true, Threshold: 0.8, MaxSummaryTokens: 512, MaxAttempts: 3},
				handover: func(context.Context, model.D) (model.ChatResponse, error) {
					handovers++
					return tt.resp, nil
				},
				llm: defaultLLMConfig(),
			})
			if run.err != nil {
				t.Fatalf("compaction failure must not end the run: %v", run.err)
			}
			if run.status == nil || run.status.CompletedStatus != agent.TaskStatusCompleted {
				t.Fatalf("status = %+v, want Completed", run.status)
			}
			if got := run.summary["compactions"]; got != float64(0) {
				t.Errorf("compactions = %v, want 0", got)
			}
			if handovers != 2 {
				t.Errorf("handover calls = %d, want 2 (summary and repair)", handovers)
			}
			events := eventsOf(run.events, eventContextCompaction)
			if len(events) != 1 || events[0]["outcome"] != "failed" {
				t.Fatalf("context_compaction events = %v, want one failed", events)
			}
			if _, ok := events[0]["error"]; !ok {
				t.Error("failed compaction must log its reason")
			}
		})
	}
}

func TestAgentLoopTokenizeFallback(t *testing.T) {
	tests := []struct {
		name string
		sc   loopScenario
	}{
		{"tokenize error", loopScenario{tokenizeErr: errors.New("tokenizer down")}},
		{"zero tokens", loopScenario{tokenizeZero: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.sc.maxIterations = 3
			tt.sc.window = 8192
			tt.sc.responses = []loopResponse{
				{finishReason: model.FinishReasonTool, toolCalls: []model.ResponseToolCall{vmExecCall("c1", "echo x")}, usage: totalUsage(7000)},
				{finishReason: model.FinishReasonTool, toolCalls: []model.ResponseToolCall{taskStatusCall("ts", agent.TaskStatusCompleted, "done")}, usage: totalUsage(100)},
			}
			tt.sc.compaction = config.CompactionConfig{Enabled: true, Threshold: 0.8, MaxSummaryTokens: 512, MaxAttempts: 3}
			tt.sc.llm = defaultLLMConfig()

			run := runScenario(t, tt.sc)
			if run.err != nil {
				t.Fatalf("Execute: %v", run.err)
			}
			if run.status == nil || run.status.CompletedStatus != agent.TaskStatusCompleted {
				t.Fatalf("status = %+v, want Completed", run.status)
			}
			if got := run.summary["compactions"]; got != float64(1) {
				t.Errorf("compactions = %v, want 1", got)
			}
		})
	}
}

func TestAgentLoopCompactionWithOutputHandles(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		window := rapid.IntRange(4096, 16384).Draw(t, "window")
		threshold := 0.5
		total := int(threshold*float64(window)) + 1000
		size := rapid.IntRange(1, 4096).Draw(t, "capturedBytes")

		run := runScenario(t, loopScenario{
			maxIterations: 3,
			window:        window,
			responses: []loopResponse{
				{
					content:      "running the capture",
					reasoning:    "ran the capture",
					finishReason: model.FinishReasonTool,
					toolCalls:    []model.ResponseToolCall{vmExecCall("c1", "echo hi")},
					usage:        totalUsage(total),
				},
				{finishReason: model.FinishReasonTool, toolCalls: []model.ResponseToolCall{taskStatusCall("ts", agent.TaskStatusCompleted, "done")}, usage: totalUsage(100)},
			},
			compaction: config.CompactionConfig{Enabled: true, Threshold: threshold, MaxSummaryTokens: 512, MaxAttempts: 3},
			output: config.OutputConfig{
				Enabled: true, Mode: "always",
				MaxCommandSize: 1 << 20, MaxTotalSize: 1 << 26, SearchMaxMatches: 200,
			},
			scripted: func(command string) (string, int) {
				switch {
				case strings.HasPrefix(command, "id -u"), strings.HasPrefix(command, "id -g"):
					return "1000\n", 0
				case strings.HasPrefix(command, "getent passwd"):
					return "ubuntu:x:1000:1000:Ubuntu:/home/ubuntu:/bin/bash\n", 0
				case strings.Contains(command, "df -P"):
					return "99999999", 0
				case strings.Contains(command, "mkdir"):
					return "", 0
				case strings.Contains(command, "__MPH_EXIT_"):
					return exitMarker(command) + "0", 0
				case strings.Contains(command, "wc -c"):
					return fmt.Sprintf("%d", size), 0
				case strings.Contains(command, "wc -l"):
					return "1", 0
				default:
					return "", 0
				}
			},
			llm: defaultLLMConfig(),
		})
		if run.err != nil {
			t.Fatalf("Execute: %v", run.err)
		}
		if run.status == nil || run.status.CompletedStatus != agent.TaskStatusCompleted {
			t.Fatalf("status = %+v, want Completed", run.status)
		}
		if got := run.summary["compactions"]; got != float64(1) {
			t.Fatalf("compactions = %v, want 1", got)
		}

		handoverMsgs, _ := run.mock.captured[1]["messages"].([]model.D)
		history, _ := handoverMsgs[1]["content"].(string)
		if !strings.Contains(history, "tool result vm_exec") {
			t.Errorf("handover history did not see the tool result: %q", history)
		}
		if !strings.Contains(history, "ran the capture") {
			t.Errorf("handover history did not see the assistant reasoning: %q", history)
		}

		nextMsgs, _ := run.mock.captured[2]["messages"].([]model.D)
		user, _ := nextMsgs[1]["content"].(string)
		for _, want := range []string{"## Active output handles", "from `echo hi`", "Only use the output_id values listed above"} {
			if !strings.Contains(user, want) {
				t.Errorf("post-compaction user message missing %q: %q", want, user)
			}
		}
	})
}

func TestAgentLoopCompactionPreventsWindowOverflow(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		window := rapid.IntRange(4096, 32768).Draw(t, "window")
		threshold := rapid.Float64Range(0.55, 0.8).Draw(t, "threshold")
		steps := rapid.IntRange(5, 20).Draw(t, "steps")
		stepBytes := int(float64(window)*threshold*4*1.5/float64(steps)) + 100

		responses := make([]loopResponse, 0, steps+1)
		for i := 0; i < steps; i++ {
			responses = append(responses, loopResponse{
				finishReason: model.FinishReasonTool,
				toolCalls:    []model.ResponseToolCall{vmExecCall("c1", "step")},
			})
		}
		responses = append(responses, loopResponse{
			finishReason: model.FinishReasonTool,
			toolCalls:    []model.ResponseToolCall{taskStatusCall("ts", agent.TaskStatusCompleted, "done")},
		})

		run := runScenario(t, loopScenario{
			maxIterations: steps + 4,
			window:        window,
			responses:     responses,
			compaction:    config.CompactionConfig{Enabled: true, Threshold: threshold, MaxSummaryTokens: 512, MaxAttempts: 3},
			agent:         config.AgentConfig{MaxOutputBytes: 64 * 1024},
			scripted: func(command string) (string, int) {
				switch {
				case strings.HasPrefix(command, "id -u"), strings.HasPrefix(command, "id -g"):
					return "1000\n", 0
				case strings.HasPrefix(command, "getent passwd"):
					return "ubuntu:x:1000:1000:Ubuntu:/home/ubuntu:/bin/bash\n", 0
				case strings.Contains(command, "step"):
					return strings.Repeat("k", stepBytes), 0
				default:
					return "", 0
				}
			},
			enforceWindow: true,
			llm:           defaultLLMConfig(),
		})
		if run.err != nil {
			t.Fatalf("run grew past the window without compacting: %v", run.err)
		}
		if run.enforcer == nil {
			t.Fatal("window enforcer was not installed")
		}
		if run.enforcer.overWindow != 0 {
			t.Errorf("%d chat requests exceeded the %d-token window before compaction", run.enforcer.overWindow, window)
		}
		if got := run.summary["compactions"].(float64); got == 0 {
			t.Fatal("no compaction applied; the window enforcer never forced one")
		}
		if run.status == nil || run.status.CompletedStatus != agent.TaskStatusCompleted {
			t.Errorf("status = %+v, want Completed", run.status)
		}
	})
}

func TestAgentLoopOverflowRecovery(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		large := rapid.Bool().Draw(t, "largeResult")
		size := rapid.IntRange(4096, 16384).Draw(t, "resultBytes")
		window := rapid.IntRange(4096, 16384).Draw(t, "window")

		run := runScenario(t, loopScenario{
			maxIterations: 3,
			window:        window,
			responses: []loopResponse{
				{finishReason: model.FinishReasonTool, toolCalls: []model.ResponseToolCall{vmExecCall("c1", "big")}, usage: totalUsage(100)},
				{finishReason: model.FinishReasonTool, toolCalls: []model.ResponseToolCall{taskStatusCall("ts", agent.TaskStatusCompleted, "done")}, usage: totalUsage(100)},
			},
			agent:       config.AgentConfig{MaxOutputBytes: 64 * 1024},
			overflowAt:  1,
			overflowErr: overflowErr(9000, window),
			scripted: func(command string) (string, int) {
				switch {
				case strings.HasPrefix(command, "id -u"), strings.HasPrefix(command, "id -g"):
					return "1000\n", 0
				case strings.HasPrefix(command, "getent passwd"):
					return "ubuntu:x:1000:1000:Ubuntu:/home/ubuntu:/bin/bash\n", 0
				case strings.Contains(command, "big"):
					if large {
						return strings.Repeat("x", size), 0
					}
					return "small", 0
				default:
					return "", 0
				}
			},
			llm: defaultLLMConfig(),
		})

		if got := run.summary["iterations"]; got != float64(2) {
			t.Errorf("iterations = %v, want 2", got)
		}
		wantToolCalls := 1
		if large {
			wantToolCalls = 2
		}
		if got := run.summary["tool_calls"]; got != float64(wantToolCalls) {
			t.Errorf("tool_calls = %v, want %d", got, wantToolCalls)
		}
		if got := run.summary["tool_failures"]; got != float64(0) {
			t.Errorf("tool_failures = %v, want 0", got)
		}
		event := onlyEvent(t, run.events, eventContextOverflow)
		if event["prompt_tokens"] != float64(9000) || event["ctx_window"] != float64(window) {
			t.Errorf("overflow measurements = %v/%v, want 9000/%d", event["prompt_tokens"], event["ctx_window"], window)
		}

		if large {
			if run.err != nil {
				t.Fatalf("recoverable overflow returned an error: %v", run.err)
			}
			if run.status == nil || run.status.CompletedStatus != agent.TaskStatusCompleted {
				t.Fatalf("status = %+v, want Completed", run.status)
			}
			if event["outcome"] != "recovered" {
				t.Errorf("overflow outcome = %v, want recovered", event["outcome"])
			}
			if event["replaced"] != float64(1) {
				t.Errorf("replaced = %v, want 1", event["replaced"])
			}
			if event["reclaimed_bytes"].(float64) <= 0 {
				t.Errorf("reclaimed_bytes = %v, want > 0", event["reclaimed_bytes"])
			}
			if run.mock.agentCalls != 3 {
				t.Errorf("chat calls = %d, want 3 (initial, overflow, retry)", run.mock.agentCalls)
			}
			if len(run.mock.captured) < 3 {
				t.Fatalf("captured requests = %d, want at least 3", len(run.mock.captured))
			}
			retry, _ := json.Marshal(run.mock.captured[2])
			if strings.Contains(string(retry), strings.Repeat("x", 100)) {
				t.Error("the oversized result survived into the retry request")
			}
			if !strings.Contains(string(retry), "was discarded") {
				t.Errorf("retry request is missing the replacement error: %s", retry)
			}
			if got := run.summary["outcome"]; got != "ok" {
				t.Errorf("outcome = %v, want ok", got)
			}
		} else {
			if run.err == nil {
				t.Fatal("an unrecoverable overflow must fail the run")
			}
			if !errors.Is(run.err, model.ErrInvalidRequest) {
				t.Errorf("error must preserve the kronk cause: %v", run.err)
			}
			if !strings.Contains(run.err.Error(), "context overflow") {
				t.Errorf("error must name the repair attempt: %v", run.err)
			}
			if run.status != nil {
				t.Errorf("status = %+v, want nil", run.status)
			}
			if event["outcome"] != "exhausted" {
				t.Errorf("overflow outcome = %v, want exhausted", event["outcome"])
			}
			if event["replaced"] != float64(0) {
				t.Errorf("replaced = %v, want 0", event["replaced"])
			}
			if run.mock.agentCalls != 2 {
				t.Errorf("chat calls = %d, want 2 (initial, overflow)", run.mock.agentCalls)
			}
			if got := run.summary["outcome"]; got != "error" {
				t.Errorf("outcome = %v, want error", got)
			}
		}
	})
}

func TestAgentLoopOverflowClassification(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		wantOverflow bool
	}{
		{"overflow", overflowErr(9000, 8192), true},
		{"other invalid request", fmt.Errorf("%w: messages[0].content[0]: file inputs are not currently supported", model.ErrInvalidRequest), false},
		{"unrelated error", errors.New("connection reset"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := runScenario(t, loopScenario{
				maxIterations: 2,
				window:        8192,
				chatFunc: func(context.Context, model.D) (model.ChatResponse, error) {
					return model.ChatResponse{}, tt.err
				},
				llm: defaultLLMConfig(),
			})
			if run.err == nil {
				t.Fatal("Execute succeeded against a failing backend")
			}
			if got := len(eventsOf(run.events, eventContextOverflow)); (got > 0) != tt.wantOverflow {
				t.Errorf("context_overflow events = %d, want overflow=%v", got, tt.wantOverflow)
			}
			if tt.wantOverflow {
				event := onlyEvent(t, run.events, eventContextOverflow)
				if event["outcome"] != "exhausted" {
					t.Errorf("overflow outcome = %v, want exhausted", event["outcome"])
				}
			}
			if got := run.summary["outcome"]; got != "error" {
				t.Errorf("outcome = %v, want error", got)
			}
		})
	}
}

func TestAgentLoopToolOutputTooLarge(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		size := rapid.IntRange(4097, 65536).Draw(t, "outputBytes")
		captures := rapid.Bool().Draw(t, "outputEnabled")

		output := config.OutputConfig{}
		if captures {
			output = config.OutputConfig{
				Enabled: true, Mode: "auto",
				MaxCommandSize: 1 << 20, MaxTotalSize: 1 << 26, SearchMaxMatches: 200,
			}
		}
		run := runScenario(t, loopScenario{
			maxIterations: 3,
			window:        8192,
			responses: []loopResponse{
				{finishReason: model.FinishReasonTool, toolCalls: []model.ResponseToolCall{vmExecCall("c1", "big")}, usage: totalUsage(100)},
				{finishReason: model.FinishReasonTool, toolCalls: []model.ResponseToolCall{taskStatusCall("ts", agent.TaskStatusCompleted, "done")}, usage: totalUsage(100)},
			},
			agent:  config.AgentConfig{MaxOutputBytes: 4096},
			output: output,
			scripted: func(command string) (string, int) {
				switch {
				case strings.HasPrefix(command, "id -u"), strings.HasPrefix(command, "id -g"):
					return "1000\n", 0
				case strings.HasPrefix(command, "getent passwd"):
					return "ubuntu:x:1000:1000:Ubuntu:/home/ubuntu:/bin/bash\n", 0
				case strings.Contains(command, "big"):
					return strings.Repeat("z", size), 0
				default:
					return "", 0
				}
			},
			llm: defaultLLMConfig(),
		})
		if run.err != nil {
			t.Fatalf("Execute: %v", run.err)
		}
		if run.status == nil || run.status.CompletedStatus != agent.TaskStatusCompleted {
			t.Fatalf("status = %+v, want Completed", run.status)
		}
		if got := run.summary["tool_calls"]; got != float64(2) {
			t.Errorf("tool_calls = %v, want 2", got)
		}
		if got := run.summary["tool_failures"]; got != float64(1) {
			t.Errorf("tool_failures = %v, want 1", got)
		}

		event := onlyEvent(t, run.events, eventToolOutputTooLarge)
		if event["tool"] != "vm_exec" {
			t.Errorf("tool = %v, want vm_exec", event["tool"])
		}
		if event["bytes"] != float64(size) || event["budget"] != float64(4096) {
			t.Errorf("bytes/budget = %v/%v, want %d/4096", event["bytes"], event["budget"], size)
		}

		req := run.mock.captured[1]
		msgs, _ := req["messages"].([]model.D)
		found := false
		for _, m := range msgs {
			if m["role"] != "tool" || m["name"] != "vm_exec" {
				continue
			}
			found = true
			content, _ := m["content"].(string)
			if !strings.Contains(content, "was discarded") {
				t.Errorf("oversized tool result = %q, want a discard notice", content)
			}
			if got := strings.Contains(content, "capture: true"); got != captures {
				t.Errorf("capture guidance present = %v, want %v: %q", got, captures, content)
			}
			if strings.Contains(content, strings.Repeat("z", 100)) {
				t.Error("oversized tool result kept the payload")
			}
		}
		if !found {
			t.Error("next request is missing the vm_exec tool result")
		}
	})
}

func TestAgentLoopCaptureAttribution(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		size := rapid.IntRange(1, 8192).Draw(t, "capturedBytes")

		run := runScenario(t, loopScenario{
			maxIterations: 3,
			window:        8192,
			responses: []loopResponse{
				{finishReason: model.FinishReasonTool, toolCalls: []model.ResponseToolCall{vmExecCall("c1", "echo hi")}, usage: totalUsage(100)},
				{finishReason: model.FinishReasonTool, toolCalls: []model.ResponseToolCall{taskStatusCall("ts", agent.TaskStatusCompleted, "done")}, usage: totalUsage(100)},
			},
			agent: config.AgentConfig{MaxOutputBytes: 4096},
			output: config.OutputConfig{
				Enabled: true, Mode: "always",
				MaxCommandSize: 1 << 20, MaxTotalSize: 1 << 26, SearchMaxMatches: 200,
			},
			scripted: func(command string) (string, int) {
				switch {
				case strings.HasPrefix(command, "id -u"), strings.HasPrefix(command, "id -g"):
					return "1000\n", 0
				case strings.HasPrefix(command, "getent passwd"):
					return "ubuntu:x:1000:1000:Ubuntu:/home/ubuntu:/bin/bash\n", 0
				case strings.Contains(command, "df -P"):
					return "99999999", 0
				case strings.Contains(command, "mkdir"):
					return "", 0
				case strings.Contains(command, "__MPH_EXIT_"):
					return exitMarker(command) + "0", 0
				case strings.Contains(command, "wc -c"):
					return fmt.Sprintf("%d", size), 0
				case strings.Contains(command, "wc -l"):
					return "1", 0
				default:
					return "", 0
				}
			},
			llm: defaultLLMConfig(),
		})
		if run.err != nil {
			t.Fatalf("Execute: %v", run.err)
		}
		if run.status == nil || run.status.CompletedStatus != agent.TaskStatusCompleted {
			t.Fatalf("status = %+v, want Completed", run.status)
		}
		if got := run.summary["tool_calls"]; got != float64(2) {
			t.Errorf("tool_calls = %v, want 2", got)
		}
		if got := run.summary["tool_failures"]; got != float64(0) {
			t.Errorf("tool_failures = %v, want 0", got)
		}

		attributed := false
		for _, e := range run.events {
			if e["message"] != "command output too large for inline context, captured to file" {
				continue
			}
			attributed = true
			if e["tool"] != "vm_exec" {
				t.Errorf("attribution tool = %v, want vm_exec", e["tool"])
			}
			if id, _ := e["out_id"].(string); id == "" {
				t.Error("attribution log is missing the output_id")
			}
			break
		}
		if !attributed {
			t.Fatal("missing capture attribution log line")
		}

		req := run.mock.captured[1]
		msgs, _ := req["messages"].([]model.D)
		found := false
		for _, m := range msgs {
			if m["role"] != "tool" || m["name"] != "vm_exec" {
				continue
			}
			found = true
			content, _ := m["content"].(string)
			if !strings.Contains(content, `"captured":true`) {
				t.Errorf("tool result = %q, want a capture envelope", content)
			}
		}
		if !found {
			t.Error("next request is missing the vm_exec tool result")
		}
	})
}
