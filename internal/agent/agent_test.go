package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ardanlabs/kronk/sdk/kronk/model"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/trollLemon/MPHarness/internal/config"
	"github.com/trollLemon/MPHarness/internal/multipass"
)

type mockKronk struct {
	responses    []model.ChatResponse
	errs         []error
	calls        int
	captured     []model.D
	chatFunc     func(context.Context, model.D) (model.ChatResponse, error)
	contextWidth int
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
		{"long truncated", "hello world", 5, "hello…(truncated, 5 of 11 bytes)"},
		{"empty", "", 5, ""},
		{"exact", "hello", 5, "hello"},
		{"zero means no limit", "hello", 0, "hello"},
		{"negative means no limit", "hello", -1, "hello"},
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
	for _, want := range []string{"multipass_exec", "multipass_info"} {
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
		name        string
		resp        model.ChatResponse
		wantBreak   bool
		wantFR      string
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
		name     string
		calls    []model.ResponseToolCall
		wantID   string
		wantName string
	}{
		{
			name:   "single exec",
			calls:  []model.ResponseToolCall{{ID: "call-1", Type: "function", Function: model.ResponseToolCallFunction{Name: "multipass_exec", Arguments: model.ToolCallArguments{"command": "ls"}}}},
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

func TestBuildToolResponseMessage(t *testing.T) {
	tc := model.ResponseToolCall{ID: "call-1", Type: "function", Function: model.ResponseToolCallFunction{Name: "multipass_exec"}}
	m := buildToolResponseMessage(tc, "out")
	if m["role"] != "tool" {
		t.Errorf("role %v", m["role"])
	}
	if m["tool_call_id"] != "call-1" {
		t.Errorf("tool_call_id %v", m["tool_call_id"])
	}
	if m["name"] != "multipass_exec" {
		t.Errorf("name %v", m["name"])
	}
	if m["content"] != "out" {
		t.Errorf("content %v", m["content"])
	}
}

func TestBuildChatRequest(t *testing.T) {
	conv := []model.D{{"role": "user", "content": "task"}}
	docs := []model.D{{"type": "function"}}

	t.Run("top_k defaults to 1", func(t *testing.T) {
		req := buildChatRequest(conv, docs, LLMConfig{ToolChoice: "auto"})
		if req["top_k"] != 1 {
			t.Errorf("top_k %v want 1", req["top_k"])
		}
		if req["parallel_tool_calls"] != false {
			t.Errorf("parallel_tool_calls %v want false", req["parallel_tool_calls"])
		}
		if req["tool_choice"] != "auto" {
			t.Errorf("tool_choice %v", req["tool_choice"])
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

	t.Run("config values pass through", func(t *testing.T) {
		req := buildChatRequest(conv, docs, LLMConfig{Temperature: 0.7, TopP: 0.9, TopK: 5, ToolChoice: "required"})
		if req["temperature"] != 0.7 {
			t.Errorf("temperature %v want 0.7", req["temperature"])
		}
		if req["top_p"] != 0.9 {
			t.Errorf("top_p %v want 0.9", req["top_p"])
		}
		if req["top_k"] != 5 {
			t.Errorf("top_k %v want 5", req["top_k"])
		}
		if req["tool_choice"] != "required" {
			t.Errorf("tool_choice %v want required", req["tool_choice"])
		}
	})
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
			wantContent: truncate(long, config.DefaultNudgeBytes),
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
			msgs := buildLengthNudgeMessages(tt.msg, config.DefaultNudgeBytes)
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

func TestLogModelOutput(t *testing.T) {
	ctx := context.Background()
	log := zerolog.Nop()

	if got := logModelOutput(ctx, log, 0, &model.ResponseMessage{Content: "answer"}, config.DefaultLogContentBytes); got != "answer" {
		t.Errorf("content %q want answer", got)
	}
	if got := logModelOutput(ctx, log, 0, &model.ResponseMessage{Reasoning: "thinking"}, config.DefaultLogContentBytes); got != "" {
		t.Errorf("content %q want empty", got)
	}
	if got := logModelOutput(ctx, log, 0, &model.ResponseMessage{}, config.DefaultLogContentBytes); got != "" {
		t.Errorf("content %q want empty", got)
	}
}

// TestLogModelOutputKeys pins the field names a log query depends on: both
// parts share one message and one text key, split only by part.
func TestLogModelOutputKeys(t *testing.T) {
	var buf bytes.Buffer
	log := zerolog.New(&buf)

	if got := logModelOutput(context.Background(), log, 1, &model.ResponseMessage{
		Reasoning: "thinking",
		Content:   "answer",
	}, config.DefaultLogContentBytes); got != "answer" {
		t.Fatalf("content %q want answer", got)
	}

	want := []struct{ part, text string }{
		{"reasoning", "thinking"},
		{"content", "answer"},
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != len(want) {
		t.Fatalf("got %d lines want %d: %s", len(lines), len(want), buf.String())
	}
	for i, w := range want {
		var rec map[string]any
		if err := json.Unmarshal([]byte(lines[i]), &rec); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if rec["message"] != "model output" {
			t.Errorf("line %d message %v want model output", i, rec["message"])
		}
		if rec["part"] != w.part {
			t.Errorf("line %d part %v want %s", i, rec["part"], w.part)
		}
		if rec["text"] != w.text {
			t.Errorf("line %d text %v want %s", i, rec["text"], w.text)
		}
		if rec["iteration"] != float64(2) {
			t.Errorf("line %d iteration %v want 2", i, rec["iteration"])
		}
	}
}

func TestLogModelOutputTruncatesTextKey(t *testing.T) {
	var buf bytes.Buffer
	log := zerolog.New(&buf)

	logModelOutput(context.Background(), log, 0, &model.ResponseMessage{Content: "hello world"}, 5)

	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rec["text"] != truncate("hello world", 5) {
		t.Errorf("text %v not truncated", rec["text"])
	}
}

func TestExecute(t *testing.T) {
	tests := []struct {
		name      string
		mock      *mockKronk
		maxIter   int
		wantCalls int
		wantErr   bool
		verifyReq func(t *testing.T, reqs []model.D)
	}{
		{
			name:    "no tool calls success",
			mock:    &mockKronk{responses: []model.ChatResponse{chatResp("final answer", "thinking", model.FinishReasonStop, nil, &model.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3, TokensPerSecond: 10})}},
			maxIter: 5, wantCalls: 1,
			verifyReq: func(t *testing.T, reqs []model.D) {
				if reqs[0]["temperature"] != 0.0 {
					t.Errorf("temperature %v", reqs[0]["temperature"])
				}
				if reqs[0]["top_p"] != 0.1 {
					t.Errorf("top_p %v", reqs[0]["top_p"])
				}
				if reqs[0]["tool_choice"] != "auto" {
					t.Errorf("tool_choice %v", reqs[0]["tool_choice"])
				}
			},
		},
		{
			name:    "uses LLMConfig",
			mock:    &mockKronk{responses: []model.ChatResponse{chatResp("ok", "", model.FinishReasonStop, nil, nil)}},
			maxIter: 3, wantCalls: 1,
			verifyReq: func(t *testing.T, reqs []model.D) {
				// this case will be handled with custom LLMConfig below; placeholder
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
				tc := []model.ResponseToolCall{{ID: "c1", Type: "function", Function: model.ResponseToolCallFunction{Name: "multipass_info", Arguments: model.ToolCallArguments{}}}}
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
			cli := multipass.New(zerolog.Nop())
			err := agent.Execute(context.Background(), conf, cli)
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
				if req["temperature"] != 0.7 {
					t.Errorf("temperature %v want 0.7", req["temperature"])
				}
				if req["top_p"] != 0.9 {
					t.Errorf("top_p %v want 0.9", req["top_p"])
				}
				if req["top_k"] != 5 {
					t.Errorf("top_k %v want 5", req["top_k"])
				}
				if req["tool_choice"] != "required" {
					t.Errorf("tool_choice %v want required", req["tool_choice"])
				}
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

func TestExecuteTagsMetricsWithRunID(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		otel.SetMeterProvider(prev)
		tokensHist, tpsHist, iterationsCounter = nil, nil, nil
		toolDurationHist, toolCallsCounter, toolFailuresCounter = nil, nil, nil
		contextWindowGauge, contextTokensGauge = nil, nil
	})
	mock := &mockKronk{
		contextWidth: 32768,
		responses: []model.ChatResponse{chatResp("done", "", model.FinishReasonStop, nil,
			&model.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120, TokensPerSecond: 8.5})},
	}
	agent := NewAgent(zerolog.Nop(), mock, 2, time.Second, 5*time.Second, defaultLLMConfig())
	conf := config.Config{VM: config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"}, Prompt: "task"}
	if err := agent.Execute(context.Background(), conf, multipass.New(zerolog.Nop())); err != nil {
		t.Fatalf("execute: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}

	seen := map[string]bool{}
	runIDs := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			seen[m.Name] = true
			attrs := dataPointAttrs(m.Data)
			if len(attrs) == 0 {
				t.Errorf("metric %q produced no data points", m.Name)
				continue
			}
			for _, set := range attrs {
				v, ok := set.Value("mph.run.id")
				if !ok || v.AsString() == "" {
					t.Fatalf("metric %q missing mph.run.id attribute (attrs: %s)", m.Name, set.Encoded(attribute.DefaultEncoder()))
					continue
				}
				runIDs[v.AsString()] = true
			}
		}
	}

	for _, name := range []string{
		"mph.tokens", "mph.tokens_per_second", "mph.iterations",
		"mph.context.window", "mph.context.tokens",
	} {
		if !seen[name] {
			t.Errorf("metric %q was never recorded", name)
		}
	}
	if len(runIDs) != 1 {
		t.Fatalf("want exactly one run id across all metrics, got %d: %v", len(runIDs), runIDs)
	}
	for id := range runIDs {
		if _, err := uuid.Parse(id); err != nil {
			t.Errorf("run id %q is not a uuid: %v", id, err)
		}
	}
}

func TestRunIDFromContext(t *testing.T) {
	if got := runIDFromContext(context.Background()); got != "" {
		t.Errorf("runIDFromContext on bare ctx = %q, want empty", got)
	}

	const id = "0198f0c1-2a3b-7c4d-8e5f-60718293a4b5"
	if got := runIDFromContext(withRunID(context.Background(), id)); got != id {
		t.Errorf("runIDFromContext = %q, want %q", got, id)
	}
}

func TestRunSpanAttrs(t *testing.T) {
	iter := attribute.Int("mph.iteration", 3)

	t.Run("omits run id when unset", func(t *testing.T) {
		got := runSpanAttrs(context.Background(), iter)
		if len(got) != 1 {
			t.Fatalf("got %d attrs, want 1: %v", len(got), got)
		}
		if key := string(got[0].Key); key != "mph.iteration" {
			t.Errorf("got %d attrs, want only the extra attr on a bare ctx: %v", len(got), got)
		}
	})

	t.Run("adds run id when set", func(t *testing.T) {
		const id = "0198f0c1-2a3b-7c4d-8e5f-60718293a4b5"
		got := runSpanAttrs(withRunID(context.Background(), id), iter)
		if len(got) != 2 {
			t.Fatalf("got %d attrs, want 2: %v", len(got), got)
		}
		if key := string(got[1].Key); key != "mph.run.id" {
			t.Fatalf("second attr key = %q, want mph.run.id", key)
		}
		if got[1].Value.AsString() != id {
			t.Errorf("run id = %q, want %q", got[1].Value.AsString(), id)
		}
	})
}

func dataPointAttrs(data metricdata.Aggregation) []attribute.Set {
	switch a := data.(type) {
	case metricdata.Histogram[int64]:
		out := make([]attribute.Set, 0, len(a.DataPoints))
		for _, dp := range a.DataPoints {
			out = append(out, dp.Attributes)
		}
		return out
	case metricdata.Histogram[float64]:
		out := make([]attribute.Set, 0, len(a.DataPoints))
		for _, dp := range a.DataPoints {
			out = append(out, dp.Attributes)
		}
		return out
	case metricdata.Sum[int64]:
		out := make([]attribute.Set, 0, len(a.DataPoints))
		for _, dp := range a.DataPoints {
			out = append(out, dp.Attributes)
		}
		return out
	case metricdata.Sum[float64]:
		out := make([]attribute.Set, 0, len(a.DataPoints))
		for _, dp := range a.DataPoints {
			out = append(out, dp.Attributes)
		}
		return out
	case metricdata.Gauge[int64]:
		out := make([]attribute.Set, 0, len(a.DataPoints))
		for _, dp := range a.DataPoints {
			out = append(out, dp.Attributes)
		}
		return out
	}
	return nil
}
