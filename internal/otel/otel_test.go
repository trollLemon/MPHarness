package otel

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log/global"
	otellog "go.opentelemetry.io/otel/log"
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
		Endpoint:    "localhost:4317",
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
	// providers should be SDK types
	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); !ok {
		t.Fatalf("unexpected tracer provider type %T", otel.GetTracerProvider())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		// Without a collector, export fails with Unavailable/connection refused – acceptable in unit test.
		if !isCollectorUnavailable(err) {
			t.Fatalf("shutdown: %v", err)
		}
		t.Logf("shutdown (expected without collector): %v", err)
	}
}

func isCollectorUnavailable(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return contains(s, "connection refused") || contains(s, "Unavailable") || contains(s, "connection error")
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i <= len(s)-len(sub); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func TestHookRun(t *testing.T) {
	origLP := global.GetLoggerProvider()
	defer global.SetLoggerProvider(origLP)

	// Setup a provider so hook has a real logger provider (but no collector)
	cfg := Config{Enabled: true, Endpoint: "localhost:4317", ServiceName: "mph-hook-test"}
	shutdown, err := Setup(cfg)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = shutdown(ctx)
	}()

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

func TestSetupDefaults(t *testing.T) {
	origTP := otel.GetTracerProvider()
	defer otel.SetTracerProvider(origTP)
	cfg := Config{Enabled: true}
	shutdown, err := Setup(cfg)
	if err != nil {
		t.Fatalf("Setup defaults: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		if !isCollectorUnavailable(err) {
			t.Fatalf("shutdown: %v", err)
		}
		t.Logf("shutdown (expected without collector): %v", err)
	}
}
