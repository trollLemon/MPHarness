package agent

import (
	"context"
	"errors"
	"time"

	"github.com/rs/zerolog"

	mphotel "github.com/trollLemon/MPHarness/internal/otel"
)

// runOutcome is the single terminal state of a run. It is logged once, on the
// run_summary event, so a run that stops for any reason is distinguishable
// from one that stopped cleanly.
type runOutcome string

const (
	outcomeOK              runOutcome = "ok"
	outcomeError           runOutcome = "error"
	outcomeBudgetExhausted runOutcome = "budget_exhausted"
	outcomeTimeout         runOutcome = "timeout"
)

func classifyOutcome(ctx context.Context, err error, exhausted bool) runOutcome {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return outcomeTimeout
	}
	if err != nil {
		return outcomeError
	}
	if exhausted {
		return outcomeBudgetExhausted
	}
	return outcomeOK
}

// runTotals is the per-run aggregate reported once by the run_summary event.
type runTotals struct {
	Iterations       int
	PromptTokens     int64
	CompletionTokens int64
	ReclaimedTokens  int64
	ToolCalls        int
	ToolFailures     int
}

func (a *Agent) logRunSummary(ctx context.Context, outcome runOutcome, elapsed time.Duration) {
	var avgIterMS int64
	if a.totals.Iterations > 0 {
		avgIterMS = elapsed.Milliseconds() / int64(a.totals.Iterations)
	}
	fields := map[string]any{
		"event":             eventRunSummary,
		"outcome":           string(outcome),
		"iterations":        a.totals.Iterations,
		"compactions":       a.compactionsApplied,
		"dur_ms":            elapsed.Milliseconds(),
		"avg_iter_ms":       avgIterMS,
		"prompt_tokens":     a.totals.PromptTokens,
		"completion_tokens": a.totals.CompletionTokens,
		"reclaimed_tokens":  a.totals.ReclaimedTokens,
		"tool_calls":        a.totals.ToolCalls,
		"tool_failures":     a.totals.ToolFailures,
	}
	if a.krn != nil {
		if window := a.krn.ModelConfig().ContextWindow(); window > 0 {
			fields["ctx_window"] = window
		}
	}
	mphotel.LogEvent(ctx, a.log.With().Ctx(ctx).Logger(), zerolog.InfoLevel, "run summary", fields)
}
