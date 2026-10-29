package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ardanlabs/kronk/sdk/kronk/model"
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

const minSummaryBytes = 64

// errEmptySummary marks a summarizer reply that carried no content, so
// compaction can retry it with a corrective prompt instead of treating it like
// a transport failure.
var errEmptySummary = errors.New("summary response contained no content")

type summarizer interface {
	Summarize(ctx context.Context, systemPrompt, renderedHistory string) (string, error)
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

// renderHistory creates a string containing the message history up to this point.
func renderHistory(conversation []model.D) string {
	var b strings.Builder
	for _, m := range conversation {
		role, _ := m["role"].(string)
		switch role {
		case "assistant":
			if c, _ := m["content"].(string); c != "" {
				b.WriteString(c + "\n")
			}
			if r, _ := m["reasoning_content"].(string); r != "" {
				b.WriteString(r + "\n")
			}
			if tcs, ok := m["tool_calls"].([]model.D); ok {
				for _, tc := range tcs {
					if fn, ok := tc["function"].(model.D); ok {
						name, _ := fn["name"].(string)
						args, _ := fn["arguments"].(string)
						fmt.Fprintf(&b, "tool: %s(%s)\n", name, args)
					}
				}
			}
		case "tool":
			name, _ := m["name"].(string)
			content, _ := m["content"].(string)
			fmt.Fprintf(&b, "tool result %s: %s\n", name, content)
		case "user":
			if c, _ := m["content"].(string); c != "" {
				b.WriteString("user: " + c + "\n")
			}
		}
	}
	return b.String()
}

func buildUserMessage(task, summary string, handles []handleInfo, compactionsApplied int) string {
	var b strings.Builder
	b.WriteString("## Task\n" + strings.TrimSpace(task) + "\n\n")
	if compactionsApplied == 1 {
		b.WriteString("## Work completed so far (after 1 compaction)\n")
	} else {
		fmt.Fprintf(&b, "## Work completed so far (after %d compactions)\n", compactionsApplied)
	}
	b.WriteString(strings.TrimSpace(summary) + "\n\n")
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
				return c
			}
		}
	}
	return ""
}

// compactConversation rewrites the conversation into a system message plus a
// summary, or returns the original with failed. The third return is the
// summary length in bytes (0 when no summary was produced). The error
// explains a failed outcome and is nil otherwise. The caller (maybeCompact)
// only reaches this after shouldCompact confirms a positive context size, so
// conversation is always non-empty: Execute seeds it with a system and user
// message before the first call.
func compactConversation(ctx context.Context, conversation []model.D, handles []handleInfo, maxSummaryTokens, budget int, win compactionWindow, s summarizer, compactionsApplied int) ([]model.D, string, int, error) {
	system := conversation[0]
	task := taskText(conversation)

	rendered := renderHistory(conversation)
	summary, err := s.Summarize(ctx, compactionSystemPrompt, rendered)
	if err != nil && !errors.Is(err, errEmptySummary) {
		return conversation, "failed", 0, fmt.Errorf("summarize: %w", err)
	}
	if reason := summaryRejection(err, summary); reason != nil {
		summary, err = repairSummary(ctx, s, conversation, reason)
		if err != nil {
			return conversation, "failed", 0, fmt.Errorf("summary repair: %w", err)
		}
		if reason := summaryRejection(nil, summary); reason != nil {
			return conversation, "failed", 0, fmt.Errorf("summary rejected after repair: %w", reason)
		}
	}
	userMsg := model.D{"role": "user", "content": buildUserMessage(task, summary, handles, compactionsApplied)}
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

func repairSummary(ctx context.Context, s summarizer, conversation []model.D, reason error) (string, error) {
	system := compactionSystemPrompt + "\n\nYour previous handover was rejected: " + reason.Error() + " Return a corrected handover with all three headings, using \"- None.\" under any empty section."
	history := renderHistory(conversation)
	return s.Summarize(ctx, system, history)
}
