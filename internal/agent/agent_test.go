package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ardanlabs/kronk/sdk/kronk/model"
	"github.com/rs/zerolog"

	"github.com/trollLemon/MPHarness/internal/config"
	"github.com/trollLemon/MPHarness/internal/textutil"
)

const validHandoverSummary = `## Done
- Ran the requested commands and captured their results, for example curl 8.5.0 and HTTP/2 200.

## In progress
- None.

## Not started
- None.`

type mockKronk struct {
	responses    []model.ChatResponse
	errs         []error
	calls        int
	respIdx      int
	captured     []model.D
	chatFunc     func(context.Context, model.D) (model.ChatResponse, error)
	contextWidth int
	failHandover bool
}

func (m *mockKronk) ModelConfig() model.Config {
	cfg := model.Config{}
	if m.contextWidth > 0 {
		cfg.PtrContextWindow = &m.contextWidth
	}
	return cfg
}

func (m *mockKronk) Chat(ctx context.Context, req model.D) (model.ChatResponse, error) {
	if m.chatFunc != nil {
		return m.chatFunc(ctx, req)
	}
	if isCompactionRequest(req) {
		m.calls++
		m.captured = append(m.captured, req)
		if m.failHandover {
			return model.ChatResponse{}, context.DeadlineExceeded
		}
		return chatResp(validHandoverSummary, "", model.FinishReasonStop, nil, nil), nil
	}
	idx := m.respIdx
	m.respIdx++
	m.calls++
	m.captured = append(m.captured, req)
	if idx < len(m.responses) {
		var err error
		if idx < len(m.errs) && m.errs[idx] != nil {
			err = m.errs[idx]
		}
		return m.responses[idx], err
	}
	return model.ChatResponse{}, nil
}

func isCompactionRequest(req model.D) bool {
	msgs, _ := req["messages"].([]model.D)
	for _, m := range msgs {
		if c, _ := m["content"].(string); c != "" && len(c) > 40 && contains(c, "handover") {
			return true
		}
	}
	if s, _ := req["system"].(string); contains(s, "handover") {
		return true
	}
	return false
}

func (m *mockKronk) Tokenize(_ context.Context, d model.D) (model.TokenizeResponse, error) {
	input, _ := d["input"].(string)
	n := len(input) / bytesPerToken
	if n < 1 && len(input) > 0 {
		n = 1
	}
	return model.TokenizeResponse{Tokens: n}, nil
}

func strPtr(s string) *string { return &s }

func chatResp(content, reasoning, finishReason string, toolCalls []model.ResponseToolCall, usage *model.Usage) model.ChatResponse {
	var frPtr *string
	if finishReason != "" {
		frPtr = strPtr(finishReason)
	}
	msg := &model.ResponseMessage{
		Content:   content,
		Reasoning: reasoning,
		ToolCalls: toolCalls,
	}
	return model.ChatResponse{
		ID:      "test-id",
		Object:  "chat.completion",
		Created: 123,
		Model:   "test-model",
		Choices: []model.Choice{
			{
				Index:           0,
				Message:         msg,
				FinishReasonPtr: frPtr,
			},
		},
		Usage: usage,
	}
}

func defaultLLMConfig() config.LLMConfig {
	return config.LLMConfig{Temperature: 0.0, TopP: 0.1, TopK: 1}
}

func TestLLMConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.LLMConfig
		want config.LLMConfig
	}{
		{
			name: "defaults when empty",
			cfg:  config.LLMConfig{},
			want: config.LLMConfig{MaxOutputTokens: config.DefaultLLMMaxOutputTokens},
		},
		{
			name: "passthrough preserves values",
			cfg:  config.LLMConfig{Temperature: 0.7, TopP: 0.9, TopK: 5, MaxOutputTokens: 512},
			want: config.LLMConfig{Temperature: 0.7, TopP: 0.9, TopK: 5, MaxOutputTokens: 512},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewAgent(zerolog.Nop(), &mockKronk{}, Options{
				MaxIterations: 3,
				ChatTimeout:   time.Second,
				TotalTimeout:  5 * time.Second,
				LLM:           tt.cfg,
				RunID:         "",
			})
			if a.llmConfig != tt.want {
				t.Fatalf("got %+v want %+v", a.llmConfig, tt.want)
			}
		})
	}
}


