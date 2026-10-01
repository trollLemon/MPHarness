package otel

import (
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestWrapAppliesAttributesToEverySpan(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	ctx := t.Context()

	tracer := Wrap(tp.Tracer("test"),
		attribute.String("mph.run.id", "baseline:01J9"),
		attribute.Int("mph.iteration", 4),
	)

	_, parent := tracer.Start(ctx, "iteration")
	_, child := tracer.Start(ctx, "prefill")
	child.End()
	parent.End()

	spans := rec.Ended()
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2", len(spans))
	}
	for _, s := range spans {
		found := map[string]string{}
		for _, kv := range s.Attributes() {
			found[string(kv.Key)] = kv.Value.String()
		}
		if found["mph.run.id"] != "baseline:01J9" || found["mph.iteration"] != "4" {
			t.Errorf("span %q attributes = %v, want run id and iteration", s.Name(), found)
		}
	}
}

func TestWrapDoesNotMutateCallerOptions(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	tracer := Wrap(tp.Tracer("test"), attribute.Int("mph.iteration", 1))

	// A shared backing array is how a variadic caller's slice gets clobbered:
	// appending to opts must not write into memory the caller still owns.
	shared := make([]trace.SpanStartOption, 1, 4)
	shared[0] = trace.WithAttributes(attribute.String("caller", "value"))

	_, s1 := tracer.Start(t.Context(), "first", shared...)
	_, s2 := tracer.Start(t.Context(), "second", shared...)
	s1.End()
	s2.End()

	for _, s := range rec.Ended() {
		var hasCaller bool
		for _, kv := range s.Attributes() {
			if string(kv.Key) == "caller" {
				hasCaller = true
			}
		}
		if !hasCaller {
			t.Errorf("span %q lost the caller's own attribute", s.Name())
		}
	}
}

func TestWrapWithoutAttributes(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))

	tracer := Wrap(tp.Tracer("test"))
	_, s := tracer.Start(t.Context(), "plain")
	s.End()

	if got := len(rec.Ended()[0].Attributes()); got != 0 {
		t.Errorf("plain span has %d attributes, want 0", got)
	}
}
