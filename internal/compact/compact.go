package compact

import (
	"context"
	"fmt"
	"strings"

	"github.com/ardanlabs/kronk/sdk/kronk/model"
)

const CompactionSystemPrompt = `You are summarising an autonomous VM-operator agent's conversation so it can continue with a smaller context. Write a concise handover with two sections: Completed (what was attempted, what worked, key outputs; weave in any captured output handles by id with what each contains) and Remaining (what still needs doing and the suggested next step). Do not invent results. Plain prose, no tool calls.`

const StandingInstructions = `Operate asynchronously without user interaction. Prior messages are your own execution logs. Stop immediately on any permission or policy denial without workarounds. Inspect every result's status field. Always finish with a tool-call-free completion summary.`

type Summarizer interface {
	Summarize(ctx context.Context, systemPrompt, renderedHistory string, maxTokens int) (string, error)
}

type HandleInfo struct {
	ID         string
	Path       string
	TotalLines int
	TotalBytes int64
	Command    string
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

// RenderHistory creates a string containing the message history up to this point.
// NOTE: some messages are truncated based on the budget variable.
func RenderHistory(conversation []model.D, budget int) string {
	var b strings.Builder
	for _, m := range conversation {
		role, _ := m["role"].(string)
		switch role {
		case "assistant":
			if c, _ := m["content"].(string); c != "" {
				b.WriteString(c + "\n")
			}
			if r, _ := m["reasoning_content"].(string); r != "" {
				b.WriteString(truncateHead(r, budget/4) + "\n")
			}
			if tcs, ok := m["tool_calls"].([]model.D); ok {
				for _, tc := range tcs {
					if fn, ok := tc["function"].(model.D); ok {
						name, _ := fn["name"].(string)
						args, _ := fn["arguments"].(string)
						cmd := args
						if len(cmd) > 200 {
							cmd = cmd[:200] + "…"
						}
						fmt.Fprintf(&b, "tool: %s(%s)\n", name, cmd)
					}
				}
			}
		case "tool":
			name, _ := m["name"].(string)
			content, _ := m["content"].(string)
			fmt.Fprintf(&b, "tool result %s: %s\n", name, truncateHead(content, budget))
		case "user":
			if c, _ := m["content"].(string); c != "" {
				b.WriteString("user: " + truncateHead(c, budget) + "\n")
			}
		}
	}
	return b.String()
}

func truncateHead(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}

// minSummaryBudgetTokens floors the adaptive summary budget.
const minSummaryBudgetTokens = 64

// estimateMessage approximates one message's token cost, including the
// tool-call documents and reasoning replayed on later turns.
func estimateMessage(m model.D, estimate func(string) int) int {
	n := 0
	if c, _ := m["content"].(string); c != "" {
		n += estimate(c)
	}
	if r, _ := m["reasoning_content"].(string); r != "" {
		n += estimate(r)
	}
	switch tcs := m["tool_calls"].(type) {
	case []model.D:
		for _, tc := range tcs {
			n += estimateToolCall(tc, estimate)
		}
	case []any:
		for _, raw := range tcs {
			if tc, ok := raw.(map[string]any); ok {
				n += estimateToolCall(tc, estimate)
			}
		}
	}
	return n
}

func estimateToolCall(tc map[string]any, estimate func(string) int) int {
	n := 0
	fn, _ := tc["function"].(map[string]any)
	if fn == nil {
		if fmd, ok := tc["function"].(model.D); ok {
			fn = fmd
		}
	}
	if fn != nil {
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

func estimateConversation(conversation []model.D, estimate func(string) int) int {
	n := 0
	for _, m := range conversation {
		n += estimateMessage(m, estimate)
	}
	return n
}

// BuildUserMessage constructs the message to show the agent on what has been done, what needs to be done, and any output handles to query.
func BuildUserMessage(task, summary string, handles []HandleInfo, recent string, compactionsApplied int) string {
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
	b.WriteString("\n## Recent activity\n" + recent + "\n")
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

func recentText(conversation []model.D, recentTurns, budget int) string {
	if recentTurns <= 0 || len(conversation) == 0 {
		return "(omitted)"
	}
	start := 0
	seen := 0
	for i := len(conversation) - 1; i >= 0; i-- {
		if role, _ := conversation[i]["role"].(string); role == "assistant" {
			seen++
			if seen == recentTurns {
				start = i
				break
			}
		}
	}
	return RenderHistory(conversation[start:], budget)
}

// Compact rewrites the conversation into a system message plus a summary, or
// returns the original with skipped, reverted, failed, or applied. The third
// return is the summary length in bytes (0 when no summary was produced), so
// callers can log what the summarizer emitted even when the rewrite reverts.
// A revert never counts against retry budgets: it means "not worth it yet",
// and the next attempt with a longer history may win.
func Compact(ctx context.Context, conversation []model.D, handles []HandleInfo, maxSummaryTokens, recentTurns, budget int, s Summarizer, estimate func(string) int, compactionsApplied int) ([]model.D, string, int) {
	if len(conversation) == 0 {
		return conversation, "skipped", 0
	}
	system := conversation[0]
	task := taskText(conversation)
	recent := recentText(conversation, recentTurns, budget)
	if estimate != nil {
		before := estimateConversation(conversation, estimate)
		afterMin := estimateMessage(system, estimate) + estimate(BuildUserMessage(task, "", handles, recent, compactionsApplied))
		if afterMin >= before {
			return conversation, "reverted", 0
		}
		if savings := before - afterMin; savings*9/10 < minSummaryBudgetTokens {
			return conversation, "reverted", 0
		} else if maxSummaryTokens > savings*9/10 {
			maxSummaryTokens = savings * 9 / 10
		}
	}
	rendered := RenderHistory(conversation, budget)
	summary, err := s.Summarize(ctx, CompactionSystemPrompt, rendered, maxSummaryTokens)
	if err != nil || strings.TrimSpace(summary) == "" {
		return conversation, "failed", 0
	}
	userMsg := model.D{"role": "user", "content": BuildUserMessage(task, summary, handles, recent, compactionsApplied)}
	compacted := []model.D{system, userMsg}
	if estimate != nil {
		before := estimateConversation(conversation, estimate)
		after := estimateMessage(system, estimate) + estimateMessage(userMsg, estimate)
		if after*10 >= before*9 {
			return conversation, "reverted", len(summary)
		}
	}
	return compacted, "applied", len(summary)
}