func TestBuildUserPrompt(t *testing.T) {
	tests := []struct {
		name   string
		conf   config.Config
		want   []string
		leaked []string
	}{
		{
			name: "basic",
			conf: config.Config{VM: config.VMConfig{Name: "test-vm", CPU: 2, RAM: "4G", Disk: "20G"}, Prompt: "do stuff"},
			want: []string{"do stuff", "Allowed commands"},
		},
		{
			name: "with allowed commands",
			conf: config.Config{VM: config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"}, Prompt: "task", AllowedCommands: map[string]bool{"ls": true, "cat": true}},
			want: []string{"ls", "cat"},
		},
		{
			name:   "no vm details leaked",
			conf:   config.Config{VM: config.VMConfig{Name: "secret-vm", CPU: 8, RAM: "16G", Disk: "100G", Image: "noble"}, Prompt: "task"},
			want:   []string{"task", "Allowed commands"},
			leaked: []string{"secret-vm", "16G", "100G", "noble", "CPUs", "Memory", "Disk", "VM Configuration"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := buildUserPrompt(tt.conf)
			if p == "" {
				t.Fatalf("empty prompt")
			}
			for _, w := range tt.want {
				if !contains(p, w) {
					t.Errorf("missing %q in %q", w, p)
				}
			}
			for _, leaked := range tt.leaked {
				if contains(p, leaked) {
					t.Errorf("prompt should not contain VM detail %q, got %q", leaked, p)
				}
			}
		})
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i <= len(s)-len(sub); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func TestBuildToolDocuments(t *testing.T) {
	tests := []struct {
		name       string
		cfg        config.Config
		want       []string
		wantAbsent []string
	}{
		{"base tools", config.Config{}, []string{"vm_exec", "vm_info"}, []string{"output_search", "output_read"}},
		{"output tools when enabled", config.Config{Output: config.OutputConfig{Enabled: true}}, []string{"vm_exec", "vm_info", "output_search", "output_read"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			docs := buildToolDocuments(tt.cfg)
			if len(docs) == 0 {
				t.Fatalf("no tool docs")
			}
			names := map[string]bool{}
			for _, d := range docs {
				fn, _ := d["function"].(model.D)
				name, _ := fn["name"].(string)
				names[name] = true
			}
			for _, want := range tt.want {
				if !names[want] {
					t.Errorf("missing tool %q", want)
				}
			}
			for _, absent := range tt.wantAbsent {
				if names[absent] {
					t.Errorf("tool %q must be omitted, got %v", absent, names)
				}
			}
		})
	}
}

func TestBuildInitialConversation(t *testing.T) {
	tests := []struct {
		name       string
		conf       config.Config
		wantSysSub string
	}{
		{"plain", config.Config{VM: config.VMConfig{Name: "vm1", CPU: 1, RAM: "1G", Disk: "10G"}, Prompt: "task"}, ""},
		{"content dir adds suffix", config.Config{VM: config.VMConfig{Name: "vm1", CPU: 1, RAM: "1G", Disk: "10G"}, Prompt: "task", ContentDir: "/tmp/content"}, "Additional Content"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conv := buildInitialConversation(tt.conf)
			if len(conv) != 2 {
				t.Fatalf("conv len %d want 2", len(conv))
			}
			if conv[0]["role"] != "system" {
				t.Errorf("first role %v want system", conv[0]["role"])
			}
			if conv[1]["role"] != "user" {
				t.Errorf("second role %v want user", conv[1]["role"])
			}
			if tt.wantSysSub != "" {
				sys, _ := conv[0]["content"].(string)
				if !contains(sys, tt.wantSysSub) {
					t.Errorf("system prompt missing %q", tt.wantSysSub)
				}
			}
		})
	}
}




func TestBuildAssistantMessageAndAppend(t *testing.T) {
	tests := []struct {
		name          string
		content       string
		reasoning     string
		sendReasoning []bool
		wantContent   any
		wantReasoning any
	}{
		{"content and reasoning kept", "hi", "think", nil, "hi", "think"},
		{"reasoning dropped when disabled", "hi", "think", []bool{false}, "hi", nil},
		{"empty content omitted", "", "think", nil, nil, "think"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := &model.ResponseMessage{Content: tt.content, Reasoning: tt.reasoning}
			am := buildAssistantMessage(msg, []model.D{{"id": "1"}}, tt.sendReasoning...)
			if am["role"] != "assistant" {
				t.Errorf("role %v", am["role"])
			}
			if am["content"] != tt.wantContent {
				t.Errorf("content %v want %v", am["content"], tt.wantContent)
			}
			if am["reasoning_content"] != tt.wantReasoning {
				t.Errorf("reasoning %v want %v", am["reasoning_content"], tt.wantReasoning)
			}
			conv2 := append([]model.D{{"role": "system"}}, am)
			if len(conv2) != 2 {
				t.Fatalf("append len %d", len(conv2))
			}
		})
	}
}

func TestBuildToolResponseMessage(t *testing.T) {
	tests := []struct {
		name     string
		id       string
		toolName string
		content  string
	}{
		{"exec result", "call-1", "vm_exec", "out"},
		{"search result", "call-2", "output_search", "{}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc := model.ResponseToolCall{ID: tt.id, Type: "function", Function: model.ResponseToolCallFunction{Name: tt.toolName}}
			m := buildToolResponseMessage(tc, tt.content)
			if m["role"] != "tool" {
				t.Errorf("role %v", m["role"])
			}
			if m["tool_call_id"] != tt.id {
				t.Errorf("tool_call_id %v", m["tool_call_id"])
			}
			if m["name"] != tt.toolName {
				t.Errorf("name %v", m["name"])
			}
			if m["content"] != tt.content {
				t.Errorf("content %v", m["content"])
			}
		})
	}
}

