package compact

import (
	"context"
	"strings"
	"testing"

	"github.com/ardanlabs/kronk/sdk/kronk/model"
)

type fakeSummarizer struct {
	summary string
	err     error
}

func (f *fakeSummarizer) Summarize(_ context.Context, _, _ string, _ int) (string, error) {
	return f.summary, f.err
}

func conv(n int) []model.D {
	out := []model.D{{"role": "system", "content": "sys"}, {"role": "user", "content": "task: do things"}}
	for i := 0; i < n-2; i++ {
		out = append(out, model.D{"role": "assistant", "content": "step"})
	}
	return out
}

func TestShouldCompact(t *testing.T) {
	if !ShouldCompact(8000, 10000, 0.8, 10, 6) {
		t.Fatalf("want compact at threshold")
	}
	if ShouldCompact(7999, 10000, 0.8, 10, 6) {
		t.Fatalf("below threshold must not compact")
	}
	if ShouldCompact(9000, 10000, 0.8, 3, 6) {
		t.Fatalf("short history must not compact")
	}
	if ShouldCompact(9000, 0, 0.8, 10, 6) {
		t.Fatalf("unknown window must not compact")
	}
}

func TestRewriteKeepsSystemAndTask(t *testing.T) {
	conversation := []model.D{
		{"role": "system", "content": "SYS-BYTES"},
		{"role": "user", "content": "Task:\nrun ls"},
		{"role": "assistant", "content": "doing"},
		{"role": "tool", "name": "multipass_exec", "content": "out"},
		{"role": "assistant", "content": "done"},
		{"role": "tool", "name": "multipass_exec", "content": "out2"},
	}
	handles := []HandleInfo{{ID: "oid-1", Path: "/tmp/x", TotalLines: 10, TotalBytes: 100, Command: "cat big"}}
	got, outcome := Compact(context.Background(), conversation, handles, 2048, 3, 4000, &fakeSummarizer{summary: "did stuff"}, nil)
	if outcome != "applied" {
		t.Fatalf("outcome %q", outcome)
	}
	if len(got) != 2 {
		t.Fatalf("want exactly 2 messages, got %d", len(got))
	}
	if got[0]["content"] != "SYS-BYTES" {
		t.Fatalf("system must be byte-for-byte, got %v", got[0]["content"])
	}
	body, _ := got[1]["content"].(string)
	for _, want := range []string{"## Task", "run ls", "did stuff", "oid-1"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in %q", want, body)
		}
	}
}

func TestActiveOutputsPresentWhenModelOmits(t *testing.T) {
	conversation := conv(8)
	handles := []HandleInfo{{ID: "must-survive", Path: "/tmp/y", TotalLines: 5, TotalBytes: 50, Command: "ls"}}
	got, _ := Compact(context.Background(), conversation, handles, 2048, 1, 4000, &fakeSummarizer{summary: "no handles mentioned"}, func(s string) int { return 1 })
	body, _ := got[1]["content"].(string)
	if !strings.Contains(body, "must-survive") {
		t.Fatalf("handle must survive deterministically: %q", body)
	}
}

func TestShrinkGuardReverts(t *testing.T) {
	conversation := conv(8)
	_, outcome := Compact(context.Background(), conversation, nil, 2048, 3, 4000, &fakeSummarizer{summary: strings.Repeat("x", 100000)}, func(s string) int { return len(s) })
	if outcome != "reverted" {
		t.Fatalf("want reverted, got %q", outcome)
	}
}

func TestFailureKeepsOriginal(t *testing.T) {
	conversation := conv(8)
	got, outcome := Compact(context.Background(), conversation, nil, 2048, 3, 4000, &fakeSummarizer{err: context.DeadlineExceeded}, nil)
	if outcome != "failed" || len(got) != len(conversation) {
		t.Fatalf("want failed with original, got %q len %d", outcome, len(got))
	}
	got, outcome = Compact(context.Background(), conversation, nil, 2048, 3, 4000, &fakeSummarizer{summary: "  "}, nil)
	if outcome != "failed" {
		t.Fatalf("empty summary must fail, got %q", outcome)
	}
}

func TestRenderHistoryCapsResults(t *testing.T) {
	conversation := []model.D{
		{"role": "assistant", "content": "hi", "tool_calls": []model.D{{"function": model.D{"name": "multipass_exec", "arguments": `{"command":"ls"}`}}}},
		{"role": "tool", "name": "multipass_exec", "content": strings.Repeat("z", 5000)},
	}
	rendered := RenderHistory(conversation, 100)
	if !strings.Contains(rendered, "tool: multipass_exec") {
		t.Fatalf("missing tool line: %q", rendered)
	}
	if len(rendered) > 2000 {
		t.Fatalf("rendered history not capped: %d", len(rendered))
	}
}

func TestRecentTextCountsTurns(t *testing.T) {
	conversation := []model.D{
		{"role": "system", "content": "sys"},
		{"role": "user", "content": "task"},
		{"role": "assistant", "content": "first"},
		{"role": "tool", "name": "multipass_exec", "content": "out1"},
		{"role": "assistant", "content": "second"},
		{"role": "tool", "name": "multipass_exec", "content": "out2"},
		{"role": "assistant", "content": "third"},
		{"role": "tool", "name": "multipass_exec", "content": "out3"},
	}
	got := recentText(conversation, 2, 4000)
	for _, want := range []string{"second", "third", "out2", "out3"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "first") || strings.Contains(got, "out1") {
		t.Fatalf("must exclude older turns: %q", got)
	}
}
