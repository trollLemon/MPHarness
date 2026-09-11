package agent

import (
	"context"
	"testing"
	"time"

	"github.com/ardanlabs/kronk/sdk/kronk/model"
	"github.com/rs/zerolog"

	"github.com/trollLemon/MPHarness/internal/config"
)

type mockKronk struct {
	responses []model.ChatResponse
	errs      []error
	calls     int
	captured  []model.D
	chatFunc  func(context.Context, model.D) (model.ChatResponse, error)
}

func (m *mockKronk) Chat(ctx context.Context, req model.D) (model.ChatResponse, error) {
	if m.chatFunc != nil {
		return m.chatFunc(ctx, req)
	}
	idx := m.calls
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

func defaultLLMConfig() LLMConfig {
	return LLMConfig{Temperature: 0.0, TopP: 0.1, TopK: 1, ToolChoice: "auto"}
}

func TestLLMConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  LLMConfig
		want LLMConfig
	}{
		{
			name: "defaults when empty",
			cfg:  LLMConfig{},
			want: LLMConfig{ToolChoice: "auto"},
		},
		{
			name: "passthrough preserves values",
			cfg:  LLMConfig{Temperature: 0.7, TopP: 0.9, TopK: 5, ToolChoice: "required"},
			want: LLMConfig{Temperature: 0.7, TopP: 0.9, TopK: 5, ToolChoice: "required"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewAgent(zerolog.Nop(), &mockKronk{}, 3, time.Second, 5*time.Second, tt.cfg)
			if a.llmConfig.Temperature != tt.want.Temperature || a.llmConfig.TopP != tt.want.TopP || a.llmConfig.TopK != tt.want.TopK || a.llmConfig.ToolChoice != tt.want.ToolChoice {
				t.Fatalf("got %+v want %+v", a.llmConfig, tt.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		s    string
		n    int
		want string
	}{
		{"short unchanged", "hello", 10, "hello"},
		{"long truncated", "hello world", 5, "hello…(truncated)"},
		{"empty", "", 5, ""},
		{"exact", "hello", 5, "hello"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := truncate(tt.s, tt.n); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestBuildUserPrompt(t *testing.T) {
	tests := []struct {
		name string
		conf config.Config
		want []string
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
			name: "no vm details leaked",
			conf: config.Config{VM: config.VMConfig{Name: "secret-vm", CPU: 8, RAM: "16G", Disk: "100G", Image: "noble"}, Prompt: "task"},
			want: []string{"task", "Allowed commands"},
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
			if tt.name == "no vm details leaked" {
				for _, leaked := range []string{"secret-vm", "16G", "100G", "noble", "CPUs", "Memory", "Disk", "VM Configuration"} {
					if contains(p, leaked) {
						t.Errorf("prompt should not contain VM detail %q, got %q", leaked, p)
					}
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
	docs := buildToolDocuments()
	if len(docs) == 0 {
		t.Fatalf("no tool docs")
	}
	names := map[string]bool{}
	for _, d := range docs {
		fn, _ := d["function"].(model.D)
		name, _ := fn["name"].(string)
		names[name] = true
	}
	for _, want := range []string{"multipass_exists", "multipass_launch", "multipass_exec", "multipass_start", "multipass_stop", "multipass_delete"} {
		if !names[want] {
			t.Errorf("missing tool %q", want)
		}
	}
}

func TestBuildInitialConversation(t *testing.T) {
	conf := config.Config{VM: config.VMConfig{Name: "vm1", CPU: 1, RAM: "1G", Disk: "10G"}, Prompt: "task"}
	conv := buildInitialConversation(conf)
	if len(conv) != 2 {
		t.Fatalf("conv len %d want 2", len(conv))
	}
	if conv[0]["role"] != "system" {
		t.Errorf("first role %v want system", conv[0]["role"])
	}
	if conv[1]["role"] != "user" {
		t.Errorf("second role %v want user", conv[1]["role"])
	}
}

func TestExtractAssistantMessage(t *testing.T) {
	tests := []struct {
		name       string
		resp       model.ChatResponse
		wantBreak  bool
		wantFR     string
		wantContent string
		wantReason  string
	}{
		{
			name:      "no choices",
			resp:      model.ChatResponse{Choices: []model.Choice{}},
			wantBreak: true,
		},
		{
			name:      "nil message and delta",
			resp:      model.ChatResponse{Choices: []model.Choice{{Message: nil, Delta: nil, FinishReasonPtr: strPtr(model.FinishReasonStop)}}},
			wantBreak: true,
			wantFR:    model.FinishReasonStop,
		},
		{
			name:        "normal message",
			resp:        chatResp("hello", "reason", model.FinishReasonTool, nil, nil),
			wantBreak:   false,
			wantFR:      model.FinishReasonTool,
			wantContent: "hello",
			wantReason:  "reason",
		},
		{
			name:        "delta fallback",
			resp:        model.ChatResponse{Choices: []model.Choice{{Message: nil, Delta: &model.ResponseMessage{Content: "delta content"}, FinishReasonPtr: strPtr(model.FinishReasonStop)}}},
			wantBreak:   false,
			wantFR:      model.FinishReasonStop,
			wantContent: "delta content",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewAgent(zerolog.Nop(), &mockKronk{}, 3, time.Second, 5*time.Second, defaultLLMConfig())
			msg, fr, shouldBreak := a.extractAssistantMessage(tt.resp)
			if shouldBreak != tt.wantBreak {
				t.Fatalf("break %v want %v", shouldBreak, tt.wantBreak)
			}
			if fr != tt.wantFR {
				t.Fatalf("finish reason %q want %q", fr, tt.wantFR)
			}
			if !tt.wantBreak {
				if msg.Content != tt.wantContent {
					t.Fatalf("content %q want %q", msg.Content, tt.wantContent)
				}
				if msg.Reasoning != tt.wantReason {
					t.Fatalf("reasoning %q want %q", msg.Reasoning, tt.wantReason)
				}
			}
		})
	}
}

func TestShouldTerminateWithoutToolCalls(t *testing.T) {
	tests := []struct {
		name   string
		calls  []model.ResponseToolCall
		reason string
		want   bool
	}{
		{"with tool calls", []model.ResponseToolCall{{ID: "1", Function: model.ResponseToolCallFunction{Name: "multipass_exec"}}}, model.FinishReasonTool, false},
		{"stop without tools", nil, model.FinishReasonStop, true},
		{"length without tools", nil, model.FinishReasonLength, true},
		{"empty finish", nil, "", true},
		{"unknown without tools", nil, "unknown", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewAgent(zerolog.Nop(), &mockKronk{}, 3, time.Second, 5*time.Second, defaultLLMConfig())
			if got := a.shouldTerminateWithoutToolCalls(tt.calls, tt.reason, 0); got != tt.want {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}

func TestBuildToolCallDocs(t *testing.T) {
	tests := []struct {
		name string
		calls []model.ResponseToolCall
		wantID string
		wantName string
	}{
		{
			name: "single exec",
			calls: []model.ResponseToolCall{{ID: "call-1", Type: "function", Function: model.ResponseToolCallFunction{Name: "multipass_exec", Arguments: model.ToolCallArguments{"command": "ls"}}}},
			wantID: "call-1", wantName: "multipass_exec",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewAgent(zerolog.Nop(), &mockKronk{}, 3, time.Second, 5*time.Second, defaultLLMConfig())
			docs := a.buildToolCallDocs(tt.calls, 0)
			if len(docs) != 1 {
				t.Fatalf("docs len %d", len(docs))
			}
			if docs[0]["id"] != tt.wantID {
				t.Errorf("id %v want %v", docs[0]["id"], tt.wantID)
			}
			fn, _ := docs[0]["function"].(model.D)
			if fn["name"] != tt.wantName {
				t.Errorf("name %v want %v", fn["name"], tt.wantName)
			}
		})
	}
}

func TestBuildAssistantMessageAndAppend(t *testing.T) {
	msg := &model.ResponseMessage{Content: "hi", Reasoning: "think"}
	docs := []model.D{{"id": "1"}}
	am := buildAssistantMessage(msg, docs)
	if am["role"] != "assistant" {
		t.Errorf("role %v", am["role"])
	}
	if am["content"] != "hi" {
		t.Errorf("content %v", am["content"])
	}
	if am["reasoning_content"] != "think" {
		t.Errorf("reasoning %v", am["reasoning_content"])
	}
	conv := []model.D{{"role": "system"}}
	conv2 := appendToConversation(conv, am)
	if len(conv2) != 2 {
		t.Fatalf("append len %d", len(conv2))
	}
}

func TestExecute(t *testing.T) {
	tests := []struct {
		name       string
		mock       *mockKronk
		maxIter    int
		wantCalls  int
		wantErr    bool
		verifyReq  func(t *testing.T, reqs []model.D)
	}{
		{
			name: "no tool calls success",
			mock: &mockKronk{responses: []model.ChatResponse{chatResp("final answer", "thinking", model.FinishReasonStop, nil, &model.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3, TokensPerSecond: 10})}},
			maxIter: 5, wantCalls: 1,
			verifyReq: func(t *testing.T, reqs []model.D) {
				if reqs[0]["temperature"] != 0.0 { t.Errorf("temperature %v", reqs[0]["temperature"]) }
				if reqs[0]["top_p"] != 0.1 { t.Errorf("top_p %v", reqs[0]["top_p"]) }
				if reqs[0]["tool_choice"] != "auto" { t.Errorf("tool_choice %v", reqs[0]["tool_choice"]) }
			},
		},
		{
			name: "uses LLMConfig",
			mock: &mockKronk{responses: []model.ChatResponse{chatResp("ok", "", model.FinishReasonStop, nil, nil)}},
			maxIter: 3, wantCalls: 1,
			verifyReq: func(t *testing.T, reqs []model.D) {
				// this case will be handled with custom LLMConfig below; placeholder
			},
		},
		{
			name: "empty choices breaks gracefully",
			mock: &mockKronk{responses: []model.ChatResponse{{Choices: []model.Choice{}}}},
			maxIter: 3, wantCalls: 1,
		},
		{
			name: "respects maxIterations",
			mock: func() *mockKronk {
				tc := []model.ResponseToolCall{{ID: "c1", Type: "function", Function: model.ResponseToolCallFunction{Name: "multipass_exists", Arguments: model.ToolCallArguments{}}}}
				resp := chatResp("", "", model.FinishReasonTool, tc, nil)
				return &mockKronk{responses: []model.ChatResponse{resp, resp, resp, resp, resp}}
			}(),
			maxIter: 2, wantCalls: 2,
		},
		{
			name: "handles chat error",
			mock: &mockKronk{chatFunc: func(ctx context.Context, req model.D) (model.ChatResponse, error) { return model.ChatResponse{}, context.DeadlineExceeded }},
			maxIter: 3, wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := zerolog.Nop()
			// special handling for LLMConfig test
			llmCfg := defaultLLMConfig()
			if tt.name == "uses LLMConfig" {
				llmCfg = LLMConfig{Temperature: 0.7, TopP: 0.9, TopK: 5, ToolChoice: "required"}
			}
			agent := NewAgent(log, tt.mock, tt.maxIter, time.Second, 5*time.Second, llmCfg)
			conf := config.Config{VM: config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"}, Prompt: "task"}
			// adjust VM name for first test
			if tt.name == "no tool calls success" {
				conf.VM = config.VMConfig{Name: "test-vm", CPU: 2, RAM: "4G", Disk: "20G"}
				conf.Prompt = "do nothing"
			}
			err := agent.Execute(conf)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err %v wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && tt.mock.calls != tt.wantCalls {
				t.Fatalf("calls %d want %d", tt.mock.calls, tt.wantCalls)
			}
			if tt.verifyReq != nil && len(tt.mock.captured) > 0 {
				tt.verifyReq(t, tt.mock.captured)
			}
			if tt.name == "uses LLMConfig" && len(tt.mock.captured) > 0 {
				req := tt.mock.captured[0]
				if req["temperature"] != 0.7 { t.Errorf("temperature %v want 0.7", req["temperature"]) }
				if req["top_p"] != 0.9 { t.Errorf("top_p %v want 0.9", req["top_p"]) }
				if req["top_k"] != 5 { t.Errorf("top_k %v want 5", req["top_k"]) }
				if req["tool_choice"] != "required" { t.Errorf("tool_choice %v want required", req["tool_choice"]) }
			}
		})
	}
}

func TestCallChatUsesTimeout(t *testing.T) {
	log := zerolog.Nop()
	called := false
	mock := &mockKronk{
		chatFunc: func(ctx context.Context, req model.D) (model.ChatResponse, error) {
			called = true
			if _, ok := ctx.Deadline(); !ok {
				t.Errorf("expected deadline from chatTimeout")
			}
			return chatResp("ok", "", model.FinishReasonStop, nil, nil), nil
		},
	}
	agent := NewAgent(log, mock, 3, 2*time.Second, 5*time.Second, defaultLLMConfig())
	_, err := agent.callChat(context.Background(), model.D{"messages": []model.D{}}, 0)
	if err != nil {
		t.Fatalf("callChat failed: %v", err)
	}
	if !called {
		t.Fatalf("mock not called")
	}
}