func TestBuildChatRequest(t *testing.T) {
	conv := []model.D{{"role": "user", "content": "task"}}
	docs := []model.D{{"type": "function"}}

	tests := []struct {
		name          string
		llm           config.LLMConfig
		wantTopK      any
		wantMaxTokens int
		wantTemp      float64
		wantTopP      float64
	}{
		{"greedy defaults", config.LLMConfig{}, 1, config.DefaultLLMMaxOutputTokens, 0, 0},
		{"config values pass through", config.LLMConfig{Temperature: 0.7, TopP: 0.9, TopK: 5}, 5, config.DefaultLLMMaxOutputTokens, 0.7, 0.9},
		{"explicit top_k wins over greedy default", config.LLMConfig{Temperature: 0, TopK: 40}, 40, config.DefaultLLMMaxOutputTokens, 0, 0},
		{"top_k omitted when sampling without top_k", config.LLMConfig{Temperature: 0.8}, nil, config.DefaultLLMMaxOutputTokens, 0.8, 0},
		{"explicit top_k kept when sampling", config.LLMConfig{Temperature: 0.8, TopK: 20}, 20, config.DefaultLLMMaxOutputTokens, 0.8, 0},
		{"explicit max tokens win", config.LLMConfig{MaxOutputTokens: 512}, 1, 512, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := buildChatRequest(conv, docs, tt.llm)
			if req["top_k"] != tt.wantTopK {
				t.Errorf("top_k %v want %v", req["top_k"], tt.wantTopK)
			}
			if req["max_tokens"] != tt.wantMaxTokens {
				t.Errorf("max_tokens %v want %d", req["max_tokens"], tt.wantMaxTokens)
			}
			if _, ok := req["tool_choice"]; ok {
				t.Error("tool_choice must not be sent: the model decides when to call tools")
			}
			if req["temperature"] != tt.wantTemp {
				t.Errorf("temperature %v want %v", req["temperature"], tt.wantTemp)
			}
			if req["top_p"] != tt.wantTopP {
				t.Errorf("top_p %v want %v", req["top_p"], tt.wantTopP)
			}
			if req["parallel_tool_calls"] != true {
				t.Errorf("parallel_tool_calls %v want true", req["parallel_tool_calls"])
			}
			if req["reasoning_effort"] != model.ReasoningEffortMedium {
				t.Errorf("reasoning_effort %v want medium", req["reasoning_effort"])
			}
			msgs, _ := req["messages"].([]model.D)
			if len(msgs) != 1 || msgs[0]["content"] != "task" {
				t.Errorf("messages %v", req["messages"])
			}
			tools, _ := req["tools"].([]model.D)
			if len(tools) != 1 {
				t.Errorf("tools %v", req["tools"])
			}
		})
	}
}

