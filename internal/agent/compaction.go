package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ardanlabs/kronk/sdk/kronk/model"

	"github.com/trollLemon/MPHarness/internal/textutil"
)

const compactionSystemPrompt = `You are summarising an autonomous VM agent's conversation so it can continue with a smaller context.

Return a handover with exactly these headings, in order:
## Done
- Every attempted step, passed or failed, with the observed result. Preserve exact values from command output (status codes, paths, counts, versions, error text); never paraphrase or round them. Include decisions, assumptions, and blockers.
## In progress
- Work started but not finished, and the exact point it stopped.
## Not started
- Original tasks not yet attempted.

Rules:
- Use the exact heading text, in order, even when a section is empty; write "- None." under an empty heading. Before replying, check that all three are present, and do not reply with only a heading, a single word, or an empty section.
- This summary is the only record of prior work; anything omitted cannot be recovered. Do not invent results or claim failed work succeeded.
- Copy every captured output_id verbatim and say what it contains. Do not invent, shorten, or reformat ids.
- Summarize inline command output; do not copy the full payload.
`

type summarizer interface {
	Summarize(ctx context.Context, systemPrompt, renderedHistory string, maxTokens int) (string, error)
}

type handleInfo struct {
	ID         string
	TotalLines int
	TotalBytes int64
	Command    string
}

// compactionWindow describes the context budget one compaction decision has to
// respect: the live window, and the per-request cost outside the messages
// themselves (tool schemas and template framing).
type compactionWindow struct {
	Size          int
	FixedOverhead int
}

// shouldCompact reports whether token usage has reached threshold of the window.
func shouldCompact(contextTokens, window int64, threshold float64) bool {
	if window <= 0 || contextTokens <= 0 {
		return false
	}
	return float64(contextTokens) >= threshold*float64(window)
}

// totalBudgetFactor bounds the whole render when no window is known. budget is a
// per-message allowance, so on its own a long conversation renders to
// len(messages)*budget and the summarizer prompt grows without limit.
const totalBudgetFactor = 8

// summarizerHeadroomTokens covers what neither side of the window bound accounts
// for: kronk's chat framing for the summarizer request. The summarizer prompt
// has to land inside the window, not on its edge.
const summarizerHeadroomTokens = 64

// renderHistory creates a string containing the message history up to this point.
// NOTE: messages are truncated to budget each, except tool results, which the
// output store is responsible for keeping out of the conversation. The total is
// capped at maxBytes so the render cannot outgrow the summarizer's window.
func renderHistory(conversation []model.D, budget, maxBytes int) string {
	var b strings.Builder
	remaining := maxBytes
	write := func(s string) {
		if remaining <= 0 {
			return
		}
		if len(s) > remaining {
			s = textutil.CutPrefix(s, remaining)
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
			write(fmt.Sprintf("tool result %s: %s\n", name, content))
		case "user":
			if c, _ := m["content"].(string); c != "" {
				write("user: " + truncateHead(c, budget) + "\n")
			}
		}
	}
	return b.String()
}

// truncateHead keeps the first n bytes, backing off to the nearest rune
// boundary so the result is always valid UTF-8.
func truncateHead(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return textutil.CutPrefix(s, n) + "…(truncated)"
}

func buildUserMessage(task, summary, recentResults string, handles []handleInfo, compactionsApplied int) string {
	var b strings.Builder
	b.WriteString("## Task\n" + strings.TrimSpace(task) + "\n\n")
	if compactionsApplied == 1 {
		b.WriteString("## Work completed so far (after 1 compaction)\n")
	} else {
		fmt.Fprintf(&b, "## Work completed so far (after %d compactions)\n", compactionsApplied)
	}
	b.WriteString(strings.TrimSpace(summary) + "\n\n")
	if recent := strings.TrimSpace(recentResults); recent != "" {
		b.WriteString("## Recent command results\n")
		b.WriteString(recent + "\n\n")
	}
	b.WriteString("## Active output handles\n")
	if len(handles) == 0 {
		b.WriteString("(none; do not call output_search or output_read, run new commands with vm_exec)\n")
	} else {
		for _, h := range handles {
			fmt.Fprintf(&b, "- %s (%d lines, %d bytes) from `%s`\n", h.ID, h.TotalLines, h.TotalBytes, h.Command)
		}
		b.WriteString("Only use the output_id values listed above; never invent one.\n")
	}
	return b.String()
}

const (
	recentResultBytes     = 512
	recentResultsMax      = 8
	recentResultsMaxBytes = 4096
)

