package otel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
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
	RunID              string
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
	if strings.TrimSpace(cfg.RunID) != "" {
		attrs = append(attrs, attribute.String("service.instance.id", cfg.RunID))
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
	rec.SetBody(attribute.StringValue(msg))
	for _, kv := range traceAttrs(ctx) {
		rec.AddAttributes(kv)
	}
	h.logger.Emit(ctx, rec)
}

// traceAttrs exposes the active span as record attributes. The log SDK's
// Record has no dedicated trace/span fields, so correlation is carried as
// attributes instead; without them log lines cannot be linked to a trace.
func traceAttrs(ctx context.Context) []attribute.KeyValue {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	return []attribute.KeyValue{
		attribute.String("trace_id", sc.TraceID().String()),
		attribute.String("span_id", sc.SpanID().String()),
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
		for _, k := range orderedFieldKeys(fields) {
			evt = appendField(evt, k, fields[k])
		}
		evt.Msg(msg)
	}
	rec := otellog.Record{}
	rec.SetTimestamp(time.Now())
	rec.SetObservedTimestamp(time.Now())
	rec.SetSeverity(convertLevel(level))
	rec.SetSeverityText(level.String())
	rec.SetBody(attribute.StringValue(msg))
	for k, v := range fields {
		switch val := v.(type) {
		case string:
			rec.AddAttributes(attribute.String(k, val))
		case int:
			rec.AddAttributes(attribute.Int(k, val))
		case int64:
			rec.AddAttributes(attribute.Int64(k, val))
		case bool:
			rec.AddAttributes(attribute.Bool(k, val))
		case float64:
			rec.AddAttributes(attribute.Float64(k, val))
		case json.RawMessage:
			rec.AddAttributes(attribute.String(k, string(val)))
		case []byte:
			rec.AddAttributes(attribute.String(k, string(val)))
		case error:
			rec.AddAttributes(attribute.String(k, val.Error()))
		default:
			rec.AddAttributes(attribute.String(k, fmt.Sprint(v)))
		}
	}
	for _, kv := range traceAttrs(ctx) {
		rec.AddAttributes(kv)
	}
	global.Logger("mph").Emit(ctx, rec)
}

// payloadKeyOrder lists the blob fields that must close a log line, in the
// order they should appear. Every entry is a key the codebase actually emits;
// adding a speculative one makes the list look authoritative when it is not.
var payloadKeyOrder = []string{"args", "text", "out", "command_output"}

// orderedFieldKeys puts identity fields first so a log line reads who and what
// before the payload blobs. Blob fields always sort last in payloadKeyOrder so
// the large payload closes the line. Middle fields sort alphabetically so the
// field order is stable across runs.
func orderedFieldKeys(fields map[string]any) []string {
	keys := make([]string, 0, len(fields))
	seen := map[string]bool{}
	for _, k := range []string{"iter", "iteration", "tool", "id", "part", "finish", "finish_reason", "outcome"} {
		if _, ok := fields[k]; ok {
			keys = append(keys, k)
			seen[k] = true
		}
	}
	var middle []string
	for k := range fields {
		if seen[k] || isPayloadKey(k) {
			continue
		}
		middle = append(middle, k)
	}
	sort.Strings(middle)
	keys = append(keys, middle...)
	for _, k := range payloadKeyOrder {
		if _, ok := fields[k]; ok && !seen[k] {
			keys = append(keys, k)
			seen[k] = true
		}
	}
	for k := range fields {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	return keys
}

func isPayloadKey(k string) bool {
	return slices.Contains(payloadKeyOrder, k)
}

func appendField(evt *zerolog.Event, k string, v any) *zerolog.Event {
	switch val := v.(type) {
	case string:
		return evt.Str(k, val)
	case int:
		return evt.Int(k, val)
	case int64:
		return evt.Int64(k, val)
	case bool:
		return evt.Bool(k, val)
	case float64:
		return evt.Float64(k, val)
	case json.RawMessage:
		return evt.RawJSON(k, val)
	case []byte:
		return evt.Bytes(k, val)
	case error:
		return evt.AnErr(k, val)
	default:
		return evt.Interface(k, v)
	}
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