func TestBuildAssistantMessageReasoning(t *testing.T) {
	tests := []struct {
		name          string
		sendReasoning []bool
		wantReasoning any
	}{
		{"included by default", nil, "because"},
		{"included when enabled", []bool{true}, "because"},
		{"omitted when disabled", []bool{false}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := buildAssistantMessage(&model.ResponseMessage{Content: "hi", Reasoning: "because"}, nil, tt.sendReasoning...)
			if m["reasoning_content"] != tt.wantReasoning {
				t.Errorf("reasoning_content %v want %v", m["reasoning_content"], tt.wantReasoning)
			}
			if m["content"] != "hi" {
				t.Errorf("content %v want hi", m["content"])
			}
		})
	}
}

func TestBuildLengthNudgeMessages(t *testing.T) {
	const nudgePrefix = "Your previous response hit the token limit"
	long := strings.Repeat("x", 2500)

	tests := []struct {
		name          string
		msg           *model.ResponseMessage
		wantLen       int
		wantRole      string
		wantContent   string
		wantReasoning string
	}{
		{
			name:     "empty message yields only the nudge",
			msg:      &model.ResponseMessage{},
			wantLen:  1,
			wantRole: "user",
		},
		{
			name:        "content is truncated into the replayed turn",
			msg:         &model.ResponseMessage{Content: long},
			wantLen:     2,
			wantRole:    "assistant",
			wantContent: textutil.Truncate(long, config.DefaultNudgeBytes),
		},
		{
			name:          "reasoning is replayed too",
			msg:           &model.ResponseMessage{Reasoning: "because"},
			wantLen:       2,
			wantRole:      "assistant",
			wantReasoning: "because",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgs := buildNudgeMessages(tt.msg, config.DefaultNudgeBytes, lengthNudge)
			if len(msgs) != tt.wantLen {
				t.Fatalf("msgs len %d want %d: %v", len(msgs), tt.wantLen, msgs)
			}
			if msgs[0]["role"] != tt.wantRole {
				t.Errorf("role %v want %v", msgs[0]["role"], tt.wantRole)
			}
			if tt.wantContent != "" && msgs[0]["content"] != tt.wantContent {
				t.Errorf("content len %d want %d", len(msgs[0]["content"].(string)), len(tt.wantContent))
			}
			if tt.wantReasoning != "" && msgs[0]["reasoning_content"] != tt.wantReasoning {
				t.Errorf("reasoning %v want %v", msgs[0]["reasoning_content"], tt.wantReasoning)
			}
			last := msgs[len(msgs)-1]
			if last["role"] != "user" {
				t.Errorf("last role %v want user", last["role"])
			}
			if nudge, _ := last["content"].(string); !strings.HasPrefix(nudge, nudgePrefix) {
				t.Errorf("nudge %q", nudge)
			}
		})
	}
}




