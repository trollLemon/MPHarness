package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/ardanlabs/kronk/sdk/kronk/model"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	mphotel "github.com/trollLemon/MPHarness/internal/otel"
	"github.com/trollLemon/MPHarness/internal/textutil"
)

var (
	contextTokensGauge metric.Int64Gauge
	contextWindowGauge metric.Int64Gauge

	iterationDurationHist metric.Float64Histogram
	chatDurationHist      metric.Float64Histogram
	prefillTTFTHist       metric.Float64Histogram
	prefillCachedHist     metric.Int64Histogram
	agentTokensHist       metric.Int64Histogram
	reasoningTokensHist   metric.Int64Histogram
	tpsHist               metric.Float64Histogram

	toolDurationHist    metric.Float64Histogram
	toolCallsCounter    metric.Int64Counter
	toolFailuresCounter metric.Int64Counter

	compactionsCounter     metric.Int64Counter
	compactionDurationHist metric.Float64Histogram
	tokensReclaimedHist    metric.Int64Histogram

	runDurationGauge     metric.Float64Gauge
	runTokensGauge       metric.Int64Gauge
	runIterationsGauge   metric.Int64Gauge
	runToolCallsGauge    metric.Int64Gauge
	runToolFailuresGauge metric.Int64Gauge
	runCompactionsGauge  metric.Int64Gauge
	runFinishedGauge     metric.Int64Gauge
)

// runIdentityKey carries the run's telemetry identity in the context so every
// helper reached during a run can attribute its measurements without the
// values being threaded through signatures.
type runIdentityKey struct{}

type runIdentityValue struct {
	id   string
	name string
}

func withRunIdentity(ctx context.Context, runLabel, runName string) context.Context {
	return context.WithValue(ctx, runIdentityKey{}, runIdentityValue{id: runLabel, name: runName})
}

func runIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(runIdentityKey{}).(runIdentityValue)
	return v.id
}

// runAttrs tags a measurement with the run identity. Metric measurements carry
// the run UUID deliberately: a developer-facing stack needs per-run series to
// compare runs, and the cardinality is bounded by how many runs a person
// actually starts.
func runAttrs(ctx context.Context, extra ...attribute.KeyValue) []attribute.KeyValue {
	v, _ := ctx.Value(runIdentityKey{}).(runIdentityValue)
	if v.id == "" && v.name == "" {
		return extra
	}
	attrs := make([]attribute.KeyValue, 0, len(extra)+2)
	if v.id != "" {
		attrs = append(attrs, attribute.String("mph.run.id", v.id))
	}
	if v.name != "" {
		attrs = append(attrs, attribute.String("mph.run.name", v.name))
	}
	return append(attrs, extra...)
}

func runSpanAttrs(ctx context.Context, extra ...attribute.KeyValue) []attribute.KeyValue {
	attrs := extra
	if runID := runIDFromContext(ctx); runID != "" {
		attrs = append(attrs, attribute.String("mph.run.id", runID))
	}
	return attrs
}

type iterationKey struct{}

// withIteration stores iter+1 because iteration labels are 1-based while the
// agent's loop counter is 0-based; converting here keeps the two from drifting.
func withIteration(ctx context.Context, iter int) context.Context {
	return context.WithValue(ctx, iterationKey{}, iter+1)
}

func iterationFromContext(ctx context.Context) (int, bool) {
	v, ok := ctx.Value(iterationKey{}).(int)
	return v, ok
}

// eventAttrs adds the iteration dimension to a run-scoped attribute set. Level
// metrics must not use it: one series per iteration turns a level into a comb
// of frozen lines instead of a readable one.
func eventAttrs(ctx context.Context, extra ...attribute.KeyValue) []attribute.KeyValue {
	if iter, ok := iterationFromContext(ctx); ok {
		extra = append(extra, attribute.Int("mph.iteration", iter))
	}
	return runAttrs(ctx, extra...)
}

// Boundaries are explicit because OpenTelemetry's defaults top out at 10s and
// a real iteration on testing/longrun.yaml runs about 90 seconds.
var (
	durationBuckets     = []float64{1, 2, 5, 10, 20, 30, 45, 60, 90, 120, 180, 300}
	ttftBuckets         = []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 40, 60, 120}
	toolDurationBuckets = []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60}
)

