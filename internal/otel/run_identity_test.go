package otel

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
)

func TestRunAttrsCarriesIdentity(t *testing.T) {
	ctx := WithRunIdentity(t.Context(), "baseline:01J9", "baseline")

	got := RunAttrs(ctx, attribute.Int("mph.iteration", 3))

	want := map[string]string{
		"mph.run.id":    "baseline:01J9",
		"mph.run.name":  "baseline",
		"mph.iteration": "3",
	}
	if len(got) != len(want) {
		t.Fatalf("RunAttrs() = %v, want %d attributes", got, len(want))
	}
	for _, kv := range got {
		if want[string(kv.Key)] != kv.Value.String() {
			t.Errorf("attribute %q = %q, want %q", kv.Key, kv.Value.String(), want[string(kv.Key)])
		}
	}
}

func TestRunAttrsWithoutIdentity(t *testing.T) {
	if got := RunAttrs(t.Context()); len(got) != 0 {
		t.Errorf("RunAttrs() with no identity = %v, want empty", got)
	}
}

func TestRunAttrsOmitsBlankFields(t *testing.T) {
	ctx := WithRunIdentity(t.Context(), "baseline:01J9", "")

	for _, kv := range RunAttrs(ctx) {
		if string(kv.Key) == "mph.run.name" {
			t.Errorf("RunAttrs() emitted mph.run.name for an empty name")
		}
	}
}

func TestRunSpanAttrs(t *testing.T) {
	const label = "baseline:01J9"
	iter := attribute.Int("mph.iteration", 3)
	tests := []struct {
		name      string
		ctx       context.Context
		wantKeys  []string
		wantRunID string
	}{
		{"omits run id when unset", context.Background(), []string{"mph.iteration"}, ""},
		{"adds run id when set", WithRunIdentity(context.Background(), label, "baseline"), []string{"mph.iteration", "mph.run.id"}, label},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RunSpanAttrs(tt.ctx, iter)
			if len(got) != len(tt.wantKeys) {
				t.Fatalf("got %d attrs, want %d: %v", len(got), len(tt.wantKeys), got)
			}
			for i, key := range tt.wantKeys {
				if string(got[i].Key) != key {
					t.Errorf("attr %d key = %q, want %q", i, got[i].Key, key)
				}
			}
			if tt.wantRunID != "" && got[1].Value.AsString() != tt.wantRunID {
				t.Errorf("run id = %q, want %q", got[1].Value.AsString(), tt.wantRunID)
			}
		})
	}
}

// The caller passes a variadic slice it may still hold; appending into its
// backing array would corrupt it.
func TestRunSpanAttrsDoesNotMutateCallerSlice(t *testing.T) {
	extra := make([]attribute.KeyValue, 1, 4)
	extra[0] = attribute.Int("mph.iteration", 3)

	RunSpanAttrs(WithRunIdentity(t.Context(), "baseline:01J9", "baseline"), extra...)

	if len(extra) != 1 {
		t.Fatalf("caller slice grew to %d entries: %v", len(extra), extra)
	}
}