func TestAssistantMessageShapeLogged(t *testing.T) {
	var buf bytes.Buffer
	mock := &mockKronk{
		contextWidth: 8192,
		responses: []model.ChatResponse{
			chatResp("", "", model.FinishReasonTool, []model.ResponseToolCall{
				{ID: "c1", Type: "function", Function: model.ResponseToolCallFunction{Name: "bogus_tool", Arguments: model.ToolCallArguments{}}},
			}, &model.Usage{TotalTokens: 100}),
			chatResp("done", "", model.FinishReasonStop, nil, &model.Usage{TotalTokens: 50}),
		},
	}
	agt := NewAgent(zerolog.New(&buf), mock, Options{
		MaxIterations: 3,
		ChatTimeout:   time.Second,
		TotalTimeout:  30 * time.Second,
		LLM:           defaultLLMConfig(),
		RunID:         "run-shape",
	})
	conf := config.Config{
		VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:  "m",
		Prompt: "task",
	}
	if _, err := agt.Execute(context.Background(), conf, newTestClient(t)); err != nil {
		t.Fatalf("execute: %v", err)
	}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["message"] != "assistant message" || rec["iter"] != float64(1) {
			continue
		}
		found = true
		if rec["calls"] != float64(1) {
			t.Errorf("calls %v want 1", rec["calls"])
		}
		if rec["clen"] != float64(0) || rec["rlen"] != float64(0) {
			t.Errorf("bare tool call must report zero text lengths, got %v", rec)
		}
	}
	if !found {
		t.Fatalf("no assistant message shape event for iteration 1:\n%s", buf.String())
	}
}

func TestExecuteReportsNoOutputWithoutError(t *testing.T) {
	mock := &mockKronk{responses: []model.ChatResponse{
		chatResp("", "", model.FinishReasonStop, nil, &model.Usage{TotalTokens: 1}),
	}}
	agt := NewAgent(zerolog.Nop(), mock, Options{
		MaxIterations: 1,
		ChatTimeout:   time.Second,
		TotalTimeout:  5 * time.Second,
		LLM:           defaultLLMConfig(),
		RunID:         "run-empty",
	})
	conf := config.Config{
		VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:  "m",
		Prompt: "task",
	}

	if _, err := agt.Execute(context.Background(), conf, newTestClient(t)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestExecute(t *testing.T) {
	tests := []struct {
		name      string
		mock      *mockKronk
		maxIter   int
		wantCalls int
		wantErr   bool
		llmCfg    *config.LLMConfig
		vm        config.VMConfig
		prompt    string
		verifyReq func(t *testing.T, reqs []model.D)
	}{
		{
			name:    "no tool calls gets a task_status nudge",
			mock:    &mockKronk{responses: []model.ChatResponse{chatResp("final answer", "thinking", model.FinishReasonStop, nil, &model.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3, TokensPerSecond: 10})}},
			maxIter: 5, wantCalls: 2,
			vm:     config.VMConfig{Name: "test-vm", CPU: 2, RAM: "4G", Disk: "20G"},
			prompt: "do nothing",
			verifyReq: func(t *testing.T, reqs []model.D) {
				if reqs[0]["temperature"] != 0.0 {
					t.Errorf("temperature %v", reqs[0]["temperature"])
				}
				if reqs[0]["top_p"] != 0.1 {
					t.Errorf("top_p %v", reqs[0]["top_p"])
				}
				if len(reqs) < 2 {
					t.Fatal("no nudge request after a finish without task_status")
				}
				msgs, _ := reqs[1]["messages"].([]model.D)
				if len(msgs) == 0 {
					t.Fatal("nudge request has no messages")
				}
				last := msgs[len(msgs)-1]
				if last["role"] != "user" {
					t.Errorf("nudge role %v want user", last["role"])
				}
				if content, _ := last["content"].(string); !strings.Contains(content, "task_status") {
					t.Errorf("nudge %q does not mention task_status", content)
				}
			},
		},
		{
			name:    "uses LLMConfig",
			mock:    &mockKronk{responses: []model.ChatResponse{chatResp("ok", "", model.FinishReasonStop, nil, nil)}},
			maxIter: 3, wantCalls: 2,
			llmCfg: &config.LLMConfig{Temperature: 0.7, TopP: 0.9, TopK: 5},
			verifyReq: func(t *testing.T, reqs []model.D) {
				if reqs[0]["temperature"] != 0.7 {
					t.Errorf("temperature %v want 0.7", reqs[0]["temperature"])
				}
				if reqs[0]["top_p"] != 0.9 {
					t.Errorf("top_p %v want 0.9", reqs[0]["top_p"])
				}
				if reqs[0]["top_k"] != 5 {
					t.Errorf("top_k %v want 5", reqs[0]["top_k"])
				}
			},
		},
		{
			name:    "empty choices breaks gracefully",
			mock:    &mockKronk{responses: []model.ChatResponse{{Choices: []model.Choice{}}}},
			maxIter: 3, wantCalls: 1,
		},
		{
			name: "respects maxIterations",
			mock: func() *mockKronk {
				tc := []model.ResponseToolCall{{ID: "c1", Type: "function", Function: model.ResponseToolCallFunction{Name: "vm_info", Arguments: model.ToolCallArguments{}}}}
				resp := chatResp("", "", model.FinishReasonTool, tc, nil)
				return &mockKronk{responses: []model.ChatResponse{resp, resp, resp, resp, resp}}
			}(),
			maxIter: 2, wantCalls: 2,
		},
		{
			name: "handles chat error",
			mock: &mockKronk{chatFunc: func(ctx context.Context, req model.D) (model.ChatResponse, error) {
				return model.ChatResponse{}, context.DeadlineExceeded
			}},
			maxIter: 3, wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			llmCfg := defaultLLMConfig()
			if tt.llmCfg != nil {
				llmCfg = *tt.llmCfg
			}
			agent := NewAgent(zerolog.Nop(), tt.mock, Options{
				MaxIterations: tt.maxIter,
				ChatTimeout:   time.Second,
				TotalTimeout:  5 * time.Second,
				LLM:           llmCfg,
				RunID:         "",
			})
			vm := config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"}
			if tt.vm.Name != "" {
				vm = tt.vm
			}
			prompt := "task"
			if tt.prompt != "" {
				prompt = tt.prompt
			}
			conf := config.Config{VM: vm, Prompt: prompt}
			cli := newTestClient(t)
			_, err := agent.Execute(context.Background(), conf, cli)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err %v wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && tt.mock.calls != tt.wantCalls {
				t.Fatalf("calls %d want %d", tt.mock.calls, tt.wantCalls)
			}
			if tt.verifyReq != nil && len(tt.mock.captured) > 0 {
				tt.verifyReq(t, tt.mock.captured)
			}
		})
	}
}

