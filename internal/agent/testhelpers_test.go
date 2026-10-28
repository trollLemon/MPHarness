package agent_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/ardanlabs/kronk/sdk/kronk/model"

	"github.com/trollLemon/MPHarness/internal/config"
)

type testFatal interface {
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
}

const (
	eventTokenUsage               = "token_usage"
	eventIteration                = "iteration"
	eventAgentToolCall            = "agent_tool_call"
	eventToolSucceeded            = "tool_succeeded"
	eventToolFailed               = "tool_failed"
	eventToolOutputTooLarge       = "tool_output_too_large"
	eventContextOverflow          = "context_overflow"
	eventContextCompaction        = "context_compaction"
	eventContextCompactionApplied = "context_compaction_applied"
	eventRunSummary               = "run_summary"
)

// testBytesPerToken mirrors the agent's conservative estimate. If production
// changes its accounting, this constant must move with it or token-pressure
// scenarios stop describing the same run.
const testBytesPerToken = 4

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
	handover     func(context.Context, model.D) (model.ChatResponse, error)

	agentCalls    int
	handoverCalls int
	usages        []*model.Usage
	overflowAt    int
	overflowErr   error
	tokenizeErr   error
	tokenizeZero  bool
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
		m.handoverCalls++
		m.captured = append(m.captured, req)
		if m.handover != nil {
			return m.handover(ctx, req)
		}
		if m.failHandover {
			return model.ChatResponse{}, context.DeadlineExceeded
		}
		return chatResp(validHandoverSummary, "", model.FinishReasonStop, nil, nil), nil
	}

	idx := m.agentCalls
	m.agentCalls++
	m.calls++
	m.captured = append(m.captured, req)
	if m.overflowErr != nil && idx == m.overflowAt {
		return model.ChatResponse{}, m.overflowErr
	}

	responseIdx := m.respIdx
	m.respIdx++
	if responseIdx < len(m.responses) {
		var err error
		if responseIdx < len(m.errs) && m.errs[responseIdx] != nil {
			err = m.errs[responseIdx]
		}
		m.usages = append(m.usages, m.responses[responseIdx].Usage)
		return m.responses[responseIdx], err
	}
	m.usages = append(m.usages, nil)
	return model.ChatResponse{}, nil
}

func (m *mockKronk) Tokenize(_ context.Context, d model.D) (model.TokenizeResponse, error) {
	if m.tokenizeErr != nil {
		return model.TokenizeResponse{}, m.tokenizeErr
	}
	if m.tokenizeZero {
		return model.TokenizeResponse{}, nil
	}
	input, _ := d["input"].(string)
	n := len(input) / testBytesPerToken
	if n < 1 && len(input) > 0 {
		n = 1
	}
	return model.TokenizeResponse{Tokens: n}, nil
}

func isCompactionRequest(req model.D) bool {
	if _, ok := req["tools"]; ok {
		return false
	}
	msgs, _ := req["messages"].([]model.D)
	if len(msgs) == 0 {
		return false
	}
	system, _ := msgs[0]["content"].(string)
	return strings.Contains(system, "handover")
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

func deltaResp(content, reasoning, finishReason string) model.ChatResponse {
	fr := finishReason
	return model.ChatResponse{
		Choices: []model.Choice{{
			Delta:           &model.ResponseMessage{Content: content, Reasoning: reasoning},
			FinishReasonPtr: &fr,
		}},
	}
}

func identityScripted(command string) (string, int) {
	switch {
	case strings.HasPrefix(command, "id -u"), strings.HasPrefix(command, "id -g"):
		return "1000\n", 0
	case strings.HasPrefix(command, "getent passwd"):
		return "ubuntu:x:1000:1000:Ubuntu:/home/ubuntu:/bin/bash\n", 0
	default:
		return "", 0
	}
}

func defaultLLMConfig() config.LLMConfig {
	return config.LLMConfig{Temperature: 0.0, TopP: 0.1, TopK: 1}
}

func decodeEvents(t testFatal, buf *bytes.Buffer) []map[string]any {
	var events []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var line map[string]any
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("decode log line %q: %v", sc.Text(), err)
		}
		events = append(events, line)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan logs: %v", err)
	}
	return events
}

func eventsOf(events []map[string]any, kind string) []map[string]any {
	var out []map[string]any
	for _, e := range events {
		if e["event"] == kind {
			out = append(out, e)
		}
	}
	return out
}

func onlyEvent(t testFatal, events []map[string]any, kind string) map[string]any {
	matched := eventsOf(events, kind)
	if len(matched) != 1 {
		t.Fatalf("want exactly one %q event, got %d", kind, len(matched))
	}
	return matched[0]
}
