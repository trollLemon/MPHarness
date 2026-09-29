package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ardanlabs/kronk/sdk/kronk/model"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/trollLemon/MPHarness/internal/config"
	"github.com/trollLemon/MPHarness/internal/multipass"
	"github.com/trollLemon/MPHarness/internal/output"
)

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
		return chatResp("work summary", "", model.FinishReasonStop, nil, nil), nil
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
			a := NewAgent(zerolog.Nop(), &mockKronk{}, 3, time.Second, 5*time.Second, tt.cfg, "")
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
		{"rune boundary never splits", "aaé", 3, "aa…(truncated, 3 of 4 bytes)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncate(tt.s, tt.n)
			if !utf8.ValidString(got) {
				t.Fatalf("truncate produced invalid UTF-8: %q", got)
			}
			if got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestToolResultBudgetBytes(t *testing.T) {
	tests := []struct {
		name          string
		cfg           config.Config
		contextWindow int
		want          int
	}{
		{"derived from 8k window", config.Config{}, 8192, 4096},
		{"derived from 32k window", config.Config{}, 32768, 16384},
		{"clamped to ceiling on 128k window", config.Config{}, 131072, maxToolResultBytes},
		{"clamped to floor on tiny window", config.Config{}, 512, minToolResultBytes},
		{"unknown window falls back", config.Config{}, 0, 4096},
		{
			name:          "explicit override wins",
			cfg:           config.Config{Agent: config.AgentConfig{MaxOutputBytes: 1234}},
			contextWindow: 8192,
			want:          1234,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toolResultBudgetBytes(tt.cfg, tt.contextWindow); got != tt.want {
				t.Errorf("got %d want %d", got, tt.want)
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
		{"base tools", config.Config{}, []string{"multipass_exec", "multipass_info"}, []string{"output_search", "output_read"}},
		{"output tools when enabled", config.Config{Output: config.OutputConfig{Enabled: true}}, []string{"multipass_exec", "multipass_info", "output_search", "output_read"}, nil},
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
			a := NewAgent(zerolog.Nop(), &mockKronk{}, 3, time.Second, 5*time.Second, defaultLLMConfig(), "")
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
			a := NewAgent(zerolog.Nop(), &mockKronk{}, 3, time.Second, 5*time.Second, defaultLLMConfig(), "")
			if got := a.shouldTerminateWithoutToolCalls(tt.calls, tt.reason, 0); got != tt.want {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}

func TestBuildToolCallDocs(t *testing.T) {
	tests := []struct {
		name      string
		calls     []model.ResponseToolCall
		wantIDs   []string
		wantNames []string
	}{
		{
			name:      "single exec",
			calls:     []model.ResponseToolCall{{ID: "call-1", Type: "function", Function: model.ResponseToolCallFunction{Name: "multipass_exec", Arguments: model.ToolCallArguments{"command": "ls"}}}},
			wantIDs:   []string{"call-1"},
			wantNames: []string{"multipass_exec"},
		},
		{
			name: "batched calls keep order",
			calls: []model.ResponseToolCall{
				{ID: "call-1", Type: "function", Function: model.ResponseToolCallFunction{Name: "multipass_exec", Arguments: model.ToolCallArguments{"command": "ls"}}},
				{ID: "call-2", Type: "function", Function: model.ResponseToolCallFunction{Name: "multipass_info", Arguments: model.ToolCallArguments{}}},
			},
			wantIDs:   []string{"call-1", "call-2"},
			wantNames: []string{"multipass_exec", "multipass_info"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewAgent(zerolog.Nop(), &mockKronk{}, 3, time.Second, 5*time.Second, defaultLLMConfig(), "")
			docs := a.buildToolCallDocs(tt.calls, 0)
			if len(docs) != len(tt.wantIDs) {
				t.Fatalf("docs len %d want %d", len(docs), len(tt.wantIDs))
			}
			for i := range docs {
				if docs[i]["id"] != tt.wantIDs[i] {
					t.Errorf("id %v want %v", docs[i]["id"], tt.wantIDs[i])
				}
				fn, _ := docs[i]["function"].(model.D)
				if fn["name"] != tt.wantNames[i] {
					t.Errorf("name %v want %v", fn["name"], tt.wantNames[i])
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
			conv2 := appendToConversation([]model.D{{"role": "system"}}, am)
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
		{"exec result", "call-1", "multipass_exec", "out"},
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
		llm           LLMConfig
		wantTopK      any
		wantMaxTokens int
		wantChoice    string
		wantTemp      float64
		wantTopP      float64
	}{
		{"greedy defaults", LLMConfig{ToolChoice: "auto"}, 1, DefaultMaxOutputTokens, "auto", 0, 0},
		{"config values pass through", LLMConfig{Temperature: 0.7, TopP: 0.9, TopK: 5, ToolChoice: "required"}, 5, DefaultMaxOutputTokens, "required", 0.7, 0.9},
		{"explicit top_k wins over greedy default", LLMConfig{Temperature: 0, TopK: 40}, 40, DefaultMaxOutputTokens, "", 0, 0},
		{"top_k omitted when sampling without top_k", LLMConfig{Temperature: 0.8}, nil, DefaultMaxOutputTokens, "", 0.8, 0},
		{"explicit top_k kept when sampling", LLMConfig{Temperature: 0.8, TopK: 20}, 20, DefaultMaxOutputTokens, "", 0.8, 0},
		{"explicit max tokens win", LLMConfig{MaxOutputTokens: 512}, 1, 512, "", 0, 0},
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
			if tt.wantChoice != "" && req["tool_choice"] != tt.wantChoice {
				t.Errorf("tool_choice %v want %v", req["tool_choice"], tt.wantChoice)
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
	tests := []struct {
		name        string
		msg         *model.ResponseMessage
		wantContent string
	}{
		{"content returned", &model.ResponseMessage{Content: "answer"}, "answer"},
		{"reasoning only yields no content", &model.ResponseMessage{Reasoning: "thinking"}, ""},
		{"empty message yields no content", &model.ResponseMessage{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := logModelOutput(context.Background(), zerolog.Nop(), 0, tt.msg, config.DefaultLogContentBytes); got != tt.wantContent {
				t.Errorf("content %q want %q", got, tt.wantContent)
			}
		})
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
		if rec["iter"] != float64(2) {
			t.Errorf("line %d iter %v want 2", i, rec["iter"])
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

// TestAssistantMessageShapeLogged pins the per-turn debug trace: a bare tool
// call with no text must still leave a model-output record showing what the
// model returned, so silent turns read as "model said nothing" and never as
// dropped logging.
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
	agt := NewAgent(zerolog.New(&buf), mock, 3, time.Second, 30*time.Second, defaultLLMConfig(), "run-shape")
	conf := config.Config{
		VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:  "m",
		Prompt: "task",
	}
	if err := agt.Execute(context.Background(), conf, multipass.New(zerolog.Nop())); err != nil {
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

func TestExecute(t *testing.T) {
	tests := []struct {
		name      string
		mock      *mockKronk
		maxIter   int
		wantCalls int
		wantErr   bool
		llmCfg    *LLMConfig
		vm        config.VMConfig
		prompt    string
		verifyReq func(t *testing.T, reqs []model.D)
	}{
		{
			name:    "no tool calls success",
			mock:    &mockKronk{responses: []model.ChatResponse{chatResp("final answer", "thinking", model.FinishReasonStop, nil, &model.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3, TokensPerSecond: 10})}},
			maxIter: 5, wantCalls: 1,
			vm:     config.VMConfig{Name: "test-vm", CPU: 2, RAM: "4G", Disk: "20G"},
			prompt: "do nothing",
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
			llmCfg: &LLMConfig{Temperature: 0.7, TopP: 0.9, TopK: 5, ToolChoice: "required"},
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
				if reqs[0]["tool_choice"] != "required" {
					t.Errorf("tool_choice %v want required", reqs[0]["tool_choice"])
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
			llmCfg := defaultLLMConfig()
			if tt.llmCfg != nil {
				llmCfg = *tt.llmCfg
			}
			agent := NewAgent(zerolog.Nop(), tt.mock, tt.maxIter, time.Second, 5*time.Second, llmCfg, "")
			vm := config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"}
			if tt.vm.Name != "" {
				vm = tt.vm
			}
			prompt := "task"
			if tt.prompt != "" {
				prompt = tt.prompt
			}
			conf := config.Config{VM: vm, Prompt: prompt}
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
	agent := NewAgent(log, mock, 3, 2*time.Second, 5*time.Second, defaultLLMConfig(), "")
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
		compactionsCounter, compactionDurationHist, tokensReclaimedHist = nil, nil, nil
	})
	mock := &mockKronk{
		contextWidth: 32768,
		responses: []model.ChatResponse{chatResp("done", "", model.FinishReasonStop, nil,
			&model.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120, TokensPerSecond: 8.5})},
	}
	agent := NewAgent(zerolog.Nop(), mock, 2, time.Second, 5*time.Second, defaultLLMConfig(), "0198f0c1-2a3b-7c4d-8e5f-60718293a4b5")
	conf := config.Config{VM: config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"}, Prompt: "task"}
	if err := agent.Execute(context.Background(), conf, multipass.New(zerolog.Nop())); err != nil {
		t.Fatalf("execute: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}

	seen := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			seen[m.Name] = true
			for _, set := range dataPointAttrs(m.Data) {
				if v, ok := set.Value("mph.run.id"); ok {
					t.Fatalf("metric %q must not carry mph.run.id, got %q", m.Name, v.AsString())
				}
			}
			if m.Name == "mph.context.window" || m.Name == "mph.context.tokens" {
				if _, ok := m.Data.(metricdata.Gauge[int64]); !ok {
					t.Errorf("metric %q must be Int64Gauge, got %T", m.Name, m.Data)
				}
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

	if got := runSpanAttrs(withRunID(context.Background(), "0198f0c1-2a3b-7c4d-8e5f-60718293a4b5")); len(got) == 0 {
		t.Fatalf("runSpanAttrs must still carry mph.run.id on spans")
	}
}

func TestRunIDFromContext(t *testing.T) {
	const id = "0198f0c1-2a3b-7c4d-8e5f-60718293a4b5"
	tests := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"bare context yields empty", context.Background(), ""},
		{"run id round-trips", withRunID(context.Background(), id), id},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runIDFromContext(tt.ctx); got != tt.want {
				t.Errorf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestRunSpanAttrs(t *testing.T) {
	const id = "0198f0c1-2a3b-7c4d-8e5f-60718293a4b5"
	iter := attribute.Int("mph.iteration", 3)
	tests := []struct {
		name      string
		ctx       context.Context
		wantKeys  []string
		wantRunID string
	}{
		{"omits run id when unset", context.Background(), []string{"mph.iteration"}, ""},
		{"adds run id when set", withRunID(context.Background(), id), []string{"mph.iteration", "mph.run.id"}, id},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runSpanAttrs(tt.ctx, iter)
			if len(got) != len(tt.wantKeys) {
				t.Fatalf("got %d attrs, want %d: %v", len(got), len(tt.wantKeys), got)
			}
			for i, key := range tt.wantKeys {
				if string(got[i].Key) != key {
					t.Errorf("attr %d key = %q, want %q", i, got[i].Key, key)
				}
			}
			if tt.wantRunID != "" && got[1].Value.AsString() != tt.wantRunID {
				t.Errorf("run id = %q, want %q", got[1].Value.AsString(), tt.wantRunID)
			}
		})
	}
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

func TestFinalCommandOutput(t *testing.T) {
	tests := []struct {
		name    string
		outputs []string
		want    string
	}{
		{"no outputs", nil, ""},
		{"single output", []string{"out"}, "out"},
		{"joined with separator", []string{"a", "b"}, "a\n---\nb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := finalCommandOutput(tt.outputs); got != tt.want {
				t.Errorf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestFinalCommandOutputIsCapped(t *testing.T) {
	outputs := []string{strings.Repeat("a", 40000), strings.Repeat("b", 40000)}
	got := finalCommandOutput(outputs)
	if len(got) > finalOutputBytes+64 {
		t.Errorf("len %d exceeds cap %d plus marker", len(got), finalOutputBytes)
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("missing truncation marker")
	}
}

func TestSystemPromptBatchingGuidance(t *testing.T) {
	// The prompt must not tell the model to run everything sequentially while
	// also telling it to batch; the two instructions conflict and a small model
	// resolves that unpredictably.
	tests := []struct {
		name string
		sub  string
		want bool
	}{
		{"no sequential instruction", "sequentially", false},
		{"batching section", "Batching Work", true},
		{"chaining operator", "&&", true},
		{"single turn", "single turn", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := contains(systemPrompt, tt.sub); got != tt.want {
				t.Errorf("system prompt contains %q = %v, want %v", tt.sub, got, tt.want)
			}
		})
	}
}

// resp.Usage covers the prompt that was sent plus the completion. It cannot
// cover the tool results appended after the reply, because they do not exist
// yet when the model answers. Those are the largest messages a turn adds, so
// measuring only the reported usage understates the context that the next
// prompt has to fit and delays compaction.
func TestRunIterationCountsToolResultsInContext(t *testing.T) {
	const usageTotal = 100
	big := strings.Repeat("z", 4000)

	mock := &mockKronk{contextWidth: 8192, responses: []model.ChatResponse{
		chatResp("", "", model.FinishReasonTool, []model.ResponseToolCall{{
			ID: "c1", Type: "function",
			Function: model.ResponseToolCallFunction{
				Name: "multipass_exec", Arguments: model.ToolCallArguments{"command": "cat big", "capture": true},
			},
		}}, &model.Usage{TotalTokens: usageTotal, PromptTokens: 90, CompletionTokens: 10}),
	}}
	cfg := config.Config{
		VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:  "m",
		Prompt: "task",
		Output: config.OutputConfig{Enabled: true, Mode: "auto", MaxCommandSize: 1 << 20, MaxTotalSize: 1 << 26, SearchMaxMatches: 200},
	}
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
	store := output.NewStore(f, "vm", "/tmp/mph-output/run1", cfg.Output, 1<<20, nil)
	agt := NewAgent(zerolog.Nop(), mock, 3, time.Second, 30*time.Second, defaultLLMConfig(), "run-tok")
	agt.conversation = buildInitialConversation(cfg)
	agt.toolDocs = buildToolDocuments(cfg)
	agt.store = store

	if err := agt.runIteration(context.Background(), multipass.New(zerolog.Nop()), cfg); err != nil {
		t.Fatalf("runIteration: %v", err)
	}
	if agt.contextTokens <= usageTotal {
		t.Fatalf("context %d must count the tool result on top of the reported %d", agt.contextTokens, usageTotal)
	}
	var wantTool int64
	for _, m := range agt.conversation {
		if m["role"] == "tool" {
			c, _ := m["content"].(string)
			wantTool += int64(agt.estimateTokens(context.Background(), c))
		}
	}
	if want, got := int64(usageTotal)+wantTool, agt.contextTokens; got != want {
		t.Fatalf("context %d want %d (usage %d + tool results %d)", got, want, usageTotal, wantTool)
	}
}

// A later turn's reported usage already includes everything appended before
// it, so the tool-result tally must reset rather than accumulate forever.
func TestRunIterationUsageSupersedesEarlierToolResults(t *testing.T) {
	mock := &mockKronk{contextWidth: 8192, responses: []model.ChatResponse{
		chatResp("done", "", model.FinishReasonStop, nil, &model.Usage{TotalTokens: 5000}),
	}}
	cfg := config.Config{
		VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:  "m",
		Prompt: "task",
	}
	agt := NewAgent(zerolog.Nop(), mock, 1, time.Second, 30*time.Second, defaultLLMConfig(), "run-reset")
	agt.conversation = buildInitialConversation(cfg)
	agt.toolDocs = buildToolDocuments(cfg)
	agt.contextTokens = 12345

	if err := agt.runIteration(context.Background(), multipass.New(zerolog.Nop()), cfg); err != nil {
		t.Fatalf("runIteration: %v", err)
	}
	if agt.contextTokens != 5000 {
		t.Fatalf("context %d want 5000 (reported usage supersedes the previous tally)", agt.contextTokens)
	}
}