func TestExecuteReturnsTaskStatus(t *testing.T) {
	tests := []struct {
		name        string
		status      string
		reason      string
		wantOutcome runOutcome
	}{
		{"completed", "Completed", "kernel string verified", outcomeOK},
		{"failed", "Failed", "cli lacks the required flag", outcomeFailed},
		{"error", "Error", "permission denied", outcomeError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc := model.ResponseToolCall{ID: "c1", Type: "function", Function: model.ResponseToolCallFunction{
				Name:      "task_status",
				Arguments: model.ToolCallArguments{"status": tt.status, "reason": tt.reason},
			}}
			mock := &mockKronk{responses: []model.ChatResponse{
				chatResp("summary", "", model.FinishReasonTool, []model.ResponseToolCall{tc}, &model.Usage{TotalTokens: 100}),
			}}
			var buf bytes.Buffer
			agt := NewAgent(zerolog.New(&buf), mock, Options{
				MaxIterations: 5,
				ChatTimeout:   time.Second,
				TotalTimeout:  5 * time.Second,
				LLM:           defaultLLMConfig(),
				RunID:         "run-status",
			})
			conf := config.Config{
				VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
				Model:  "m",
				Prompt: "task",
			}

			got, err := agt.Execute(context.Background(), conf, newTestClient(t))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			want := TaskStatusOutcome{CompletedStatus: tt.status, Reason: tt.reason}
			if got == nil || *got != want {
				t.Fatalf("status = %+v, want %+v", got, want)
			}
			if !agt.done {
				t.Error("agent must stop after task_status")
			}
			if mock.calls != 1 {
				t.Errorf("chat calls = %d, want 1: task_status must end the run", mock.calls)
			}

			summary := onlyEvent(t, decodeEvents(t, &buf), eventRunSummary)
			if summary["status"] != tt.status {
				t.Errorf("run_summary status = %v, want %q", summary["status"], tt.status)
			}
			if summary["reason"] != tt.reason {
				t.Errorf("run_summary reason = %v, want %q", summary["reason"], tt.reason)
			}
			if summary["outcome"] != string(tt.wantOutcome) {
				t.Errorf("run_summary outcome = %v, want %q", summary["outcome"], tt.wantOutcome)
			}
		})
	}
}

