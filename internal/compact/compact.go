package compact

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/ardanlabs/kronk/sdk/kronk/model"
)

const CompactionSystemPrompt = `You are summarising an autonomous VM-operator agent's conversation so it can continue with a smaller context.

Structure the handover as three sections of bullet points:

Done
- What was attempted, what succeeded, and what failed.
- Key outputs: exact values, file paths, command names, and counts. Copy them verbatim; never round, reformat, or paraphrase a value you are reporting.

In progress
- Anything started but not finished, and the exact point it stopped at.

Not started
- Everything from the original task that has not been attempted yet, so nothing is silently dropped.

Rules:
- This summary is the only record of prior work. Anything you leave out cannot be recovered.
- Captured output handles are referenced by their output_id. Copy every output_id verbatim, character for character, and say what each one contains. Never invent, shorten, or reformat an id.
- Do not invent results, and do not claim work succeeded that the log shows failing.
- Plain prose and bullets.
- Summarize inlined command outputs so that their result is articulated without needing to include the whole thing.

Make sure these are formatted/indicated clearly so an agent can continue without confusion.
`

const StandingInstructions = `Operate asynchronously without user interaction. Prior messages are your own execution logs. Stop immediately on any permission or policy denial without workarounds. Inspect every result's status field. Always finish with a tool-call-free completion summary.`

// ToolResultStyle selects how much of a tool result a render carries.
type ToolResultStyle int

const (
	// ToolResultFullText keeps the bytes, for the summarizer that has to reason
	// about what the command actually returned.
	ToolResultFullText ToolResultStyle = iota
	// ToolResultHandleRef stands a large result down to its size. The handover
	// is the conversation that replaces the history, so copying the payload
	// forward makes the rewrite reclaim nothing.
	ToolResultHandleRef
)

type Summarizer interface {
	Summarize(ctx context.Context, systemPrompt, renderedHistory string, maxTokens int) (string, error)
}

type HandleInfo struct {
	ID         string
	TotalLines int
	TotalBytes int64
	Command    string
}

// Window describes the context budget one compaction decision has to respect:
// the live window, and the per-request cost outside the messages themselves
// (tool schemas and template framing). FixedOverhead is measured, not derived,
// so the guards and the reported figures share the basis kronk charged for.
type Window struct {
	Size          int
	FixedOverhead int
}

// ShouldCompact reports whether token usage has reached threshold of the window.
func ShouldCompact(contextTokens, window int64, threshold float64, numMessages, minMessages int) bool {
	if window <= 0 || contextTokens <= 0 {
		return false
	}
	if numMessages < minMessages {
		return false
	}
	return float64(contextTokens) >= threshold*float64(window)
}

// totalBudgetFactor bounds the whole render when no window is known. budget is a
// per-message allowance, so on its own a long conversation renders to
// len(messages)*budget and the summarizer prompt grows without limit.
const totalBudgetFactor = 8

// bytesPerToken inverts the estimator's tokens-per-byte assumption so a token
// budget can be expressed as the byte allowance RenderHistory works in.
const bytesPerToken = 4

// summarizerHeadroomTokens covers what neither side of the window bound accounts
// for: kronk's chat framing for the summarizer request, and the estimator's own
// error. The summarizer prompt has to land inside the window, not on its edge.
const summarizerHeadroomTokens = 64

// RenderHistory creates a string containing the message history up to this point.
// NOTE: messages are truncated to budget each, except tool results, which the
// output store is responsible for keeping out of the conversation. The total is
// capped at maxBytes so the render cannot outgrow the summarizer's window.
func RenderHistory(conversation []model.D, budget, maxBytes int, style ToolResultStyle) string {
	var b strings.Builder
	remaining := maxBytes
	write := func(s string) {
		if remaining <= 0 {
			return
		}
		if len(s) > remaining {
			s = cutPrefix(s, remaining)
			remaining = 0
		} else {
			remaining -= len(s)
		}
		b.WriteString(s)
	}
	for _, m := range conversation {
		role, _ := m["role"].(string)
		switch role {
		case "assistant":
			if c, _ := m["content"].(string); c != "" {
				write(truncateHead(c, budget) + "\n")
			}
			if r, _ := m["reasoning_content"].(string); r != "" {
				write(truncateHead(r, budget/4) + "\n")
			}
			if tcs, ok := m["tool_calls"].([]model.D); ok {
				for _, tc := range tcs {
					if fn, ok := tc["function"].(model.D); ok {
						name, _ := fn["name"].(string)
						args, _ := fn["arguments"].(string)
						write(fmt.Sprintf("tool: %s(%s)\n", name, args))
					}
				}
			}
		case "tool":
			name, _ := m["name"].(string)
			content, _ := m["content"].(string)
			if style == ToolResultHandleRef && len(content) > budget {
				content = fmt.Sprintf("%d bytes, see ## Active output handles", len(content))
			}
			write(fmt.Sprintf("tool result %s: %s\n", name, content))
		case "user":
			if c, _ := m["content"].(string); c != "" {
				write("user: " + truncateHead(c, budget) + "\n")
			}
		}
	}
	return b.String()
}

// cutPrefix returns the first n bytes of s, backed off to the nearest rune
// boundary so the result is always valid UTF-8.
func cutPrefix(s string, n int) string {
	if n >= len(s) {
		return s
	}
	head := s[:n]
	for len(head) > 0 && !utf8.ValidString(head) {
		head = head[:len(head)-1]
	}
	return head
}

// truncateHead keeps the first n bytes, backing off to the nearest rune
// boundary so the result is always valid UTF-8.
func truncateHead(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return cutPrefix(s, n) + "…(truncated)"
}