func getAgentTracer() trace.Tracer {
	return otel.Tracer("mph")
}

func hist64(name, unit string, buckets []float64) (metric.Float64Histogram, error) {
	opts := []metric.Float64HistogramOption{metric.WithUnit(unit)}
	if len(buckets) > 0 {
		opts = append(opts, metric.WithExplicitBucketBoundaries(buckets...))
	}
	return otel.Meter("mph").Float64Histogram(name, opts...)
}

func histInt(name, unit string, buckets []float64) (metric.Int64Histogram, error) {
	opts := []metric.Int64HistogramOption{metric.WithUnit(unit)}
	if len(buckets) > 0 {
		opts = append(opts, metric.WithExplicitBucketBoundaries(buckets...))
	}
	return otel.Meter("mph").Int64Histogram(name, opts...)
}

func initAgentMetrics() {
	var err error

	if contextTokensGauge, err = otel.Meter("mph").Int64Gauge("mph.context.tokens"); err != nil {
		contextTokensGauge = nil
	}
	if contextWindowGauge, err = otel.Meter("mph").Int64Gauge("mph.context.window"); err != nil {
		contextWindowGauge = nil
	}
	if runDurationGauge, err = otel.Meter("mph").Float64Gauge("mph.run.duration",
		metric.WithUnit("s")); err != nil {
		runDurationGauge = nil
	}
	if runTokensGauge, err = otel.Meter("mph").Int64Gauge("mph.run.tokens"); err != nil {
		runTokensGauge = nil
	}
	if runIterationsGauge, err = otel.Meter("mph").Int64Gauge("mph.run.iterations"); err != nil {
		runIterationsGauge = nil
	}
	if runToolCallsGauge, err = otel.Meter("mph").Int64Gauge("mph.run.tool_calls"); err != nil {
		runToolCallsGauge = nil
	}
	if runToolFailuresGauge, err = otel.Meter("mph").Int64Gauge("mph.run.tool_failures"); err != nil {
		runToolFailuresGauge = nil
	}
	if runCompactionsGauge, err = otel.Meter("mph").Int64Gauge("mph.run.compactions"); err != nil {
		runCompactionsGauge = nil
	}
	if runFinishedGauge, err = otel.Meter("mph").Int64Gauge("mph.run.finished"); err != nil {
		runFinishedGauge = nil
	}

	if iterationDurationHist, err = hist64("mph.agent.iteration.duration", "s", durationBuckets); err != nil {
		iterationDurationHist = nil
	}
	if chatDurationHist, err = hist64("mph.agent.chat.duration", "s", durationBuckets); err != nil {
		chatDurationHist = nil
	}
	if prefillTTFTHist, err = hist64("mph.model.prefill.ttft", "s", ttftBuckets); err != nil {
		prefillTTFTHist = nil
	}
	if tpsHist, err = hist64("mph.agent.tokens_per_second", "{token/s}", nil); err != nil {
		tpsHist = nil
	}
	if toolDurationHist, err = hist64("mph.agent.tool.duration", "s", toolDurationBuckets); err != nil {
		toolDurationHist = nil
	}
	if compactionDurationHist, err = hist64("mph.context.compaction.duration", "s", durationBuckets); err != nil {
		compactionDurationHist = nil
	}

	if prefillCachedHist, err = histInt("mph.model.prefill.cached_tokens", "{token}", nil); err != nil {
		prefillCachedHist = nil
	}
	if agentTokensHist, err = histInt("mph.agent.tokens", "{token}", nil); err != nil {
		agentTokensHist = nil
	}
	if reasoningTokensHist, err = histInt("mph.agent.reasoning.tokens", "{token}", nil); err != nil {
		reasoningTokensHist = nil
	}
	if tokensReclaimedHist, err = histInt("mph.context.tokens.reclaimed", "{token}", nil); err != nil {
		tokensReclaimedHist = nil
	}

	if toolCallsCounter, err = otel.Meter("mph").Int64Counter("mph.agent.tool.calls"); err != nil {
		toolCallsCounter = nil
	}
	if toolFailuresCounter, err = otel.Meter("mph").Int64Counter("mph.agent.tool.failures"); err != nil {
		toolFailuresCounter = nil
	}
	if compactionsCounter, err = otel.Meter("mph").Int64Counter("mph.context.compactions"); err != nil {
		compactionsCounter = nil
	}
}