// recentResults renders the most recent tool results as bounded evidence for the
// handover. The summarizer is not trusted to preserve exact values on its own.
func recentResults(conversation []model.D, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	commands := make(map[string]string)
	type result struct {
		command string
		output  string
	}
	var results []result
	for _, m := range conversation {
		role, _ := m["role"].(string)
		switch role {
		case "assistant":
			tcs, _ := m["tool_calls"].([]model.D)
			for _, tc := range tcs {
				id, _ := tc["id"].(string)
				fn, _ := tc["function"].(model.D)
				name, _ := fn["name"].(string)
				args, _ := fn["arguments"].(string)
				if id != "" && name != "" {
					commands[id] = strings.TrimSpace(name + " " + args)
				}
			}
		case "tool":
			content, _ := m["content"].(string)
			if strings.TrimSpace(content) == "" {
				continue
			}
			id, _ := m["tool_call_id"].(string)
			command := commands[id]
			if command == "" {
				command, _ = m["name"].(string)
			}
			if command == "" {
				command = "command"
			}
			results = append(results, result{command: command, output: content})
		}
	}

	remaining := maxBytes
	kept := 0
	var blocks []string
	for i := len(results) - 1; i >= 0 && kept < recentResultsMax && remaining > 0; i-- {
		block := fmt.Sprintf("$ %s\n%s\n\n", results[i].command, truncateHead(results[i].output, recentResultBytes))
		if len(block) > remaining {
			block = textutil.CutPrefix(block, remaining)
		}
		blocks = append(blocks, block)
		remaining -= len(block)
		kept++
	}
	var b strings.Builder
	for i := len(blocks) - 1; i >= 0; i-- {
		b.WriteString(blocks[i])
	}
	return strings.TrimSpace(b.String())
}

const minSummaryBytes = 64

func validateSummary(summary string) error {
	trimmed := strings.TrimSpace(summary)
	if len(trimmed) < minSummaryBytes {
		return fmt.Errorf("summary is %d bytes, want at least %d", len(trimmed), minSummaryBytes)
	}
	return nil
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
func renderBudgetBytes(win compactionWindow, budget int) int {
	if win.Size <= 0 {
		return budget * totalBudgetFactor
	}
	avail := win.Size - win.FixedOverhead - len(compactionSystemPrompt)/bytesPerToken - summarizerHeadroomTokens
	if avail <= 0 {
		return 0
	}
	return avail * bytesPerToken
}

// errEmptySummary marks a summarizer reply that carried no content, so
// compaction can retry it with a corrective prompt instead of treating it like
// a transport failure.
var errEmptySummary = errors.New("summary response contained no content")

// compactConversation rewrites the conversation into a system message plus a
// summary, or returns the original with skipped or failed. The third return is
// the summary length in bytes (0 when no summary was produced). The error
// explains a failed outcome and is nil otherwise.
func compactConversation(ctx context.Context, conversation []model.D, handles []handleInfo, maxSummaryTokens, budget int, win compactionWindow, s summarizer, compactionsApplied int) ([]model.D, string, int, error) {
	if len(conversation) == 0 {
		return conversation, "skipped", 0, nil
	}
	system := conversation[0]
	task := taskText(conversation)
	maxBytes := renderBudgetBytes(win, budget)
	recentBudget := maxBytes / 4
	if recentBudget > recentResultsMaxBytes {
		recentBudget = recentResultsMaxBytes
	}
	recent := recentResults(conversation, recentBudget)

	rendered := renderHistory(conversation, budget, maxBytes)
	summary, err := s.Summarize(ctx, compactionSystemPrompt, rendered, maxSummaryTokens)
	if err != nil && !errors.Is(err, errEmptySummary) {
		return conversation, "failed", 0, fmt.Errorf("summarize: %w", err)
	}
	if reason := summaryRejection(err, summary); reason != nil {
		summary, err = repairSummary(ctx, s, conversation, budget, maxBytes, maxSummaryTokens, summary, reason)
		if err != nil {
			return conversation, "failed", 0, fmt.Errorf("summary repair: %w", err)
		}
		if reason := summaryRejection(nil, summary); reason != nil {
			return conversation, "failed", 0, fmt.Errorf("summary rejected after repair: %w", reason)
		}
	}
	userMsg := model.D{"role": "user", "content": buildUserMessage(task, summary, recent, handles, compactionsApplied)}
	return []model.D{system, userMsg}, "applied", len(summary), nil
}

func summaryRejection(err error, summary string) error {
	if err != nil {
		return err
	}
	if strings.TrimSpace(summary) == "" {
		return errors.New("summary is empty")
	}
	return validateSummary(summary)
}

func repairSummary(ctx context.Context, s summarizer, conversation []model.D, budget, maxBytes, maxTokens int, previous string, reason error) (string, error) {
	system := compactionSystemPrompt + "\n\nYour previous handover was rejected: " + reason.Error() + " Return a corrected handover with all three headings, using \"- None.\" under any empty section."
	history := renderHistory(conversation, budget, maxBytes)
	if previous = strings.TrimSpace(previous); previous != "" {
		block := "\n\n## Previous rejected handover\n" + previous
		if maxBytes > len(block) {
			history = renderHistory(conversation, budget, maxBytes-len(block)) + block
		}
	}
	return s.Summarize(ctx, system, history, maxTokens)
}
