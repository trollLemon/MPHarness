package harness

import (
	"context"
	"testing"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/trollLemon/MPHarness/internal/config"
	"github.com/trollLemon/MPHarness/internal/multipass"
)

// startRootSpan creates the root span and returns it. An empty VM name makes
// client.Info fail before it shells out to multipass, so the run ends right
// after the span is recorded: no VM, no agent, no prompt.
func startRootSpan(t *testing.T, opts Options) sdktrace.ReadOnlySpan {
	t.Helper()

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	cfg := config.Config{VM: config.VMConfig{Name: ""}, Prompt: "task"}
	if err := Start(context.Background(), nil, multipass.New(zerolog.Nop()), cfg, opts); err == nil {
		t.Fatal("Start succeeded with an empty VM name; test setup is wrong")
	}

	for _, s := range sr.Ended() {
		if s.Name() == "mph.run" {
			return s
		}
	}
	t.Fatal("no mph.run span recorded")
	return nil
}

// The run identity is injected by Start, not by the agent, so the root span
// carries it too. When it did not, mph.run and the mph.vm.* spans under it were
// unsearchable by run id while the agent's own spans were searchable.
func TestStartTagsRootSpanWithRunIdentity(t *testing.T) {
	root := startRootSpan(t, Options{RunID: "01J9", RunLabel: "baseline:01J9", RunName: "baseline"})

	var runID string
	for _, kv := range root.Attributes() {
		if string(kv.Key) == "mph.run.id" {
			runID = kv.Value.AsString()
		}
	}
	if runID != "baseline:01J9" {
		t.Errorf("mph.run.id = %q, want %q", runID, "baseline:01J9")
	}
}

// Spans take the label only, matching RunSpanAttrs on the agent side.
func TestStartRootSpanOmitsRunName(t *testing.T) {
	root := startRootSpan(t, Options{RunID: "01J9", RunLabel: "baseline:01J9", RunName: "baseline"})

	for _, kv := range root.Attributes() {
		if string(kv.Key) == "mph.run.name" {
			t.Error("mph.run must not carry mph.run.name; RunSpanAttrs omits it")
		}
	}
}

func TestStartRootSpanOmitsRunIdentityWhenUnset(t *testing.T) {
	root := startRootSpan(t, Options{RunID: "01J9"})

	for _, kv := range root.Attributes() {
		if string(kv.Key) == "mph.run.id" {
			t.Errorf("mph.run carries mph.run.id = %q with no RunLabel set", kv.Value.AsString())
		}
	}
}
