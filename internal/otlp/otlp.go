package otlp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
)

type ctxKey string

const skipHookKey ctxKey = "otel-skip-hook"

// Defaults for a Config left partially blank. Exported so mph can present the
// same values in help text and layer environment variables over the YAML without
// the two drifting apart.
const (
	DefaultEndpoint    = "localhost:4317"
	DefaultServiceName = "mph"
)

// Exporter and batcher tunables.
const (
	exporterTimeout = 10 * time.Second
	exportTimeout   = 5 * time.Second
	logInterval     = 1 * time.Second
)

type Config struct {
	Enabled            bool
	Endpoint           string
	ServiceName        string
	ResourceAttributes map[string]string
	RunID              string
	RunName            string
}

// WithDefaults fills blank endpoint and service fields. It is idempotent, so
// callers that need the resolved values and Setup can both apply it.
func (c Config) WithDefaults() Config {
	if c.Endpoint == "" {
		c.Endpoint = DefaultEndpoint
	}
	if c.ServiceName == "" {
		c.ServiceName = DefaultServiceName
	}
	return c
}

func Setup(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	if !cfg.Enabled {
		return func(context.Context) error { return nil }, nil
	}
	if os.Getenv("OTEL_SDK_DISABLED") == "true" {
		return func(context.Context) error { return nil }, nil
	}

	cfg = cfg.WithDefaults()

	attrs := []attribute.KeyValue{
		attribute.String("service.name", cfg.ServiceName),
	}
	for k, v := range cfg.ResourceAttributes {
		attrs = append(attrs, attribute.String(k, v))
	}
	if strings.TrimSpace(cfg.RunID) != "" {
		attrs = append(attrs, attribute.String("service.instance.id", cfg.RunID))
	}
	if strings.TrimSpace(cfg.RunName) != "" {
		attrs = append(attrs, attribute.String("mph.run.name", cfg.RunName))
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

	logExp, err := otlploggrpc.New(ctx,
		otlploggrpc.WithEndpoint(cfg.Endpoint),
		otlploggrpc.WithInsecure(),
		otlploggrpc.WithTimeout(exporterTimeout),
	)
	if err != nil {
		return nil, err
	}

	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp,
			sdklog.WithExportInterval(logInterval),
			sdklog.WithExportTimeout(exportTimeout),
		)),
		sdklog.WithResource(res),
	)
	otel.SetLoggerProvider(lp)

	return lp.Shutdown, nil
}

type Hook struct {
	logger otellog.Logger
}

func NewHook(name string) *Hook {
	return &Hook{
		logger: otel.Logger(name),
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
	h.logger.Emit(ctx, newRecord(level, msg))
}

func newRecord(level zerolog.Level, msg string) otellog.Record {
	rec := otellog.Record{}
	rec.SetTimestamp(time.Now())
	rec.SetObservedTimestamp(time.Now())
	rec.SetSeverity(convertLevel(level))
	rec.SetSeverityText(level.String())
	rec.SetBody(attribute.StringValue(msg))
	return rec
}

func LogEvent(ctx context.Context, logger zerolog.Logger, level zerolog.Level, msg string, fields map[string]any) {
	skipCtx := context.WithValue(ctx, skipHookKey, true)
	// A filtered level makes WithLevel return a nil *Event, whose zerolog
	// methods are nil-safe, so the OTel emit below still has to happen.
	evt := logger.WithLevel(level).Ctx(skipCtx).CallerSkipFrame(1)
	rec := newRecord(level, msg)
	for _, k := range orderedFieldKeys(fields) {
		evt = addField(evt, &rec, k, fields[k])
	}
	if evt != nil {
		evt.Msg(msg)
	}
	runLogger.Emit(ctx, rec)
}

func addField(evt *zerolog.Event, rec *otellog.Record, k string, v any) *zerolog.Event {
	switch val := v.(type) {
	case string:
		rec.AddAttributes(attribute.String(k, val))
		return evt.Str(k, val)
	case int:
		rec.AddAttributes(attribute.Int(k, val))
		return evt.Int(k, val)
	case int64:
		rec.AddAttributes(attribute.Int64(k, val))
		return evt.Int64(k, val)
	case bool:
		rec.AddAttributes(attribute.Bool(k, val))
		return evt.Bool(k, val)
	case float64:
		rec.AddAttributes(attribute.Float64(k, val))
		return evt.Float64(k, val)
	case json.RawMessage:
		rec.AddAttributes(attribute.String(k, string(val)))
		return evt.RawJSON(k, val)
	case []byte:
		rec.AddAttributes(attribute.String(k, string(val)))
		return evt.Bytes(k, val)
	case error:
		rec.AddAttributes(attribute.String(k, val.Error()))
		return evt.AnErr(k, val)
	default:
		rec.AddAttributes(attribute.String(k, fmt.Sprint(v)))
		return evt.Interface(k, v)
	}
}

// runLogger is resolved once. go.opentelemetry.io/otel/log/global hands back a
// delegating logger, so this is safe to take before Setup installs the real
// provider, and it avoids rebuilding a logger for every log line.
var runLogger = otel.Logger("mph")

// payloadKeyOrder lists the blob fields that must close a log line, in the
// order they should appear. Every entry is a key the codebase actually emits;
// adding a speculative one makes the list look authoritative when it is not.
var payloadKeyOrder = []string{"args", "text", "command_output"}

var eventFieldOrder = []string{"event", "iter", "iteration", "tool", "id", "part", "finish", "finish_reason", "outcome"}

// orderedFieldKeys puts the event kind and identity fields first so a log line reads who and what
// before the payload blobs. Blob fields always sort last in payloadKeyOrder so
// the large payload closes the line. Middle fields sort alphabetically so the
// field order is stable across runs.
func orderedFieldKeys(fields map[string]any) []string {
	keys := appendPresent(make([]string, 0, len(fields)), fields, eventFieldOrder)
	middle := make([]string, 0, len(fields)-len(keys))
	for k := range fields {
		if slices.Contains(eventFieldOrder, k) || slices.Contains(payloadKeyOrder, k) {
			continue
		}
		middle = append(middle, k)
	}
	slices.Sort(middle)
	keys = append(keys, middle...)
	return appendPresent(keys, fields, payloadKeyOrder)
}

func appendPresent(keys []string, fields map[string]any, order []string) []string {
	for _, k := range order {
		if _, ok := fields[k]; ok {
			keys = append(keys, k)
		}
	}
	return keys
}

var severityByLevel = map[zerolog.Level]otellog.Severity{
	zerolog.TraceLevel: otellog.SeverityTrace,
	zerolog.DebugLevel: otellog.SeverityDebug,
	zerolog.InfoLevel:  otellog.SeverityInfo,
	zerolog.WarnLevel:  otellog.SeverityWarn,
	zerolog.ErrorLevel: otellog.SeverityError,
	zerolog.FatalLevel: otellog.SeverityFatal,
	zerolog.PanicLevel: otellog.SeverityFatal2,
}

func convertLevel(level zerolog.Level) otellog.Severity {
	if severity, ok := severityByLevel[level]; ok {
		return severity
	}
	return otellog.SeverityUndefined
}
