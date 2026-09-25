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
	tokensHist          metric.Int64Histogram
	tpsHist             metric.Float64Histogram
	iterationsCounter   metric.Int64Counter
	toolDurationHist    metric.Float64Histogram
	toolCallsCounter    metric.Int64Counter
	toolFailuresCounter metric.Int64Counter
	contextWindowGauge  metric.Int64UpDownCounter
	contextTokensGauge  metric.Int64UpDownCounter
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

// runAttrs tags every measurement with the run UUID, or with nothing when the
// context has no run, so out-of-run recordings cannot create an empty series.
func runAttrs(ctx context.Context) metric.MeasurementOption {
	runID := runIDFromContext(ctx)
	if runID == "" {
		return metric.WithAttributes()
	}
	return metric.WithAttributes(attribute.String("mph.run.id", runID))
}

// runSpanAttrs is runAttrs for spans, which take attributes rather than
// measurement options.
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
	contextWindowGauge, err = meter.Int64UpDownCounter("mph.context.window")
	if err != nil {
		contextWindowGauge = nil
	}
	contextTokensGauge, err = meter.Int64UpDownCounter("mph.context.tokens")
	if err != nil {
		contextTokensGauge = nil
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
	contextWindowGauge.Add(ctx, int64(window), runAttrs(ctx))
}

func recordTokenUsage(ctx context.Context, span trace.Span, log zerolog.Logger, prevContextTokens int64, usage *model.Usage) int64 {
	if usage == nil {
		return prevContextTokens
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
		tokensHist.Record(ctx, int64(usage.PromptTokens), runAttrs(ctx), metric.WithAttributes(attribute.String("tokens.type", "prompt")))
		tokensHist.Record(ctx, int64(usage.CompletionTokens), runAttrs(ctx), metric.WithAttributes(attribute.String("tokens.type", "completion")))
		tokensHist.Record(ctx, int64(usage.TotalTokens), runAttrs(ctx), metric.WithAttributes(attribute.String("tokens.type", "total")))
	}
	if tpsHist != nil {
		tpsHist.Record(ctx, usage.TokensPerSecond, runAttrs(ctx))
	}
	// Context in play for this request. The conversation grows every iteration, so
	// the final value is how close the run got to the context window limit. Only
	// the delta is added, which keeps the gauge at "current", not "cumulative".
	current := int64(usage.TotalTokens)
	if contextTokensGauge != nil {
		contextTokensGauge.Add(ctx, current-prevContextTokens, runAttrs(ctx))
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
