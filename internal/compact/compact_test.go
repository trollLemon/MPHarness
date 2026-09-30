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
			handles:     []HandleInfo{{ID: "oid-1", TotalLines: 10, TotalBytes: 100, Command: "cat big"}},
			summary:     "did stuff",
			wantOutcome: "applied",
			wantSystem:  "SYS-BYTES",
			wantSubs:    []string{"## Task", "run ls", "did stuff", "oid-1", "after 0 compactions", "never invent one"},
		},
		{
			name:         "handles survive when the model omits them",
			conversation: conv(8),
			handles:      []HandleInfo{{ID: "must-survive", TotalLines: 5, TotalBytes: 50, Command: "ls"}},
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
			got, outcome, _ := Compact(context.Background(), tt.conversation, tt.handles, 2048, 4000, Window{Size: 1 << 20}, &fakeSummarizer{summary: tt.summary}, nil, tt.applied)
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
	_, outcome, _ := Compact(context.Background(), conversation, nil, 2048, 4000, Window{Size: 1 << 20}, &fakeSummarizer{summary: strings.Repeat("x", 100000)}, func(s string) int { return len(s) }, 0)
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
			got, outcome, _ := Compact(context.Background(), conversation, nil, 2048, 4000, Window{Size: 1 << 20}, &fakeSummarizer{summary: tt.summary, err: tt.err}, nil, 0)
			if outcome != "failed" {
				t.Fatalf("want failed, got %q", outcome)
			}
			if len(got) != len(conversation) {
				t.Fatalf("want original kept, got len %d want %d", len(got), len(conversation))
			}
		})
	}
}

type countingSummarizer struct {
	summary   string
	calls     int
	maxTokens []int
	rendered  []string
}

func (f *countingSummarizer) Summarize(_ context.Context, _, renderedHistory string, maxTokens int) (string, error) {
	f.calls++
	f.maxTokens = append(f.maxTokens, maxTokens)
	f.rendered = append(f.rendered, renderedHistory)
	return f.summary, nil
}

func TestPreCheckSkipsSummarizerWhenNothingToSave(t *testing.T) {
	conversation := conv(4)
	sum := &countingSummarizer{summary: "brief"}
	_, outcome, summaryLen := Compact(context.Background(), conversation, nil, 2048, 4000, Window{Size: 1 << 20}, sum, func(s string) int { return len(s) }, 0)
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

func TestSummarizerInputFitsWindow(t *testing.T) {
	longResult := strings.Repeat("out", 4000)
	conversation := []model.D{
		{"role": "system", "content": "sys"},
		{"role": "user", "content": "Task:\nrun ls"},
	}
	for i := 0; i < 4; i++ {
		conversation = append(conversation,
			model.D{"role": "assistant", "content": "calling", "tool_calls": []model.D{
				{"function": model.D{"name": "multipass_exec", "arguments": `{"command":"ls"}`}}}},
			model.D{"role": "tool", "name": "multipass_exec", "content": longResult},
		)
	}
	est := func(s string) int { return len(s)/4 + 1 }

	tests := []struct {
		name          string
		window        int
		budget        int
		wantRendered  int
		wantOversized bool
	}{
		{name: "budget larger than the window", window: 4096, budget: 2048, wantRendered: 4000},
		{name: "window several times the budget", window: 32768, budget: 2048, wantRendered: 4000},
		{name: "window barely fits the prompt", window: 1024, budget: 2048, wantRendered: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sum := &countingSummarizer{summary: "brief"}
			Compact(context.Background(), conversation, nil, 512, tt.budget,
				Window{Size: tt.window}, sum, est, 0)

			if sum.calls != 1 {
				t.Fatalf("want 1 summarizer call, got %d", sum.calls)
			}
			if used := est(CompactionSystemPrompt) + est(sum.rendered[0]); used > tt.window {
				t.Fatalf("summarizer prompt is %d tokens, over the %d window", used, tt.window)
			}
			if len(sum.rendered[0]) < tt.wantRendered {
				t.Fatalf("render is %d bytes, want at least %d; the bound must not gut the history",
					len(sum.rendered[0]), tt.wantRendered)
			}
		})
	}
}

func TestSummaryBudgetCappedBySavings(t *testing.T) {
	conversation := []model.D{
		{"role": "system", "content": "sys"},
		{"role": "user", "content": "Task:\nrun ls"},
		{"role": "assistant", "content": strings.Repeat("a", 2000)},
		{"role": "tool", "name": "multipass_exec", "content": strings.Repeat("b", 2000)},
	}

	sum := &countingSummarizer{summary: "Completed: listed files. Remaining: done."}
	est := func(s string) int { return len(s)/4 + 1 }
	got, outcome, summaryLen := Compact(context.Background(), conversation, nil, 2048, 4000, Window{Size: 1 << 20}, sum, est, 2)
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
	_, outcome, summaryLen := Compact(context.Background(), conversation, nil, 2048, 4000, Window{Size: 1 << 20}, sum, func(s string) int { return len(s) }, 0)
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
			got := BuildUserMessage("do things", "Completed: x. Remaining: y.", tt.handles, tt.applied)
			for _, want := range tt.wantSubs {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q in %q", want, got)
				}
			}
		})
	}
}

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