// minSummaryBudgetTokens floors the adaptive summary budget.
const minSummaryBudgetTokens = 64

// estimateMessage approximates one message's token cost, including the
// tool-call documents and reasoning replayed on later turns.
func EstimateMessage(m model.D, estimate func(string) int) int {
	n := 0
	if c, _ := m["content"].(string); c != "" {
		n += estimate(c)
	}
	if r, _ := m["reasoning_content"].(string); r != "" {
		n += estimate(r)
	}
	tcs, _ := m["tool_calls"].([]model.D)
	for _, tc := range tcs {
		n += estimateToolCall(tc, estimate)
	}
	return n
}

func estimateToolCall(tc map[string]any, estimate func(string) int) int {
	n := 0
	if fn, ok := tc["function"].(model.D); ok {
		if name, _ := fn["name"].(string); name != "" {
			n += estimate(name)
		}
		if args, _ := fn["arguments"].(string); args != "" {
			n += estimate(args)
		}
	}
	if id, _ := tc["id"].(string); id != "" {
		n += estimate(id)
	}
	return n
}

func EstimateConversation(conversation []model.D, estimate func(string) int) int {
	n := 0
	for _, m := range conversation {
		n += EstimateMessage(m, estimate)
	}
	return n
}

// BuildUserMessage constructs the message to show the agent on what has been done, what needs to be done, and any output handles to query. There is no verbatim tail of the conversation: the summary is the only
// record of prior work, and re-inlining recent tool results is what stopped the rewrite from reclaiming anything.
func BuildUserMessage(task, summary string, handles []HandleInfo, compactionsApplied int) string {
	var b strings.Builder
	b.WriteString("## Task\n" + strings.TrimSpace(task) + "\n\n")
	b.WriteString("## Standing instructions\n" + StandingInstructions + "\n\n")
	if compactionsApplied == 1 {
		b.WriteString("## Work completed so far (after 1 compaction)\n")
	} else {
		fmt.Fprintf(&b, "## Work completed so far (after %d compactions)\n", compactionsApplied)
	}
	b.WriteString(strings.TrimSpace(summary) + "\n\n")
	b.WriteString("## Active output handles\n")
	if len(handles) == 0 {
		b.WriteString("(none — do not call output_search or output_read; run new commands with multipass_exec)\n")
	} else {
		for _, h := range handles {
			fmt.Fprintf(&b, "- %s (%d lines, %d bytes) from `%s`\n", h.ID, h.TotalLines, h.TotalBytes, h.Command)
		}
		b.WriteString("Only use the output_id values listed above; never invent one.\n")
	}
	return b.String()
}

func taskText(conversation []model.D) string {
	for _, m := range conversation {
		if role, _ := m["role"].(string); role == "user" {
			if c, _ := m["content"].(string); c != "" {
				if v, ok := strings.CutPrefix(c, "Task:\n"); ok {
					return strings.TrimSpace(v)
				}
				return c
			}
		}
	}
	return ""
}

// renderBudgetBytes derives the whole-render allowance from the same window the
// agent runs in, so the summarizer request cannot overrun the context on its
// own. Without a known window it falls back to the per-message budget.
func renderBudgetBytes(win Window, budget int, estimate func(string) int) int {
	if win.Size <= 0 || estimate == nil {
		return budget * totalBudgetFactor
	}
	avail := win.Size - win.FixedOverhead - estimate(CompactionSystemPrompt) - summarizerHeadroomTokens
	if avail <= 0 {
		return 0
	}
	return avail * bytesPerToken
}

// Compact rewrites the conversation into a system message plus a summary, or
// returns the original with skipped, reverted, failed, or applied. The third
// return is the summary length in bytes (0 when no summary was produced).
// A revert never counts against retry budgets: it means "not worth it yet",
// and the next attempt with a longer history may win.
func Compact(ctx context.Context, conversation []model.D, handles []HandleInfo, maxSummaryTokens, budget int, win Window, s Summarizer, estimate func(string) int, compactionsApplied int) ([]model.D, string, int) {
	if len(conversation) == 0 {
		return conversation, "skipped", 0
	}
	system := conversation[0]
	task := taskText(conversation)
	maxBytes := renderBudgetBytes(win, budget, estimate)
	if estimate != nil {
		before := EstimateConversation(conversation, estimate)
		afterMin := EstimateMessage(system, estimate) + estimate(BuildUserMessage(task, "", handles, compactionsApplied))
		if afterMin >= before {
			return conversation, "reverted", 0
		}
		if savings := before - afterMin; savings*9/10 < minSummaryBudgetTokens {
			return conversation, "reverted", 0
		} else if maxSummaryTokens > savings*9/10 {
			maxSummaryTokens = savings * 9 / 10
		}
	}
	rendered := RenderHistory(conversation, budget, maxBytes, ToolResultFullText)
	summary, err := s.Summarize(ctx, CompactionSystemPrompt, rendered, maxSummaryTokens)
	if err != nil || strings.TrimSpace(summary) == "" {
		return conversation, "failed", 0
	}
	userMsg := model.D{"role": "user", "content": BuildUserMessage(task, summary, handles, compactionsApplied)}
	compacted := []model.D{system, userMsg}
	if estimate != nil {
		before := EstimateConversation(conversation, estimate)
		after := EstimateMessage(system, estimate) + EstimateMessage(userMsg, estimate)
		if after*10 >= before*9 {
			return conversation, "reverted", len(summary)
		}
	}
	return compacted, "applied", len(summary)
}