func TestExecuteStopsAtTaskStatusInBatch(t *testing.T) {
	completed := model.ResponseToolCall{ID: "c1", Type: "function", Function: model.ResponseToolCallFunction{
		Name:      "task_status",
		Arguments: model.ToolCallArguments{"status": "Completed", "reason": "first status wins"},
	}}
	tests := []struct {
		name  string
		later model.ResponseToolCall
	}{
		{
			name: "later task_status ignored",
			later: model.ResponseToolCall{ID: "c2", Type: "function", Function: model.ResponseToolCallFunction{
				Name:      "task_status",
				Arguments: model.ToolCallArguments{"status": "Failed", "reason": "second status must not win"},
			}},
		},
		{
			name: "later tool call not executed",
			later: model.ResponseToolCall{ID: "c3", Type: "function", Function: model.ResponseToolCallFunction{
				Name:      "vm_info",
				Arguments: model.ToolCallArguments{},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockKronk{responses: []model.ChatResponse{
				chatResp("summary", "", model.FinishReasonTool, []model.ResponseToolCall{completed, tt.later}, &model.Usage{TotalTokens: 100}),
			}}
			agt := NewAgent(zerolog.Nop(), mock, Options{
				MaxIterations: 5,
				ChatTimeout:   time.Second,
				TotalTimeout:  5 * time.Second,
				LLM:           defaultLLMConfig(),
				RunID:         "run-status-batch",
			})
			conf := config.Config{
				VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
				Model:  "m",
				Prompt: "task",
			}

			got, err := agt.Execute(context.Background(), conf, newTestClient(t))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			want := TaskStatusOutcome{CompletedStatus: "Completed", Reason: "first status wins"}
			if got == nil || *got != want {
				t.Fatalf("status = %+v, want %+v", got, want)
			}
			if agt.totals.ToolCalls != 1 {
				t.Errorf("tool calls = %d, want 1: nothing after task_status may run", agt.totals.ToolCalls)
			}
			if mock.calls != 1 {
				t.Errorf("chat calls = %d, want 1: task_status must end the run", mock.calls)
			}
		})
	}
}

func TestExecuteRetriesAfterInvalidTaskStatus(t *testing.T) {
	bad := model.ResponseToolCall{ID: "c1", Type: "function", Function: model.ResponseToolCallFunction{
		Name:      "task_status",
		Arguments: model.ToolCallArguments{"status": "Done", "reason": "x"},
	}}
	good := model.ResponseToolCall{ID: "c2", Type: "function", Function: model.ResponseToolCallFunction{
		Name:      "task_status",
		Arguments: model.ToolCallArguments{"status": "Completed", "reason": "all checks passed"},
	}}
	mock := &mockKronk{responses: []model.ChatResponse{
		chatResp("", "", model.FinishReasonTool, []model.ResponseToolCall{bad}, &model.Usage{TotalTokens: 100}),
		chatResp("", "", model.FinishReasonTool, []model.ResponseToolCall{good}, &model.Usage{TotalTokens: 100}),
	}}
	agt := NewAgent(zerolog.Nop(), mock, Options{
		MaxIterations: 5,
		ChatTimeout:   time.Second,
		TotalTimeout:  5 * time.Second,
		LLM:           defaultLLMConfig(),
		RunID:         "run-status-retry",
	})
	conf := config.Config{
		VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:  "m",
		Prompt: "task",
	}

	got, err := agt.Execute(context.Background(), conf, newTestClient(t))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got == nil || got.CompletedStatus != "Completed" || got.Reason != "all checks passed" {
		t.Fatalf("status = %+v, want the retried Completed status", got)
	}
	if mock.calls != 2 {
		t.Errorf("chat calls = %d, want 2: an invalid status must not end the run", mock.calls)
	}
}


func TestExecuteLogsTokenUsageAndIterationEvents(t *testing.T) {
	var buf bytes.Buffer
	mock := &mockKronk{
		contextWidth: 32768,
		responses: []model.ChatResponse{chatResp("done", "", model.FinishReasonStop, nil,
			&model.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120, TokensPerSecond: 8.5})},
	}
	agent := NewAgent(zerolog.New(&buf), mock, Options{
		MaxIterations: 1,
		ChatTimeout:   time.Second,
		TotalTimeout:  5 * time.Second,
		LLM:           defaultLLMConfig(),
		RunID:         "0198f0c1-2a3b-7c4d-8e5f-60718293a4b5",
	})
	conf := config.Config{VM: config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"}, Prompt: "task"}
	ctx := context.Background()
	if _, err := agent.Execute(ctx, conf, newTestClient(t)); err != nil {
		t.Fatalf("execute: %v", err)
	}

	events := decodeEvents(t, &buf)
	usage := onlyEvent(t, events, eventTokenUsage)
	for key, want := range map[string]float64{
		"iter": 1, "prompt_tokens": 100, "completion_tokens": 20,
		"total_tokens": 120, "tps": 8.5, "ctx_window": 32768,
	} {
		if got := usage[key]; got != want {
			t.Errorf("token_usage %s = %v, want %v", key, got, want)
		}
	}

	iteration := onlyEvent(t, events, eventIteration)
	if iteration["iter"] != float64(1) {
		t.Errorf("iteration iter = %v, want 1", iteration["iter"])
	}
	for _, key := range []string{"dur_ms", "chat_ms", "tool_ms"} {
		if _, ok := iteration[key].(float64); !ok {
			t.Errorf("iteration event missing numeric %q: %v", key, iteration)
		}
	}

	onlyEvent(t, events, eventRunSummary)
}



// would leave a dashboard column blank, which reads as broken logging.
// "the loop would have run again".
func TestRunOutcomeDistinguishesCleanFinishFromExhaustion(t *testing.T) {
	tests := []struct {
		name         string
		maxIter      int
		finishReason string
		toolCalls    []model.ResponseToolCall
		want         runOutcome
	}{
		{"clean finish below the cap", 5, model.FinishReasonTool, []model.ResponseToolCall{
			{ID: "c1", Type: "function", Function: model.ResponseToolCallFunction{
				Name:      "task_status",
				Arguments: model.ToolCallArguments{"status": "Completed", "reason": "done"},
			}},
		}, outcomeOK},
		{"clean finish on the last permitted iteration", 1, model.FinishReasonTool, []model.ResponseToolCall{
			{ID: "c1", Type: "function", Function: model.ResponseToolCallFunction{
				Name:      "task_status",
				Arguments: model.ToolCallArguments{"status": "Completed", "reason": "done"},
			}},
		}, outcomeOK},
		{"cap reached while still calling tools", 1, model.FinishReasonTool, []model.ResponseToolCall{
			{ID: "c1", Type: "function", Function: model.ResponseToolCallFunction{Name: "vm_info", Arguments: model.ToolCallArguments{}}},
		}, outcomeBudgetExhausted},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockKronk{contextWidth: 32768, responses: []model.ChatResponse{
				chatResp("", "", tt.finishReason, tt.toolCalls, &model.Usage{TotalTokens: 100}),
			}}
			var buf bytes.Buffer
			a := NewAgent(zerolog.New(&buf), mock, Options{
				MaxIterations: tt.maxIter,
				ChatTimeout:   time.Second,
				TotalTimeout:  30 * time.Second,
				LLM:           defaultLLMConfig(),
				RunID:         "01J9",
			})
			conf := config.Config{VM: config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"}, Prompt: "task"}
			if _, err := a.Execute(t.Context(), conf, newTestClient(t)); err != nil {
				t.Fatalf("Execute: %v", err)
			}

			summary := onlyEvent(t, decodeEvents(t, &buf), eventRunSummary)
			if got := runOutcome(summary["outcome"].(string)); got != tt.want {
				t.Errorf("outcome = %q, want %q", got, tt.want)
			}
		})
	}
}
