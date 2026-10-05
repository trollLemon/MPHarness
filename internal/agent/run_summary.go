package agent

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	mphotel "github.com/trollLemon/MPHarness/internal/otel"
)

// runOutcome is the single terminal state of a run. It is recorded once, on
// the run.finished gauge, so a run that stops for any reason is
// distinguishable from one that stopped cleanly.
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

// runTotals is the per-run aggregate. It is assembled from values the agent
// already tallies rather than from the metric stream, because the gauges are
// recorded once at the end and cannot be derived from cumulative histograms.
//
// NewAgent must build it: ToolCalls and ToolFailures are written by
// increment, so a zero runTotals panics on first use.
type runTotals struct {
	Iterations       int
	PromptTokens     int64
	CompletionTokens int64
	ReclaimedTokens  int64
	ToolCalls        map[string]int
	ToolFailures     map[string]int
}

func (a *Agent) recordRunSummary(ctx context.Context, outcome runOutcome, elapsed time.Duration) {
	attrs := func(extra ...attribute.KeyValue) metric.MeasurementOption {
		return metric.WithAttributes(mphotel.RunAttrs(ctx, extra...)...)
	}

	// Float seconds, consistent with every other duration here, so a
	// sub-second run is not rounded away.
	if runDurationGauge != nil {
		runDurationGauge.Record(ctx, elapsed.Seconds(), attrs())
	}
	if runIterationsGauge != nil {
		runIterationsGauge.Record(ctx, int64(a.totals.Iterations), attrs())
	}
	if runCompactionsGauge != nil {
		runCompactionsGauge.Record(ctx, int64(a.compactionsApplied), attrs())
	}
	if runFinishedGauge != nil {
		runFinishedGauge.Record(ctx, 1, attrs(attribute.String("outcome", string(outcome))))
	}
	if runTokensGauge != nil {
		runTokensGauge.Record(ctx, a.totals.PromptTokens, attrs(attribute.String("tokens.type", "prompt")))
		runTokensGauge.Record(ctx, a.totals.CompletionTokens, attrs(attribute.String("tokens.type", "completion")))
		runTokensGauge.Record(ctx, a.totals.ReclaimedTokens, attrs(attribute.String("tokens.type", "reclaimed")))
	}
	for name, n := range a.totals.ToolCalls {
		if runToolCallsGauge != nil {
			runToolCallsGauge.Record(ctx, int64(n), attrs(attribute.String("mph.tool.name", name)))
		}
	}
	for name, n := range a.totals.ToolFailures {
		if runToolFailuresGauge != nil {
			runToolFailuresGauge.Record(ctx, int64(n), attrs(attribute.String("mph.tool.name", name)))
		}
	}
	// Recording the terminal zero is what stops a finished run's context line
	// from freezing at its final percentage in the Prometheus exporter, which
	// would otherwise read as still running forever.
	if contextTokensGauge != nil {
		contextTokensGauge.Record(ctx, 0, metric.WithAttributes(mphotel.RunAttrs(ctx)...))
	}
}
