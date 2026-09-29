package compact

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

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
	tests := []struct {
		name      string
		tokens    int64
		window    int64
		threshold float64
		msgs      int
		minMsgs   int
		want      bool
	}{
		{"at threshold", 8000, 10000, 0.8, 10, 6, true},
		{"below threshold", 7999, 10000, 0.8, 10, 6, false},
		{"short history", 9000, 10000, 0.8, 3, 6, false},
		{"unknown window", 9000, 0, 0.8, 10, 6, false},
		{"zero usage", 0, 10000, 0.8, 10, 6, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShouldCompact(tt.tokens, tt.window, tt.threshold, tt.msgs, tt.minMsgs); got != tt.want {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}

func TestCompactHandover(t *testing.T) {
	tests := []struct {
		name         string
		conversation []model.D
		handles      []HandleInfo
		summary      string
		applied      int
		wantOutcome  string
		wantSystem   string
		wantSubs     []string
	}{
		{
			name: "rewrite keeps system and task",
			conversation: []model.D{
				{"role": "system", "content": "SYS-BYTES"},
				{"role": "user", "content": "Task:\nrun ls"},
				{"role": "assistant", "content": "doing"},
				{"role": "tool", "name": "multipass_exec", "content": "out"},
				{"role": "assistant", "content": "done"},
				{"role": "tool", "name": "multipass_exec", "content": "out2"},
			},
			handles:     []HandleInfo{{ID: "oid-1", Path: "/tmp/x", TotalLines: 10, TotalBytes: 100, Command: "cat big"}},
			summary:     "did stuff",
			wantOutcome: "applied",
			wantSystem:  "SYS-BYTES",
			wantSubs:    []string{"## Task", "run ls", "did stuff", "oid-1", "after 0 compactions", "never invent one"},
		},
		{
			name:         "handles survive when the model omits them",
			conversation: conv(8),
			handles:      []HandleInfo{{ID: "must-survive", Path: "/tmp/y", TotalLines: 5, TotalBytes: 50, Command: "ls"}},
			summary:      "no handles mentioned",
			wantOutcome:  "applied",
			wantSubs:     []string{"must-survive"},
		},
		{
			name:         "handover carries the compaction count",
			conversation: conv(8),
			summary:      "did stuff",
			applied:      3,
			wantOutcome:  "applied",
			wantSubs:     []string{"after 3 compactions", "do not call output_search or output_read"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, outcome, _ := Compact(context.Background(), tt.conversation, tt.handles, 2048, 3, 4000, &fakeSummarizer{summary: tt.summary}, nil, tt.applied)
			if outcome != tt.wantOutcome {
				t.Fatalf("outcome %q want %q", outcome, tt.wantOutcome)
			}
			if len(got) != 2 {
				t.Fatalf("want exactly 2 messages, got %d", len(got))
			}
			if tt.wantSystem != "" && got[0]["content"] != tt.wantSystem {
				t.Fatalf("system must be byte-for-byte, got %v", got[0]["content"])
			}
			body, _ := got[1]["content"].(string)
			for _, want := range tt.wantSubs {
				if !strings.Contains(body, want) {
					t.Fatalf("missing %q in %q", want, body)
				}
			}
		})
	}
}

func TestShrinkGuardReverts(t *testing.T) {
	conversation := conv(8)
	_, outcome, _ := Compact(context.Background(), conversation, nil, 2048, 3, 4000, &fakeSummarizer{summary: strings.Repeat("x", 100000)}, func(s string) int { return len(s) }, 0)
	if outcome != "reverted" {
		t.Fatalf("want reverted, got %q", outcome)
	}
}

func TestFailureKeepsOriginal(t *testing.T) {
	tests := []struct {
		name    string
		summary string
		err     error
	}{
		{"summarizer error", "", context.DeadlineExceeded},
		{"blank summary", "  ", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conversation := conv(8)
			got, outcome, _ := Compact(context.Background(), conversation, nil, 2048, 3, 4000, &fakeSummarizer{summary: tt.summary, err: tt.err}, nil, 0)
			if outcome != "failed" {
				t.Fatalf("want failed, got %q", outcome)
			}
			if len(got) != len(conversation) {
				t.Fatalf("want original kept, got len %d want %d", len(got), len(conversation))
			}
		})
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

type countingSummarizer struct {
	summary   string
	calls     int
	maxTokens []int
}

func (f *countingSummarizer) Summarize(_ context.Context, _, _ string, maxTokens int) (string, error) {
	f.calls++
	f.maxTokens = append(f.maxTokens, maxTokens)
	return f.summary, nil
}

func TestPreCheckSkipsSummarizerWhenNothingToSave(t *testing.T) {
	conversation := conv(4)
	sum := &countingSummarizer{summary: "brief"}
	_, outcome, summaryLen := Compact(context.Background(), conversation, nil, 2048, 3, 4000, sum, func(s string) int { return len(s) }, 0)
	if outcome != "reverted" {
		t.Fatalf("want reverted, got %q", outcome)
	}
	if sum.calls != 0 {
		t.Fatalf("pre-check must skip the summarizer call, got %d calls", sum.calls)
	}
	if summaryLen != 0 {
		t.Fatalf("no summary produced, want 0 bytes, got %d", summaryLen)
	}
}

func TestSummaryBudgetCappedBySavings(t *testing.T) {
	conversation := []model.D{
		{"role": "system", "content": "sys"},
		{"role": "user", "content": "Task:\nrun ls"},
	}
	for i := 0; i < 3; i++ {
		conversation = append(conversation,
			model.D{"role": "assistant", "content": strings.Repeat("a", 2000)},
			model.D{"role": "tool", "name": "multipass_exec", "content": strings.Repeat("b", 2000)},
		)
	}
	sum := &countingSummarizer{summary: "Completed: listed files. Remaining: done."}
	est := func(s string) int { return len(s)/4 + 1 }
	got, outcome, summaryLen := Compact(context.Background(), conversation, nil, 2048, 1, 4000, sum, est, 2)
	if outcome != "applied" {
		t.Fatalf("want applied, got %q", outcome)
	}
	if sum.calls != 1 {
		t.Fatalf("want exactly 1 summarizer call, got %d", sum.calls)
	}
	if sum.maxTokens[0] >= 2048 {
		t.Fatalf("budget must be capped below maxSummaryTokens 2048, got %d", sum.maxTokens[0])
	}
	if summaryLen != len("Completed: listed files. Remaining: done.") {
		t.Fatalf("want summary bytes logged, got %d", summaryLen)
	}
	body, _ := got[1]["content"].(string)
	if !strings.Contains(body, "after 2 compactions") {
		t.Fatalf("handover must carry the compaction count: %q", body)
	}
}

func TestRevertedReportsSummaryBytes(t *testing.T) {
	conversation := []model.D{
		{"role": "system", "content": "sys"},
		{"role": "user", "content": "Task:\nrun ls"},
	}
	for i := 0; i < 6; i++ {
		conversation = append(conversation,
			model.D{"role": "assistant", "content": strings.Repeat("a", 2000)},
			model.D{"role": "tool", "name": "multipass_exec", "content": strings.Repeat("b", 2000)},
		)
	}
	sum := &countingSummarizer{summary: strings.Repeat("x", 50000)}
	_, outcome, summaryLen := Compact(context.Background(), conversation, nil, 2048, 3, 4000, sum, func(s string) int { return len(s) }, 0)
	if outcome != "reverted" {
		t.Fatalf("want reverted, got %q", outcome)
	}
	if summaryLen != 50000 {
		t.Fatalf("revert must still report summary bytes, got %d", summaryLen)
	}
}

func TestBuildUserMessageGuidesHandles(t *testing.T) {
	tests := []struct {
		name     string
		handles  []HandleInfo
		applied  int
		wantSubs []string
	}{
		{
			name:     "no handles forbids output tools",
			applied:  1,
			wantSubs: []string{"after 1 compaction", "do not call output_search or output_read", "Completed: x"},
		},
		{
			name:     "listed handles must not be invented",
			handles:  []HandleInfo{{ID: "oid-9", TotalLines: 3, TotalBytes: 30, Command: "ls"}},
			wantSubs: []string{"oid-9", "never invent one", "after 0 compactions"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildUserMessage("do things", "Completed: x. Remaining: y.", tt.handles, "recent", tt.applied)
			for _, want := range tt.wantSubs {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q in %q", want, got)
				}
			}
		})
	}
}

// A cut in the middle of a multi-byte rune yields invalid UTF-8, which some
// tokenizers and JSON encoders reject outright.
func TestTruncateHeadKeepsValidUTF8(t *testing.T) {
	tests := []struct {
		name string
		s    string
		n    int
	}{
		{"cut lands mid rune", strings.Repeat("a", 5) + "日本語テキスト", 8},
		{"cut lands on rune start", strings.Repeat("a", 5) + "日本語テキスト", 9},
		{"emoji surrogate pair", strings.Repeat("a", 3) + "🙂🙂🙂", 5},
		{"no truncation", "short", 100},
		{"zero budget", "anything", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateHead(tt.s, tt.n)
			if !utf8.ValidString(got) {
				t.Fatalf("invalid UTF-8: %q", got)
			}
			if body := strings.TrimSuffix(got, "…(truncated)"); !strings.HasPrefix(tt.s, body) {
				t.Fatalf("truncation is not a prefix of the input: %q", got)
			}
		})
	}
}

// A single long assistant message must respect the budget like every other
// role; tool results are capped but assistant prose is not.
func TestRenderHistoryCapsAssistantContent(t *testing.T) {
	conversation := []model.D{
		{"role": "assistant", "content": strings.Repeat("a", 100000)},
	}
	rendered := RenderHistory(conversation, 100)
	if len(rendered) > 500 {
		t.Fatalf("assistant content not capped: %d bytes", len(rendered))
	}
}

// budget is a per-message allowance, so a long conversation renders to
// len(messages)*budget with no global limit. The summarizer prompt has to stay
// bounded regardless of how many turns went by.
func TestRenderHistoryBoundsTotalSize(t *testing.T) {
	var conversation []model.D
	for i := 0; i < 400; i++ {
		conversation = append(conversation, model.D{
			"role": "assistant", "content": strings.Repeat("a", 1000),
			"reasoning_content": strings.Repeat("r", 1000),
		})
	}
	rendered := RenderHistory(conversation, 1000)
	if len(rendered) > 8*1000 {
		t.Fatalf("rendered history has no total bound: %d bytes", len(rendered))
	}
}
