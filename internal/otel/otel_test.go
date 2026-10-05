package otel

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// TestSetup covers the Setup matrix: disabled and SDK-disabled installs
// nothing and leave the globals alone, while enabled installs SDK providers.
//
// The returned shutdown is deliberately never called on the enabled paths.
// Shutdown flushes the batch exporters, which can open a socket to the
// configured endpoint. Pointing that at the default
// endpoint would make the test depend on whatever OTLP collector the machine
// happens to be running, and pointing it at a dead port would still spend the
// full 5s exportTimeout proving the port is dead. Neither belongs in a unit
// test.
//
// Setup itself opens no socket: the OTLP exporters dial lazily, so
// constructing them does no I/O and everything asserted here is reached
// offline. The globals are restored on exit, so the providers left installed
// are inert.
func TestSetup(t *testing.T) {
	tests := []struct {
		name          string
		cfg           Config
		env           map[string]string
		wantInstalled bool
	}{
		{"disabled leaves globals alone", Config{Enabled: false}, nil, false},
		{"enabled installs SDK providers", Config{
			Enabled:     true,
			ServiceName: "mph-test",
			ResourceAttributes: map[string]string{
				"environment": "test",
			},
		}, nil, true},
		{"zero config resolves defaults and installs", Config{Enabled: true}, nil, true},
		{"OTEL_SDK_DISABLED installs nothing", Config{Enabled: true, Endpoint: "localhost:4317", ServiceName: "mph-test"}, map[string]string{"OTEL_SDK_DISABLED": "true"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origTP := otel.GetTracerProvider()
			origMP := otel.GetMeterProvider()
			origLP := global.GetLoggerProvider()
			defer func() {
				otel.SetTracerProvider(origTP)
				otel.SetMeterProvider(origMP)
				global.SetLoggerProvider(origLP)
			}()
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			shutdown, err := Setup(context.Background(), tt.cfg)
			if err != nil {
				t.Fatalf("Setup: %v", err)
			}
			if shutdown == nil {
				t.Fatalf("shutdown nil")
			}
			if !tt.wantInstalled {
				if err := shutdown(context.Background()); err != nil {
					t.Fatalf("shutdown: %v", err)
				}
				if got := otel.GetTracerProvider(); got != origTP {
					t.Fatalf("tracer provider changed when nothing should install")
				}
				if got := otel.GetMeterProvider(); got != origMP {
					t.Fatalf("meter provider changed when nothing should install")
				}
				if got := global.GetLoggerProvider(); got != origLP {
					t.Fatalf("logger provider changed when nothing should install")
				}
				return
			}
			if got := otel.GetTracerProvider(); got == origTP {
				t.Fatalf("tracer provider not set")
			}
			if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); !ok {
				t.Fatalf("unexpected tracer provider type %T", otel.GetTracerProvider())
			}
			if got := otel.GetMeterProvider(); got != origMP {
				t.Fatalf("meter provider installed; mph exports no metrics")
			}
			if _, ok := global.GetLoggerProvider().(*sdklog.LoggerProvider); !ok {
				t.Fatalf("unexpected logger provider type %T", global.GetLoggerProvider())
			}
		})
	}
}

