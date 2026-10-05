package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/ardanlabs/kronk/sdk/kronk/model"
	"github.com/rs/zerolog"

	"github.com/trollLemon/MPHarness/internal/config"
	"github.com/trollLemon/MPHarness/internal/multipass"
)

func decodeEvents(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
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

func onlyEvent(t *testing.T, events []map[string]any, kind string) map[string]any {
	t.Helper()
	matched := eventsOf(events, kind)
	if len(matched) != 1 {
		t.Fatalf("want exactly one %q event, got %d", kind, len(matched))
	}
	return matched[0]
}

func TestExecuteLogsToolEvents(t *testing.T) {
	var buf bytes.Buffer
	mock := &mockKronk{contextWidth: 32768, responses: []model.ChatResponse{
		chatResp("", "", model.FinishReasonTool, []model.ResponseToolCall{
			{ID: "c1", Type: "function", Function: model.ResponseToolCallFunction{Name: "no_such_tool", Arguments: model.ToolCallArguments{}}},
		}, &model.Usage{TotalTokens: 100}),
		chatResp("done", "", model.FinishReasonStop, nil, &model.Usage{TotalTokens: 120}),
	}}
	a := NewAgent(zerolog.New(&buf), mock, Options{
		MaxIterations: 3, ChatTimeout: time.Second, TotalTimeout: 30 * time.Second,
		LLM: defaultLLMConfig(), RunID: "01J9",
	})
	conf := config.Config{VM: config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"}, Prompt: "task"}
	if err := a.Execute(t.Context(), conf, multipass.New(zerolog.Nop())); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	events := decodeEvents(t, &buf)
	call := onlyEvent(t, events, eventAgentToolCall)
	if call["tool"] != "no_such_tool" || call["id"] != "c1" || call["iter"] != float64(1) {
		t.Errorf("agent_tool_call = %v, want tool no_such_tool, id c1, iter 1", call)
	}
	failed := onlyEvent(t, events, eventToolFailed)
	if failed["tool"] != "no_such_tool" || failed["iter"] != float64(1) {
		t.Errorf("tool_failed = %v, want tool no_such_tool at iter 1", failed)
	}
	if _, ok := failed["dur_ms"].(float64); !ok {
		t.Errorf("tool_failed missing numeric dur_ms: %v", failed)
	}
	if len(eventsOf(events, eventToolSucceeded)) != 0 {
		t.Error("a failed tool must not also log tool_succeeded")
	}
	summary := onlyEvent(t, events, eventRunSummary)
	if summary["tool_calls"] != float64(1) || summary["tool_failures"] != float64(1) {
		t.Errorf("run_summary tool_calls/tool_failures = %v/%v, want 1/1", summary["tool_calls"], summary["tool_failures"])
	}
}
