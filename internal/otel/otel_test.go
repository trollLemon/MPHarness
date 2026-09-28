package otel

import (
	"context"
	"os"
	"testing"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestSetupDisabled(t *testing.T) {
	origTP := otel.GetTracerProvider()
	origMP := otel.GetMeterProvider()
	origLP := global.GetLoggerProvider()
	defer func() {
		otel.SetTracerProvider(origTP)
		otel.SetMeterProvider(origMP)
		global.SetLoggerProvider(origLP)
	}()

	shutdown, err := Setup(Config{Enabled: false})
	if err != nil {
		t.Fatalf("Setup disabled: %v", err)
	}
	if shutdown == nil {
		t.Fatalf("shutdown nil")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if got := otel.GetTracerProvider(); got != origTP {
		t.Fatalf("tracer provider changed when disabled")
	}
}

func TestSetupOTEL_SDK_DISABLED(t *testing.T) {
	origTP := otel.GetTracerProvider()
	defer otel.SetTracerProvider(origTP)
	t.Setenv("OTEL_SDK_DISABLED", "true")

	shutdown, err := Setup(Config{Enabled: true, Endpoint: "localhost:4317", ServiceName: "mph-test"})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	_ = os.Unsetenv("OTEL_SDK_DISABLED")
}

// TestSetupEnabled asserts what Setup does locally when enabled: it installs SDK
// providers and replaces the global ones.
//
// The returned shutdown is deliberately never called. The metric reader is
// periodic, and Shutdown forces a final collect-and-export whether or not
// anything was recorded, so calling it opens a socket to the configured
// endpoint. Pointing that at the default endpoint would make the test depend on
// whatever OTLP collector the machine happens to be running, and pointing it at
// a dead port would still spend the full 5s exportTimeout proving the port is
// dead. Neither belongs in a unit test.
//
// Setup itself opens no socket: the OTLP exporters dial lazily, so constructing
// them does no I/O and everything asserted here is reached offline. The globals
// are restored on exit, so the providers left installed are inert.
func TestSetupEnabled(t *testing.T) {
	origTP := otel.GetTracerProvider()
	origMP := otel.GetMeterProvider()
	origLP := global.GetLoggerProvider()
	defer func() {
		otel.SetTracerProvider(origTP)
		otel.SetMeterProvider(origMP)
		global.SetLoggerProvider(origLP)
	}()

	cfg := Config{
		Enabled:     true,
		ServiceName: "mph-test",
		ResourceAttributes: map[string]string{
			"environment": "test",
		},
	}
	shutdown, err := Setup(cfg)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if shutdown == nil {
		t.Fatalf("shutdown nil")
	}
	if got := otel.GetTracerProvider(); got == origTP {
		t.Fatalf("tracer provider not set")
	}
	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); !ok {
		t.Fatalf("unexpected tracer provider type %T", otel.GetTracerProvider())
	}
	if _, ok := otel.GetMeterProvider().(*sdkmetric.MeterProvider); !ok {
		t.Fatalf("unexpected meter provider type %T", otel.GetMeterProvider())
	}
	if _, ok := global.GetLoggerProvider().(*sdklog.LoggerProvider); !ok {
		t.Fatalf("unexpected logger provider type %T", global.GetLoggerProvider())
	}
}

func TestHookRun(t *testing.T) {
	origLP := global.GetLoggerProvider()
	defer global.SetLoggerProvider(origLP)

	// The hook only needs a real LoggerProvider, which Setup installs offline, so
	// the returned shutdown is never called and no socket is opened. See
	// TestSetupEnabled for why shutdown is off limits in a unit test.
	cfg := Config{Enabled: true, ServiceName: "mph-hook-test"}
	shutdown, err := Setup(cfg)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if shutdown == nil {
		t.Fatalf("shutdown nil")
	}

	h := NewHook("mph")
	if h == nil {
		t.Fatalf("NewHook nil")
	}

	// Run should not panic for nil event
	h.Run(nil, zerolog.InfoLevel, "nil event")

	// Normal event with context
	logger := zerolog.Nop()
	evt := logger.Info().Ctx(context.Background())
	// Hook is called inside msg(); we can directly call Run with a fake event
	// that has ctx. Use zerolog's event creation via logger.Info()
	// but we need an *Event to pass. Create via scratch.
	e := logger.Info()
	if e != nil {
		// Set ctx to background with span
		ctx, span := otel.Tracer("mph").Start(context.Background(), "test-hook")
		e.Ctx(ctx)
		h.Run(e, zerolog.InfoLevel, "hello hook")
		span.End()
		_ = ctx
	}
	_ = evt
}

func TestConvertLevel(t *testing.T) {
	tests := []struct {
		level zerolog.Level
		want  otellog.Severity
	}{
		{zerolog.TraceLevel, otellog.SeverityTrace},
		{zerolog.DebugLevel, otellog.SeverityDebug},
		{zerolog.InfoLevel, otellog.SeverityInfo},
		{zerolog.WarnLevel, otellog.SeverityWarn},
		{zerolog.ErrorLevel, otellog.SeverityError},
		{zerolog.FatalLevel, otellog.SeverityFatal},
		{zerolog.PanicLevel, otellog.SeverityFatal2},
		{zerolog.NoLevel, otellog.SeverityUndefined},
	}
	for _, tt := range tests {
		if got := convertLevel(tt.level); got != tt.want {
			t.Fatalf("convertLevel(%v) = %v want %v", tt.level, got, tt.want)
		}
	}
}

// TestSetupDefaults covers the zero-config path, so the Config is left entirely
// blank and Setup has to resolve DefaultEndpoint and DefaultServiceName itself.
// shutdown is not called, so resolving the default endpoint costs no I/O; see
// TestSetupEnabled.
func TestSetupDefaults(t *testing.T) {
	origTP := otel.GetTracerProvider()
	defer otel.SetTracerProvider(origTP)
	shutdown, err := Setup(Config{Enabled: true})
	if err != nil {
		t.Fatalf("Setup defaults: %v", err)
	}
	if shutdown == nil {
		t.Fatalf("shutdown nil")
	}
	if got := otel.GetTracerProvider(); got == origTP {
		t.Fatalf("tracer provider not set for a zero-value Config")
	}
}

func TestTraceAttrs(t *testing.T) {
	origTP := otel.GetTracerProvider()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(origTP)
		_ = tp.Shutdown(context.Background())
	})

	t.Run("no span in context yields no attributes", func(t *testing.T) {
		if got := traceAttrs(context.Background()); got != nil {
			t.Fatalf("traceAttrs(background) = %v, want nil", got)
		}
	})

	t.Run("active span yields trace and span id attributes", func(t *testing.T) {
		ctx, span := otel.Tracer("mph").Start(context.Background(), "test-span")
		defer span.End()

		got := traceAttrs(ctx)
		if len(got) != 2 {
			t.Fatalf("traceAttrs returned %d attributes, want 2: %v", len(got), got)
		}

		want := map[string]string{
			"trace_id": span.SpanContext().TraceID().String(),
			"span_id":  span.SpanContext().SpanID().String(),
		}
		for _, kv := range got {
			key := string(kv.Key)
			if want[key] == "" {
				t.Errorf("unexpected attribute %q", key)
				continue
			}
			if got := kv.Value.AsString(); got != want[key] {
				t.Errorf("%s = %v, want %v", key, got, want[key])
			}
			delete(want, key)
		}
		for key := range want {
			t.Errorf("missing attribute %q", key)
		}
	})

	t.Run("ended span still yields attributes", func(t *testing.T) {
		ctx, span := otel.Tracer("mph").Start(context.Background(), "ended-span")
		span.End()
		if got := traceAttrs(ctx); len(got) != 2 {
			t.Fatalf("traceAttrs(ended span) = %v, want 2 attributes", got)
		}
	})
}
