package compact

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ardanlabs/kronk/sdk/kronk/model"

	"github.com/trollLemon/MPHarness/internal/textutil"
)

const CompactionSystemPrompt = `You are summarising an autonomous VM-operator agent's conversation so it can continue with a smaller context.

Return a handover using exactly these three headings, in this order:

## Done
- Every task or step attempted, whether it passed or failed.
- For each one, include the observed result with enough concrete detail to verify or continue the work. Preserve exact values from the command output, for example HTTP status codes, file paths, counts, version strings, or error text; never paraphrase or round them.
- Keep any other facts the next turn needs, such as decisions made, assumptions, and blockers.

## In progress
- Work that was started but not finished, and the exact point it stopped at.

## Not started
- Work from the original task that has not been attempted yet.

Rules:
- Every handover must include all three headings, even when a section is empty. Under an empty heading write "- None." instead of omitting the heading.
- Never rename, merge, or reorder the headings. Use the exact text "## Done", "## In progress", and "## Not started".
- Before replying, check that all three headings are present.
- This summary is the only record of prior work. Anything you leave out cannot be recovered.
- Do not reply with only a heading, a single word, or an empty section.
- Captured output handles are referenced by their output_id. Copy every output_id verbatim, character for character, and say what each one contains. Never invent, shorten, or reformat an id.
- Do not invent results, and do not claim work succeeded that the log shows failing.
- Summarize inlined command outputs so their result is articulated without including the whole payload.
- Plain prose and bullets.

Make sure the handover is formatted clearly so an agent can continue without confusion.
`

const StandingInstructions = `Operate asynchronously without user interaction. Prior messages are your own execution logs. Stop immediately on any permission or policy denial without workarounds. Inspect every result's status field. Always finish with a tool-call-free completion summary.`

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
func RenderHistory(conversation []model.D, budget, maxBytes int) string {
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

func buildUserMessage(task, summary, recentResults string, handles []HandleInfo, compactionsApplied int) string {
	var b strings.Builder
	b.WriteString("## Task\n" + strings.TrimSpace(task) + "\n\n")
	b.WriteString("## Standing instructions\n" + StandingInstructions + "\n\n")
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

// ErrEmptySummary marks a summarizer reply that carried no content, so
// compaction can retry it with a corrective prompt instead of treating it like
// a transport failure.
var ErrEmptySummary = errors.New("summary response contained no content")

// Compact rewrites the conversation into a system message plus a summary, or
// returns the original with skipped, reverted, failed, or applied. The third
// return is the summary length in bytes (0 when no summary was produced). The
// error explains a failed outcome and is nil otherwise.
// A revert never counts against retry budgets: it means "not worth it yet",
// and the next attempt with a longer history may win.
func Compact(ctx context.Context, conversation []model.D, handles []HandleInfo, maxSummaryTokens, budget int, win Window, s Summarizer, estimate func(string) int, compactionsApplied int) ([]model.D, string, int, error) {
	if len(conversation) == 0 {
		return conversation, "skipped", 0, nil
	}
	system := conversation[0]
	task := taskText(conversation)
	maxBytes := renderBudgetBytes(win, budget, estimate)
	recentBudget := maxBytes / 4
	if recentBudget > recentResultsMaxBytes {
		recentBudget = recentResultsMaxBytes
	}
	recent := recentResults(conversation, recentBudget)
	if estimate != nil {
		before := EstimateConversation(conversation, estimate)
		afterMin := EstimateMessage(system, estimate) + estimate(buildUserMessage(task, "", recent, handles, compactionsApplied))
		if afterMin >= before {
			return conversation, "reverted", 0, nil
		}
		if savings := before - afterMin; savings*9/10 < minSummaryBudgetTokens {
			return conversation, "reverted", 0, nil
		} else if maxSummaryTokens > savings*9/10 {
			maxSummaryTokens = savings * 9 / 10
		}
	}
	worthIt := func(summary string) bool {
		if estimate == nil {
			return true
		}
		before := EstimateConversation(conversation, estimate)
		after := EstimateMessage(system, estimate) + estimate(buildUserMessage(task, summary, recent, handles, compactionsApplied))
		return after*10 < before*9
	}

	rendered := RenderHistory(conversation, budget, maxBytes)
	summary, err := s.Summarize(ctx, CompactionSystemPrompt, rendered, maxSummaryTokens)
	if err != nil && !errors.Is(err, ErrEmptySummary) {
		return conversation, "failed", 0, fmt.Errorf("summarize: %w", err)
	}
	if !worthIt(summary) {
		return conversation, "reverted", len(summary), nil
	}
	if reason := summaryRejection(err, summary); reason != nil {
		summary, err = repairSummary(ctx, s, conversation, budget, maxBytes, maxSummaryTokens, summary, reason)
		if err != nil {
			return conversation, "failed", 0, fmt.Errorf("summary repair: %w", err)
		}
		if reason := summaryRejection(nil, summary); reason != nil {
			return conversation, "failed", 0, fmt.Errorf("summary rejected after repair: %w", reason)
		}
		if !worthIt(summary) {
			return conversation, "reverted", len(summary), nil
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

func repairSummary(ctx context.Context, s Summarizer, conversation []model.D, budget, maxBytes, maxTokens int, previous string, reason error) (string, error) {
	system := CompactionSystemPrompt + "\n\nYour previous handover was rejected: " + reason.Error() + " Return a corrected handover with all three headings, using \"- None.\" under any empty section."
	history := RenderHistory(conversation, budget, maxBytes)
	if previous = strings.TrimSpace(previous); previous != "" {
		block := "\n\n## Previous rejected handover\n" + previous
		if maxBytes > len(block) {
			history = RenderHistory(conversation, budget, maxBytes-len(block)) + block
		}
	}
	return s.Summarize(ctx, system, history, maxTokens)
}
