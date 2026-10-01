package otel

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	kotel "github.com/ardanlabs/kronk/sdk/kronk/observ/otel"
)

// wrappedTracer stamps attrs onto every span it starts. kronk creates its own
// spans (process-request, prefill, token-generation) through a tracer it reads
// from the context, and span attributes are not inherited, so without this the
// kronk spans carry no run or iteration identity and cannot be found by
// TraceQL alongside ours.
type wrappedTracer struct {
	trace.Tracer
	attrs []attribute.KeyValue
}

// Wrap returns a tracer that adds attrs to every span it starts.
func Wrap(base trace.Tracer, attrs ...attribute.KeyValue) trace.Tracer {
	if len(attrs) == 0 {
		return base
	}
	return &wrappedTracer{Tracer: base, attrs: attrs}
}

func (w *wrappedTracer) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	// Copy before appending: opts often shares its backing array with the
	// caller's slice, and appending in place would overwrite it.
	merged := make([]trace.SpanStartOption, 0, len(opts)+1)
	merged = append(merged, opts...)
	merged = append(merged, trace.WithAttributes(w.attrs...))
	return w.Tracer.Start(ctx, name, merged...)
}

// InjectTracing puts a run-labelled tracer into ctx so kronk's own spans
// (process-request, prefill, token-generation) carry the same identity as ours.
// kronk's SDK reads a tracer from the context under a private key and creates
// no spans without one; only kronk's server injects it, so an SDK consumer has
// to do it itself.
func InjectTracing(ctx context.Context, tracer trace.Tracer, attrs ...attribute.KeyValue) context.Context {
	return kotel.InjectTracing(ctx, Wrap(tracer, attrs...))
}
