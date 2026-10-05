package otel

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
)

// runIdentityKey carries the run's telemetry identity in the context so every
// helper reached during a run can attribute its measurements without the
// values being threaded through signatures.
type runIdentityKey struct{}

// runIdentity.label is the composite "<name>:<uuid>" form. It is not a bare
// uuid: the uuid alone cannot be matched against a name a human chose.
type runIdentity struct {
	label string
	name  string
}

// WithRunIdentity tags ctx with the run's telemetry identity. Inject it before
// starting the root span, so every span and measurement beneath inherits it.
func WithRunIdentity(ctx context.Context, label, name string) context.Context {
	return context.WithValue(ctx, runIdentityKey{}, runIdentity{label: label, name: name})
}

func identityFromContext(ctx context.Context) runIdentity {
	v, _ := ctx.Value(runIdentityKey{}).(runIdentity)
	return v
}

// RunAttrs tags a measurement with the run identity. Measurements carry it
// deliberately: per-run series are what make two runs comparable, and the
// cardinality is bounded by how many runs a person actually starts.
func RunAttrs(ctx context.Context, extra ...attribute.KeyValue) []attribute.KeyValue {
	v := identityFromContext(ctx)
	if v.label == "" && v.name == "" {
		return extra
	}
	attrs := make([]attribute.KeyValue, 0, len(extra)+2)
	if v.label != "" {
		attrs = append(attrs, attribute.String("mph.run.id", v.label))
	}
	if v.name != "" {
		attrs = append(attrs, attribute.String("mph.run.name", v.name))
	}
	return append(attrs, extra...)
}

// RunSpanAttrs tags a span with the run identity. Spans carry the label but not
// the name, matching what RunAttrs stamps on measurements.
func RunSpanAttrs(ctx context.Context, extra ...attribute.KeyValue) []attribute.KeyValue {
	v := identityFromContext(ctx)
	if v.label == "" {
		return extra
	}
	attrs := make([]attribute.KeyValue, 0, len(extra)+1)
	attrs = append(attrs, extra...)
	return append(attrs, attribute.String("mph.run.id", v.label))
}
