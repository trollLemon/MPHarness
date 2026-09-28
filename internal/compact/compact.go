package compact

import (
	"context"
	"fmt"
	"strings"

	"github.com/ardanlabs/kronk/sdk/kronk/model"
)

const CompactionSystemPrompt = `You are summarising an autonomous VM-operator agent's conversation so it can continue with a smaller context. Write a concise handover: what was attempted, what worked, key outputs, what remains. Weave in any captured output handles by id with what each contains. Do not invent results. Plain prose, no tool calls.`

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

// BuildUserMessage constructs the message to show the agent on what has been done, what needs to be done, and any output handles to query.
func BuildUserMessage(task, summary string, handles []HandleInfo, recent string) string {
	var b strings.Builder
	b.WriteString("## Task\n" + strings.TrimSpace(task) + "\n\n")
	b.WriteString("## Standing instructions\n" + StandingInstructions + "\n\n")
	b.WriteString("## Work completed so far\n" + strings.TrimSpace(summary) + "\n\n")
	b.WriteString("## Active output handles\n")
	if len(handles) == 0 {
		b.WriteString("(none)\n")
	} else {
		for _, h := range handles {
			fmt.Fprintf(&b, "- %s (%d lines, %d bytes) from `%s`\n", h.ID, h.TotalLines, h.TotalBytes, h.Command)
		}
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
// returns the original with skipped, reverted, failed, or applied.
func Compact(ctx context.Context, conversation []model.D, handles []HandleInfo, maxSummaryTokens, recentTurns, budget int, s Summarizer, estimate func(string) int) ([]model.D, string) {
	if len(conversation) == 0 {
		return conversation, "skipped"
	}
	system := conversation[0]
	rendered := RenderHistory(conversation, budget)
	summary, err := s.Summarize(ctx, CompactionSystemPrompt, rendered, maxSummaryTokens)
	if err != nil || strings.TrimSpace(summary) == "" {
		return conversation, "failed"
	}
	task := taskText(conversation)
	recent := recentText(conversation, recentTurns, budget)
	userMsg := model.D{"role": "user", "content": BuildUserMessage(task, summary, handles, recent)}
	compacted := []model.D{system, userMsg}
	if estimate != nil {
		before := 0
		for _, m := range conversation {
			if c, _ := m["content"].(string); c != "" {
				before += estimate(c)
			}
		}
		after := 0
		for _, m := range compacted {
			if c, _ := m["content"].(string); c != "" {
				after += estimate(c)
			}
		}
		if after >= before {
			return conversation, "reverted"
		}
	}
	return compacted, "applied"
}
