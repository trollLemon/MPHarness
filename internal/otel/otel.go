package otel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/rs/zerolog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type ctxKey string

const skipHookKey ctxKey = "otel-skip-hook"

// Defaults for a Config left partially blank. Exported so cmd/mph can present the
// same values in help text and layer environment variables over the YAML without
// the two drifting apart.
const (
	DefaultEndpoint    = "localhost:4317"
	DefaultServiceName = "mph"
)

// Exporter and batcher tunables.
const (
	exporterTimeout = 10 * time.Second
	batchTimeout    = 2 * time.Second
	exportTimeout   = 5 * time.Second
	metricInterval  = 5 * time.Second
	logInterval     = 1 * time.Second
)

type Config struct {
	Enabled            bool
	Endpoint           string
	ServiceName        string
	ResourceAttributes map[string]string
}

func Setup(cfg Config) (func(context.Context) error, error) {
	if !cfg.Enabled {
		return func(context.Context) error { return nil }, nil
	}
	if os.Getenv("OTEL_SDK_DISABLED") == "true" {
		return func(context.Context) error { return nil }, nil
	}
	ctx := context.Background()

	if cfg.Endpoint == "" {
		cfg.Endpoint = DefaultEndpoint
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = DefaultServiceName
	}

	attrs := []attribute.KeyValue{
		attribute.String("service.name", cfg.ServiceName),
	}
	for k, v := range cfg.ResourceAttributes {
		attrs = append(attrs, attribute.String(k, v))
	}

	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
		resource.WithAttributes(attrs...),
	)
	if err != nil {
		return nil, err
	}

	traceExp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
		otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithTimeout(exporterTimeout),
	)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp, sdktrace.WithBatchTimeout(batchTimeout), sdktrace.WithExportTimeout(exportTimeout)),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	metricExp, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpoint(cfg.Endpoint),
		otlpmetricgrpc.WithInsecure(),
		otlpmetricgrpc.WithTimeout(exporterTimeout),
	)
	if err != nil {
		_ = tp.Shutdown(ctx)
		return nil, err
	}

	reader := sdkmetric.NewPeriodicReader(metricExp,
		sdkmetric.WithInterval(metricInterval),
		sdkmetric.WithTimeout(exportTimeout),
	)
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	logExp, err := otlploggrpc.New(ctx,
		otlploggrpc.WithEndpoint(cfg.Endpoint),
		otlploggrpc.WithInsecure(),
		otlploggrpc.WithTimeout(exporterTimeout),
	)
	if err != nil {
		_ = tp.Shutdown(ctx)
		_ = mp.Shutdown(ctx)
		return nil, err
	}

	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp,
			sdklog.WithExportInterval(logInterval),
			sdklog.WithExportTimeout(exportTimeout),
		)),
		sdklog.WithResource(res),
	)
	global.SetLoggerProvider(lp)

	shutdown := func(ctx context.Context) error {
		var errs []error
		if err := tp.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
		if err := mp.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
		if err := lp.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
		return errors.Join(errs...)
	}

	return shutdown, nil
}

type Hook struct {
	logger otellog.Logger
}

func NewHook(name string) *Hook {
	return &Hook{
		logger: global.Logger(name),
	}
}

func (h Hook) Run(e *zerolog.Event, level zerolog.Level, msg string) {
	if e == nil {
		return
	}
	ctx := e.GetCtx()
	if ctx.Value(skipHookKey) != nil {
		return
	}
	rec := otellog.Record{}
	rec.SetTimestamp(time.Now())
	rec.SetObservedTimestamp(time.Now())
	rec.SetSeverity(convertLevel(level))
	rec.SetSeverityText(level.String())
	rec.SetBody(otellog.StringValue(msg))
	for _, kv := range traceAttrs(ctx) {
		rec.AddAttributes(kv)
	}
	h.logger.Emit(ctx, rec)
}

// traceAttrs exposes the active span as record attributes. The log SDK's
// Record has no dedicated trace/span fields, so correlation is carried as
// attributes instead; without them log lines cannot be linked to a trace.
func traceAttrs(ctx context.Context) []otellog.KeyValue {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	return []otellog.KeyValue{
		otellog.String("trace_id", sc.TraceID().String()),
		otellog.String("span_id", sc.SpanID().String()),
	}
}

func LogEvent(ctx context.Context, logger zerolog.Logger, level zerolog.Level, msg string, fields map[string]any) {
	skipCtx := context.WithValue(ctx, skipHookKey, true)
	evt := logger.WithLevel(level).Ctx(skipCtx)
	if evt == nil {
		// level disabled – still emit OTEL directly
		evt = nil
	} else {
		evt.CallerSkipFrame(1)
		for k, v := range fields {
			switch val := v.(type) {
			case string:
				evt = evt.Str(k, val)
			case int:
				evt = evt.Int(k, val)
			case int64:
				evt = evt.Int64(k, val)
			case bool:
				evt = evt.Bool(k, val)
			case float64:
				evt = evt.Float64(k, val)
			case json.RawMessage:
				evt = evt.RawJSON(k, val)
			case []byte:
				evt = evt.Bytes(k, val)
			case error:
				evt = evt.AnErr(k, val)
			default:
				evt = evt.Interface(k, v)
			}
		}
		evt.Msg(msg)
	}
	rec := otellog.Record{}
	rec.SetTimestamp(time.Now())
	rec.SetObservedTimestamp(time.Now())
	rec.SetSeverity(convertLevel(level))
	rec.SetSeverityText(level.String())
	rec.SetBody(otellog.StringValue(msg))
	for k, v := range fields {
		switch val := v.(type) {
		case string:
			rec.AddAttributes(otellog.String(k, val))
		case int:
			rec.AddAttributes(otellog.Int(k, val))
		case int64:
			rec.AddAttributes(otellog.Int64(k, val))
		case bool:
			rec.AddAttributes(otellog.Bool(k, val))
		case float64:
			rec.AddAttributes(otellog.Float64(k, val))
		case json.RawMessage:
			rec.AddAttributes(otellog.String(k, string(val)))
		case []byte:
			rec.AddAttributes(otellog.String(k, string(val)))
		case error:
			rec.AddAttributes(otellog.String(k, val.Error()))
		default:
			rec.AddAttributes(otellog.String(k, fmt.Sprint(v)))
		}
	}
	for _, kv := range traceAttrs(ctx) {
		rec.AddAttributes(kv)
	}
	global.Logger("mph").Emit(ctx, rec)
}

func convertLevel(level zerolog.Level) otellog.Severity {
	switch level {
	case zerolog.TraceLevel:
		return otellog.SeverityTrace
	case zerolog.DebugLevel:
		return otellog.SeverityDebug
	case zerolog.InfoLevel:
		return otellog.SeverityInfo
	case zerolog.WarnLevel:
		return otellog.SeverityWarn
	case zerolog.ErrorLevel:
		return otellog.SeverityError
	case zerolog.FatalLevel:
		return otellog.SeverityFatal
	case zerolog.PanicLevel:
		return otellog.SeverityFatal2
	default:
		return otellog.SeverityUndefined
	}
}
