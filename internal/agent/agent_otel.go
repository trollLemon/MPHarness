package agent

import (
	"context"
	"encoding/json"

	"github.com/ardanlabs/kronk/sdk/kronk/model"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	mphotel "github.com/trollLemon/MPHarness/internal/otel"
)

var (
	tokensHist             metric.Int64Histogram
	tpsHist                metric.Float64Histogram
	iterationsCounter      metric.Int64Counter
	toolDurationHist       metric.Float64Histogram
	toolCallsCounter       metric.Int64Counter
	toolFailuresCounter    metric.Int64Counter
	contextWindowGauge     metric.Int64Gauge
	contextTokensGauge     metric.Int64Gauge
	compactionsCounter     metric.Int64Counter
	compactionDurationHist metric.Float64Histogram
	tokensReclaimedHist    metric.Int64Histogram
)

// runIDKey scopes the run UUID to the context so every helper reached during a
// run can attribute its measurements without the ID being threaded through
// signatures.
type runIDKey struct{}

func withRunID(ctx context.Context, runID string) context.Context {
	return context.WithValue(ctx, runIDKey{}, runID)
}

func runIDFromContext(ctx context.Context) string {
	runID, _ := ctx.Value(runIDKey{}).(string)
	return runID
}

// runSpanAttrs tags spans with the run UUID, which is the correct use of a
// high-cardinality correlation id. Metric measurements must not carry it.
func runSpanAttrs(ctx context.Context, extra ...attribute.KeyValue) []attribute.KeyValue {
	attrs := extra
	if runID := runIDFromContext(ctx); runID != "" {
		attrs = append(attrs, attribute.String("mph.run.id", runID))
	}
	return attrs
}

func getAgentTracer() trace.Tracer {
	return otel.Tracer("mph")
}

func getAgentMeter() metric.Meter {
	return otel.Meter("mph")
}

func initAgentMetrics() {
	meter := getAgentMeter()
	var err error
	tokensHist, err = meter.Int64Histogram("mph.tokens")
	if err != nil {
		tokensHist = nil
	}
	tpsHist, err = meter.Float64Histogram("mph.tokens_per_second")
	if err != nil {
		tpsHist = nil
	}
	iterationsCounter, err = meter.Int64Counter("mph.iterations")
	if err != nil {
		iterationsCounter = nil
	}
	toolDurationHist, err = meter.Float64Histogram("mph.agent.tool.duration")
	if err != nil {
		toolDurationHist = nil
	}
	toolCallsCounter, err = meter.Int64Counter("mph.agent.tool.calls")
	if err != nil {
		toolCallsCounter = nil
	}
	toolFailuresCounter, err = meter.Int64Counter("mph.agent.tool.failures")
	if err != nil {
		toolFailuresCounter = nil
	}
	contextWindowGauge, err = meter.Int64Gauge("mph.context.window")
	if err != nil {
		contextWindowGauge = nil
	}
	contextTokensGauge, err = meter.Int64Gauge("mph.context.tokens")
	if err != nil {
		contextTokensGauge = nil
	}
	compactionsCounter, err = meter.Int64Counter("mph.context.compactions")
	if err != nil {
		compactionsCounter = nil
	}
	compactionDurationHist, err = meter.Float64Histogram("mph.context.compaction.duration")
	if err != nil {
		compactionDurationHist = nil
	}
	tokensReclaimedHist, err = meter.Int64Histogram("mph.context.tokens.reclaimed")
	if err != nil {
		tokensReclaimedHist = nil
	}
}

// recordContextWindow exposes the effective context window for the run. Kronk
// auto-tunes it, so the configured value (0 = auto) is not the real limit.
func recordContextWindow(ctx context.Context, krn Kronk) {
	initAgentMetrics()
	if contextWindowGauge == nil || krn == nil {
		return
	}
	window := krn.ModelConfig().ContextWindow()
	if window <= 0 {
		return
	}
	contextWindowGauge.Record(ctx, int64(window))
}

func recordTokenUsage(ctx context.Context, span trace.Span, log zerolog.Logger, usage *model.Usage) int64 {
	if usage == nil {
		return 0
	}
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
	if tokensHist != nil {
		tokensHist.Record(ctx, int64(usage.PromptTokens), metric.WithAttributes(attribute.String("tokens.type", "prompt")))
		tokensHist.Record(ctx, int64(usage.CompletionTokens), metric.WithAttributes(attribute.String("tokens.type", "completion")))
		tokensHist.Record(ctx, int64(usage.TotalTokens), metric.WithAttributes(attribute.String("tokens.type", "total")))
	}
	if tpsHist != nil {
		tpsHist.Record(ctx, usage.TokensPerSecond)
	}
	current := int64(usage.TotalTokens)
	if contextTokensGauge != nil {
		contextTokensGauge.Record(ctx, current)
	}
	return current
}

func addToolCallEvents(span trace.Span, toolCalls []model.ResponseToolCall, maxArgs int) {
	for _, tc := range toolCalls {
		argsJSON, _ := json.Marshal(tc.Function.Arguments)
		span.AddEvent("mph.agent.tool_call", trace.WithAttributes(
			attribute.String("mph.tool.name", tc.Function.Name),
			attribute.String("mph.tool.id", tc.ID),
			attribute.String("mph.tool.arguments", truncate(string(argsJSON), maxArgs)),
		))
	}
}