func TestRenderHistory(t *testing.T) {
	longToolResult := []model.D{
		{"role": "assistant", "content": "calling", "tool_calls": []model.D{
			{"function": model.D{"name": "multipass_exec", "arguments": `{"command":"ls"}`}}}},
		{"role": "tool", "name": "multipass_exec", "content": strings.Repeat("z", 5000)},
	}
	manyTurns := make([]model.D, 0, 800)
	for i := 0; i < 400; i++ {
		manyTurns = append(manyTurns, model.D{
			"role": "assistant", "content": strings.Repeat("a", 1000),
			"reasoning_content": strings.Repeat("r", 1000),
		})
	}

	tests := []struct {
		name         string
		conversation []model.D
		budget       int
		maxBytes     int
		style        ToolResultStyle
		maxRendered  int
		want         []string
		dontWant     []string
	}{
		{
			// Prose is the summarizer's only view of what the agent was doing,
			// but a single rambling message still has to respect the budget.
			name:         "assistant prose respects the per-message budget",
			conversation: []model.D{{"role": "assistant", "content": strings.Repeat("a", 100000)}},
			budget:       100, maxBytes: 1 << 20, style: ToolResultFullText,
			maxRendered: 500,
		},
		{
			name:         "user text respects the per-message budget",
			conversation: []model.D{{"role": "user", "content": strings.Repeat("u", 100000)}},
			budget:       100, maxBytes: 1 << 20, style: ToolResultFullText,
			maxRendered: 500,
		},
		{
			// budget is per-message, so without a total bound a long
			// conversation renders to len(messages)*budget and the summarizer
			// prompt grows without limit.
			name:         "total render is bounded by maxBytes",
			conversation: manyTurns,
			budget:       1000, maxBytes: 8000, style: ToolResultFullText,
			maxRendered: 8000,
		},
		{
			// The summarizer has to see what a command actually returned.
			name:         "full text style keeps tool output in the summarizer view",
			conversation: longToolResult,
			budget:       100, maxBytes: 1 << 20, style: ToolResultFullText,
			want: []string{"multipass_exec", "zzz"},
		},
		{
			// The handover replaces the history, so copying the payload forward
			// makes the rewrite reclaim nothing.
			name:         "handle ref style stands the payload down in the handover",
			conversation: longToolResult,
			budget:       100, maxBytes: 1 << 20, style: ToolResultHandleRef,
			maxRendered: 500,
			want:        []string{"multipass_exec", "5000 bytes"},
			dontWant:    []string{"zzz"},
		},
		{
			name:         "handle ref style keeps a short tool result inline",
			conversation: []model.D{{"role": "tool", "name": "multipass_exec", "content": "ok"}},
			budget:       100, maxBytes: 1 << 20, style: ToolResultHandleRef,
			want: []string{"ok"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RenderHistory(tt.conversation, tt.budget, tt.maxBytes, tt.style)
			if tt.maxRendered > 0 && len(got) > tt.maxRendered {
				t.Fatalf("rendered %d bytes, over the %d bound", len(got), tt.maxRendered)
			}
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q in %q", want, got)
				}
			}
			for _, dontWant := range tt.dontWant {
				if strings.Contains(got, dontWant) {
					t.Fatalf("must not contain %q: %q", dontWant, got)
				}
			}
		})
	}
}

func TestEstimateMessageCountsToolCalls(t *testing.T) {
	m := model.D{
		"role": "assistant",
		"tool_calls": []model.D{
			{"id": "call-1", "function": model.D{"name": "multipass_exec", "arguments": `{"command":"df -h"}`}},
		},
	}

	if got := EstimateMessage(m, func(s string) int { return len(s) }); got == 0 {
		t.Fatal("tool_calls must contribute to the estimate, got 0")
	}
}

func TestCompactionSystemPromptAsksForResumableState(t *testing.T) {
	for _, want := range []string{"Done", "In progress", "Not started"} {
		if !strings.Contains(CompactionSystemPrompt, want) {
			t.Errorf("CompactionSystemPrompt missing %q section", want)
		}
	}
}

func TestCompactionSystemPromptProtectsHandleIDs(t *testing.T) {
	for _, want := range []string{"output_id", "verbatim"} {
		if !strings.Contains(CompactionSystemPrompt, want) {
			t.Errorf("CompactionSystemPrompt missing %q", want)
		}
	}
}

func TestBuildUserMessageCarriesNoVerbatimTail(t *testing.T) {
	got := BuildUserMessage("do things", "Done: x", nil, 1)
	if strings.Contains(got, "## Recent activity") {
		t.Fatalf("handover still carries a verbatim recent block: %q", got)
	}
}