// recordContextWindow exposes the effective context window for the run. Kronk
// auto-tunes it, so the configured value (0 = auto) is not the real limit.
func recordIterationDuration(ctx context.Context, d time.Duration) {
	if iterationDurationHist == nil {
		return
	}
	iterationDurationHist.Record(ctx, d.Seconds(), metric.WithAttributes(eventAttrs(ctx)...))
}

func recordContextWindow(ctx context.Context, krn Kronk) {
	initAgentMetrics()
	if contextWindowGauge == nil || krn == nil {
		return
	}
	window := krn.ModelConfig().ContextWindow()
	if window <= 0 {
		return
	}
	contextWindowGauge.Record(ctx, int64(window), metric.WithAttributes(runAttrs(ctx)...))
}

func recordTokenUsage(ctx context.Context, a *Agent, span trace.Span, log zerolog.Logger, usage *model.Usage) {
	if usage == nil {
		return
	}
	a.totals.PromptTokens += int64(usage.PromptTokens)
	a.totals.CompletionTokens += int64(usage.CompletionTokens)
	mphotel.LogEvent(ctx, log, zerolog.InfoLevel, "token usage", map[string]any{
		"prompt_tokens":     usage.PromptTokens,
		"completion_tokens": usage.CompletionTokens,
		"total_tokens":      usage.TotalTokens,
		"tps":               usage.TokensPerSecond,
	})
	span.SetAttributes(
		attribute.Int("mph.tokens.prompt", usage.PromptTokens),
		attribute.Int("mph.tokens.completion", usage.CompletionTokens),
		attribute.Int("mph.tokens.total", usage.TotalTokens),
		attribute.Float64("mph.tokens_per_second", usage.TokensPerSecond),
	)
	if agentTokensHist != nil {
		agentTokensHist.Record(ctx, int64(usage.PromptTokens), metric.WithAttributes(eventAttrs(ctx, attribute.String("tokens.type", "prompt"))...))
		agentTokensHist.Record(ctx, int64(usage.CompletionTokens), metric.WithAttributes(eventAttrs(ctx, attribute.String("tokens.type", "completion"))...))
	}
	if tpsHist != nil {
		tpsHist.Record(ctx, usage.TokensPerSecond, metric.WithAttributes(eventAttrs(ctx)...))
	}
	if prefillTTFTHist != nil && usage.TimeToFirstTokenMS > 0 {
		prefillTTFTHist.Record(ctx, usage.TimeToFirstTokenMS/1000, metric.WithAttributes(eventAttrs(ctx)...))
	}
	// Recorded unconditionally: a measured 0 (no prefix reuse this turn) is a
	// real signal, and suppressing it leaves the dashboard column blank, which
	// reads as a broken metric rather than "IMC reused nothing".
	if prefillCachedHist != nil {
		prefillCachedHist.Record(ctx, int64(usage.PromptTokensDetails.CachedTokens), metric.WithAttributes(eventAttrs(ctx)...))
	}
	if reasoningTokensHist != nil && usage.CompletionTokensDetails.ReasoningTokens > 0 {
		reasoningTokensHist.Record(ctx, int64(usage.CompletionTokensDetails.ReasoningTokens), metric.WithAttributes(eventAttrs(ctx)...))
	}
	// A level metric: one context line per iteration would read as a comb of
	// stale values rather than a single number that climbs toward the window.
	if contextTokensGauge != nil {
		contextTokensGauge.Record(ctx, int64(usage.TotalTokens), metric.WithAttributes(runAttrs(ctx)...))
	}
}

func addToolCallEvents(span trace.Span, toolCalls []model.ResponseToolCall, maxArgs int) {
	for _, tc := range toolCalls {
		argsJSON, _ := json.Marshal(tc.Function.Arguments)
		span.AddEvent("mph.agent.tool_call", trace.WithAttributes(
			attribute.String("mph.tool.name", tc.Function.Name),
			attribute.String("mph.tool.id", tc.ID),
			attribute.String("mph.tool.arguments", textutil.Truncate(string(argsJSON), maxArgs)),
		))
	}
}