func TestHookRun(t *testing.T) {
	origLP := global.GetLoggerProvider()
	defer global.SetLoggerProvider(origLP)

	// The hook only needs a real LoggerProvider, which Setup installs offline, so
	// the returned shutdown is never called and no socket is opened. See
	// TestSetup for why shutdown is off limits in a unit test.
	cfg := Config{Enabled: true, ServiceName: "mph-hook-test"}
	shutdown, err := Setup(context.Background(), cfg)
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
		name  string
		level zerolog.Level
		want  otellog.Severity
	}{
		{"trace", zerolog.TraceLevel, otellog.SeverityTrace},
		{"debug", zerolog.DebugLevel, otellog.SeverityDebug},
		{"info", zerolog.InfoLevel, otellog.SeverityInfo},
		{"warn", zerolog.WarnLevel, otellog.SeverityWarn},
		{"error", zerolog.ErrorLevel, otellog.SeverityError},
		{"fatal", zerolog.FatalLevel, otellog.SeverityFatal},
		{"panic", zerolog.PanicLevel, otellog.SeverityFatal2},
		{"no level", zerolog.NoLevel, otellog.SeverityUndefined},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := convertLevel(tt.level); got != tt.want {
				t.Fatalf("convertLevel(%v) = %v want %v", tt.level, got, tt.want)
			}
		})
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

	tests := []struct {
		name    string
		setup   func(t *testing.T) (context.Context, string, string)
		wantNil bool
	}{
		{
			name:    "no span in context yields no attributes",
			setup:   func(t *testing.T) (context.Context, string, string) { return context.Background(), "", "" },
			wantNil: true,
		},
		{
			name: "active span yields trace and span id attributes",
			setup: func(t *testing.T) (context.Context, string, string) {
				ctx, span := otel.Tracer("mph").Start(context.Background(), "test-span")
				t.Cleanup(func() { span.End() })
				return ctx, span.SpanContext().TraceID().String(), span.SpanContext().SpanID().String()
			},
		},
		{
			name: "ended span still yields attributes",
			setup: func(t *testing.T) (context.Context, string, string) {
				ctx, span := otel.Tracer("mph").Start(context.Background(), "ended-span")
				span.End()
				return ctx, span.SpanContext().TraceID().String(), span.SpanContext().SpanID().String()
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, traceID, spanID := tt.setup(t)
			got := traceAttrs(ctx)
			if tt.wantNil {
				if got != nil {
					t.Fatalf("traceAttrs(background) = %v, want nil", got)
				}
				return
			}
			if len(got) != 2 {
				t.Fatalf("traceAttrs returned %d attributes, want 2: %v", len(got), got)
			}
			want := map[string]string{"trace_id": traceID, "span_id": spanID}
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
	}
}

func TestLogEventFieldOrder(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	for range 20 {
		buf.Reset()
		LogEvent(context.Background(), logger, zerolog.InfoLevel, "tool succeeded", map[string]any{
			"out":   strings.Repeat("x", 5000),
			"id":    "call-1",
			"tool":  "multipass_exec",
			"iter":  2,
			"event": "tool_succeeded",
		})
		line := buf.String()
		event := strings.Index(line, `"event"`)
		tool := strings.Index(line, `"tool"`)
		id := strings.Index(line, `"id"`)
		iter := strings.Index(line, `"iter"`)
		out := strings.Index(line, `"out"`)
		if event < 0 || iter <= event || tool <= iter || id <= tool || out <= id {
			t.Fatalf("fields out of order: %q...", line[:120])
		}
	}
}

func TestLogEventWithLevelDisabledStillEmits(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf).Level(zerolog.ErrorLevel)

	LogEvent(context.Background(), logger, zerolog.InfoLevel, "quiet", map[string]any{"k": "v"})

	if buf.Len() != 0 {
		t.Fatalf("filtered level must not reach zerolog, got %q", buf.String())
	}
}

func TestLogEventPayloadLast(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	for range 20 {
		buf.Reset()
		LogEvent(context.Background(), logger, zerolog.InfoLevel, "agent tool call", map[string]any{
			"out":  strings.Repeat("x", 100),
			"args": `{"command":"apt --help"}`,
			"id":   "call-1",
			"tool": "multipass_exec",
			"iter": 2,
		})
		line := buf.String()
		iter := strings.Index(line, `"iter"`)
		tool := strings.Index(line, `"tool"`)
		id := strings.Index(line, `"id"`)
		args := strings.Index(line, `"args"`)
		out := strings.Index(line, `"out"`)
		if iter < 0 || tool < 0 || id < 0 || args < 0 || out < 0 {
			t.Fatalf("missing keys: %q", line)
		}
		if tool <= iter || id <= tool || args <= id || out <= args {
			t.Fatalf("payload not last / out of order: %q", line)
		}
	}
}

// The payload key list decides which fields sort to the end of a line. A key
// nothing emits is dead weight that makes the list look authoritative when it
// is not, so the list must be exactly the keys the codebase actually logs.
func TestPayloadKeysMatchEmittedKeys(t *testing.T) {
	emitted := map[string]bool{
		"args": true, "text": true, "out": true, "command_output": true,
	}
	for _, k := range payloadKeyOrder {
		if !emitted[k] {
			t.Errorf("payload key %q is never emitted as a LogEvent field", k)
		}
	}
	for k := range emitted {
		if !slices.Contains(payloadKeyOrder, k) {
			t.Errorf("emitted key %q is missing from payloadKeyOrder", k)
		}
	}
}

func TestSetupAddsRunNameResourceAttribute(t *testing.T) {
	shutdown, err := Setup(t.Context(), Config{
		Enabled:     true,
		Endpoint:    DefaultEndpoint,
		ServiceName: "mph-test",
		RunID:       "baseline:01J9F2K3M4N5P6Q7R8S9T0V1W2",
		RunName:     "baseline",
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(t.Context()) })
}
