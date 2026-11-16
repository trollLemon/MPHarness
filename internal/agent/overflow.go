package agent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/ardanlabs/kronk/sdk/kronk/model"
	"github.com/rs/zerolog"

	"github.com/trollLemon/MPHarness/v2/internal/otlp"
)

// minOverflowReclaimBytes stops the repair loop once replacing a result no
// longer frees enough context to be worth losing it.
const (
	minOverflowReclaimBytes = 256
	maxOverflowRepairs      = 8
)

type overflowOutcome string

const (
	overflowRecovered overflowOutcome = "recovered"
	overflowAbandoned overflowOutcome = "abandoned"
	overflowExhausted overflowOutcome = "exhausted"
)

var (
	contextOverflowPattern = regexp.MustCompile(`input tokens \[(\d+)\] exceed context window \[(\d+)\]`)
	errContextOverflow     = errors.New("context window exceeded")
)

// contextOverflowError adapts kronk's ErrInvalidRequest, which covers many
// validation failures, into a value callers can match with errors.As.
type contextOverflowError struct {
	prompt int
	window int
	err    error
}

func (e *contextOverflowError) Error() string { return e.err.Error() }
func (e *contextOverflowError) Unwrap() error { return e.err }
func (e *contextOverflowError) Is(target error) bool {
	return target == errContextOverflow
}

// asContextOverflow separates the context-window failure from other
// ErrInvalidRequest causes. Kronk exposes no dedicated sentinel, so the message
// is the only discriminator; the regex stays confined here.
func asContextOverflow(err error) (*contextOverflowError, bool) {
	var overflow *contextOverflowError
	if errors.As(err, &overflow) {
		return overflow, true
	}
	if err == nil || !errors.Is(err, model.ErrInvalidRequest) {
		return nil, false
	}
	m := contextOverflowPattern.FindStringSubmatch(err.Error())
	if m == nil {
		return nil, false
	}
	prompt, promptErr := strconv.Atoi(m[1])
	window, windowErr := strconv.Atoi(m[2])
	if promptErr != nil || windowErr != nil {
		return nil, false
	}
	return &contextOverflowError{prompt: prompt, window: window, err: err}, true
}

// buildContextOverflowContent is the tool result the model sees in place of a
// payload that cannot fit. It stays short because reclaiming context is the
// whole point.
func buildContextOverflowContent(tool string, bytes, budget int) string {
	var msg string
	switch tool {
	case "vm_exec":
		msg = fmt.Sprintf("vm_exec output was %d bytes, over the %d-byte context budget, so it was discarded. Re-run with a narrower command using head, tail, or grep.", bytes, budget)
	case "output_read":
		msg = fmt.Sprintf("output_read returned %d bytes, over the %d-byte context budget, so it was discarded. Request a smaller byte range with offset and limit, or use output_search first.", bytes, budget)
	default:
		msg = fmt.Sprintf("%s result was %d bytes, over the %d-byte context budget, so it was discarded. Narrow the request.", tool, bytes, budget)
	}
	return buildToolErrorContent(errors.New(msg))
}

// discardLargestToolResult rewrites the largest tool result with a short error
// envelope and reports what it reclaimed. Every replacement shrinks the
// conversation; chatWithOverflowRecovery caps how many it will attempt.
func (a *Agent) discardLargestToolResult() (tool string, bytes, reclaimed int, ok bool) {
	bestIdx := -1
	var bestContent, bestName, bestReplacement string

	for i, m := range a.conversation {
		if role, _ := m["role"].(string); role != "tool" {
			continue
		}
		content, _ := m["content"].(string)
		name, _ := m["name"].(string)
		if name == "" {
			name = "tool"
		}
		replacement := buildContextOverflowContent(name, len(content), a.toolResultBudget)
		if len(content)-len(replacement) < minOverflowReclaimBytes {
			continue
		}
		if bestIdx == -1 || len(content) > len(bestContent) {
			bestIdx = i
			bestContent = content
			bestName = name
			bestReplacement = replacement
		}
	}

	if bestIdx == -1 {
		return "", 0, 0, false
	}
	a.conversation[bestIdx]["content"] = bestReplacement
	return bestName, len(bestContent), len(bestContent) - len(bestReplacement), true
}

// chatWithOverflowRecovery retries a chat call after replacing the largest
// tool results with short error envelopes. It logs one overflow event per
// episode and returns the accumulated chat time.
func (a *Agent) chatWithOverflowRecovery(ctx context.Context, iter int, log zerolog.Logger) (model.ChatResponse, time.Duration, error) {
	var (
		resp      model.ChatResponse
		err       error
		first     *contextOverflowError
		replaced  int
		reclaimed int
		dur       time.Duration
		exhausted bool
	)
	for {
		start := time.Now()
		resp, err = a.callChat(ctx, buildChatRequest(a.conversation, a.toolDocs, a.llmConfig), iter)
		dur += time.Since(start)
		overflow, ok := asContextOverflow(err)
		if !ok {
			break
		}
		if first == nil {
			first = overflow
		}
		if replaced >= maxOverflowRepairs {
			exhausted = true
			err = overflowFailure(overflow, replaced, reclaimed)
			break
		}
		_, _, gained, ok := a.discardLargestToolResult()
		if !ok {
			exhausted = true
			err = overflowFailure(overflow, replaced, reclaimed)
			break
		}
		replaced++
		reclaimed += gained
	}
	if first != nil {
		outcome := overflowRecovered
		if err != nil {
			outcome = overflowAbandoned
			if exhausted {
				outcome = overflowExhausted
			}
		}
		logContextOverflow(ctx, log, iter, first, err, replaced, reclaimed, outcome)
	}
	return resp, dur, err
}

func overflowFailure(cause error, replaced, reclaimed int) error {
	return fmt.Errorf("context overflow: discarded %d tool results (%d bytes), request still exceeds the model window: %w", replaced, reclaimed, cause)
}

func logContextOverflow(ctx context.Context, log zerolog.Logger, iter int, first *contextOverflowError, final error, replaced, reclaimed int, outcome overflowOutcome) {
	level := zerolog.WarnLevel
	if outcome != overflowRecovered {
		level = zerolog.ErrorLevel
	}
	fields := map[string]any{
		"event":           eventContextOverflow,
		"iter":            iter + 1,
		"outcome":         outcome,
		"replaced":        replaced,
		"reclaimed_bytes": reclaimed,
		"prompt_tokens":   first.prompt,
		"ctx_window":      first.window,
	}
	if final != nil {
		fields["error"] = final
	}
	otlp.LogEvent(ctx, log, level, "context overflow", fields)
}
