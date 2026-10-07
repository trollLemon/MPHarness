package multipass

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const testSecret = "ghp_abc123"

// fakeResolver resolves placeholders from a fixed table. secrets.Resolve and
// secrets.Redact are the subjects of their own tests; here the table is only an
// input, and what is asserted is what reaches the VM and what does not.
type fakeResolver struct {
	values map[string]string
}

func (f fakeResolver) Lookup(name string) (string, error) {
	v, ok := f.values[name]
	if !ok {
		return "", errors.New("no secret with that name")
	}
	return v, nil
}

// newFakeMultipass writes a stand-in for the multipass binary and returns its
// path plus the path of the file it records its argv into.
//
// Recording the real argv is what lets a test prove the resolved value reached
// the VM, which cannot be inferred from Exec's return value because that value
// is redacted.
func newFakeMultipass(t *testing.T) (bin string, argsFile string) {
	t.Helper()

	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	script := filepath.Join(dir, "multipass")

	body := "#!/bin/sh\n" +
		": > \"$MPH_TEST_ARGS_FILE\"\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\" >> \"$MPH_TEST_ARGS_FILE\"; done\n" +
		"if [ -n \"$MPH_TEST_FAIL\" ]; then printf '%s\\n' \"$MPH_TEST_FAIL\" >&2; exit 1; fi\n" +
		"if [ -n \"$MPH_TEST_ECHO\" ]; then printf '%s\\n' \"$MPH_TEST_ECHO\"; fi\n"

	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake multipass: %v", err)
	}
	return script, argsFile
}

func readArgsFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}
	return string(raw)
}

// newSecretClient returns a Client wired to the fake binary and the fake
// resolver, with its log output captured.
func newSecretClient(t *testing.T, bin string, logs *bytes.Buffer, values map[string]string) *Client {
	t.Helper()
	c := New(zerolog.New(logs), fakeResolver{values: values})
	c.bin = bin
	return c
}

// TestExecSubstitutesPlaceholdersBeforeTheVM pins the whole point of the
// feature: the VM receives the real value, the log does not.
func TestExecSubstitutesPlaceholdersBeforeTheVM(t *testing.T) {
	bin, argsFile := newFakeMultipass(t)
	t.Setenv("MPH_TEST_ARGS_FILE", argsFile)
	t.Setenv("MPH_TEST_ECHO", "ran")

	var logs bytes.Buffer
	c := newSecretClient(t, bin, &logs, map[string]string{"PRO_TOKEN": testSecret})

	if _, err := c.Exec(t.Context(), "vm1", "sudo pro attach %{PRO_TOKEN}", 0); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	raw := readArgsFile(t, argsFile)
	if !strings.Contains(raw, "sudo pro attach "+testSecret) {
		t.Errorf("VM argv = %q, want it to contain the resolved value", raw)
	}
	if !strings.Contains(raw, "bash\n-c\n") {
		t.Errorf("VM argv = %q, want the usual exec/b/bash/-c shape", raw)
	}
	if strings.Contains(logs.String(), testSecret) {
		t.Errorf("log output leaked the resolved value: %s", logs.String())
	}
}

// TestExecRedactsResolvedValuesFromOutput covers a command that prints its own
// secret, which would otherwise reach the model in the tool result.
func TestExecRedactsResolvedValuesFromOutput(t *testing.T) {
	bin, argsFile := newFakeMultipass(t)
	t.Setenv("MPH_TEST_ARGS_FILE", argsFile)
	t.Setenv("MPH_TEST_ECHO", "pro attach: bad token "+testSecret)

	var logs bytes.Buffer
	c := newSecretClient(t, bin, &logs, map[string]string{"PRO_TOKEN": testSecret})

	out, err := c.Exec(t.Context(), "vm1", "attach %{PRO_TOKEN}", 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if strings.Contains(out, testSecret) {
		t.Errorf("returned output %q leaked the resolved value", out)
	}
	if !strings.Contains(out, "%{PRO_TOKEN}") {
		t.Errorf("returned output = %q, want the value masked as its placeholder", out)
	}
	if strings.Contains(logs.String(), testSecret) {
		t.Errorf("log output leaked the resolved value: %s", logs.String())
	}
}

// TestExecRedactsSecretsFromLaterCommands is the output_read case. The second
// command carries no placeholder, so it has nothing to resolve, yet a secret
// used earlier in the run is still in the file the agent is asking for.
func TestExecRedactsSecretsFromLaterCommands(t *testing.T) {
	bin, argsFile := newFakeMultipass(t)
	t.Setenv("MPH_TEST_ARGS_FILE", argsFile)

	var logs bytes.Buffer
	c := newSecretClient(t, bin, &logs, map[string]string{"PRO_TOKEN": testSecret})

	t.Setenv("MPH_TEST_ECHO", "ran")
	if _, err := c.Exec(t.Context(), "vm1", "attach %{PRO_TOKEN}", 0); err != nil {
		t.Fatalf("first Exec: %v", err)
	}

	t.Setenv("MPH_TEST_ECHO", "s3cret="+testSecret)
	out, err := c.Exec(t.Context(), "vm1", "cat /tmp/mph-output/run1/raw", 0)
	if err != nil {
		t.Fatalf("second Exec: %v", err)
	}

	if strings.Contains(out, testSecret) {
		t.Errorf("a later marker-free command returned the secret: %q", out)
	}
	if !strings.Contains(out, "s3cret=%{PRO_TOKEN}") {
		t.Errorf("returned output = %q, want the value masked as its placeholder", out)
	}
}

// TestExecRedactsEverySecretUsedInTheRun catches a redaction that only handles
// the secret belonging to the current command.
func TestExecRedactsEverySecretUsedInTheRun(t *testing.T) {
	bin, argsFile := newFakeMultipass(t)
	t.Setenv("MPH_TEST_ARGS_FILE", argsFile)

	var logs bytes.Buffer
	c := newSecretClient(t, bin, &logs, map[string]string{
		"PRO_TOKEN": "aaa111",
		"GH_PAT":    "bbb222",
	})

	t.Setenv("MPH_TEST_ECHO", "ran")
	if _, err := c.Exec(t.Context(), "vm1", "attach %{PRO_TOKEN}", 0); err != nil {
		t.Fatalf("first Exec: %v", err)
	}
	if _, err := c.Exec(t.Context(), "vm1", "auth %{GH_PAT}", 0); err != nil {
		t.Fatalf("second Exec: %v", err)
	}

	t.Setenv("MPH_TEST_ECHO", "aaa111 bbb222")
	out, err := c.Exec(t.Context(), "vm1", "cat /tmp/mph-output/run1/raw", 0)
	if err != nil {
		t.Fatalf("third Exec: %v", err)
	}

	if want := "%{PRO_TOKEN} %{GH_PAT}"; strings.TrimSpace(out) != want {
		t.Errorf("returned output = %q, want %q", strings.TrimSpace(out), want)
	}
}

// TestExecLeavesMarkerFreeOutputUntouched guards the common case: a run with no
// placeholders must not have its output altered by a redaction that found
// nothing to do.
func TestExecLeavesMarkerFreeOutputUntouched(t *testing.T) {
	bin, argsFile := newFakeMultipass(t)
	t.Setenv("MPH_TEST_ARGS_FILE", argsFile)
	t.Setenv("MPH_TEST_ECHO", "hello world")

	var logs bytes.Buffer
	c := newSecretClient(t, bin, &logs, map[string]string{"PRO_TOKEN": testSecret})

	out, err := c.Exec(t.Context(), "vm1", "echo hello world", 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out != "hello world\n" {
		t.Errorf("returned output = %q, want %q", out, "hello world\n")
	}
}

// TestExecRedactsSecretsFromErrors covers the failure path: the VM's stderr goes
// into the error string, which becomes the tool result the model reads.
func TestExecRedactsSecretsFromErrors(t *testing.T) {
	bin, argsFile := newFakeMultipass(t)
	t.Setenv("MPH_TEST_ARGS_FILE", argsFile)
	t.Setenv("MPH_TEST_FAIL", "pro attach: bad token "+testSecret)

	var logs bytes.Buffer
	c := newSecretClient(t, bin, &logs, map[string]string{"PRO_TOKEN": testSecret})

	_, err := c.Exec(t.Context(), "vm1", "attach %{PRO_TOKEN}", 0)
	if err == nil {
		t.Fatal("Exec succeeded, want the fake binary's exit 1 to surface")
	}

	msg := err.Error()
	if strings.Contains(msg, testSecret) {
		t.Errorf("error text leaked the resolved value: %s", msg)
	}
	if !strings.Contains(msg, "%{PRO_TOKEN}") {
		t.Errorf("error text = %s, want the command echoed with its placeholder", msg)
	}
	if strings.Contains(logs.String(), testSecret) {
		t.Errorf("log output leaked the resolved value: %s", logs.String())
	}
}

// TestExecRejectsUnsafeValues pins that a value which would be reinterpreted by
// the VM's shell fails before any exec happens.
func TestExecRejectsUnsafeValues(t *testing.T) {
	bin, argsFile := newFakeMultipass(t)
	t.Setenv("MPH_TEST_ARGS_FILE", argsFile)
	t.Setenv("MPH_TEST_ECHO", "ran")

	var logs bytes.Buffer
	c := newSecretClient(t, bin, &logs, map[string]string{"PRO_TOKEN": "$(id -u)"})

	if _, err := c.Exec(t.Context(), "vm1", "attach %{PRO_TOKEN}", 0); err == nil {
		t.Fatal("Exec succeeded, want a value containing shell metacharacters rejected")
	}

	raw := readArgsFile(t, argsFile)
	if strings.TrimSpace(raw) != "" {
		t.Errorf("argv = %q, want the binary never invoked", raw)
	}
}

// TestExecRejectsPlaceholdersWithoutAResolver covers a nil resolver: passing the
// placeholder through to the shell would fail confusingly inside the VM.
func TestExecRejectsPlaceholdersWithoutAResolver(t *testing.T) {
	bin, argsFile := newFakeMultipass(t)
	t.Setenv("MPH_TEST_ARGS_FILE", argsFile)
	t.Setenv("MPH_TEST_ECHO", "ran")

	c := New(zerolog.Nop(), nil)
	c.bin = bin

	if _, err := c.Exec(t.Context(), "vm1", "attach %{PRO_TOKEN}", 0); err == nil {
		t.Fatal("Exec succeeded, want an unresolvable placeholder to fail")
	}

	raw := readArgsFile(t, argsFile)
	if strings.TrimSpace(raw) != "" {
		t.Errorf("argv = %q, want the binary never invoked", raw)
	}
}

func TestExecLeavesMarkerFreeCommandsWorkingWithoutAResolver(t *testing.T) {
	bin, argsFile := newFakeMultipass(t)
	t.Setenv("MPH_TEST_ARGS_FILE", argsFile)
	t.Setenv("MPH_TEST_ECHO", "ran")

	c := New(zerolog.Nop(), nil)
	c.bin = bin

	out, err := c.Exec(t.Context(), "vm1", "df -h", 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out != "ran\n" {
		t.Errorf("returned output = %q, want %q", out, "ran\n")
	}
}

// TestExecDoesNotRecordSecretsInSpans guards the telemetry path. A resolved
// value in a span attribute reaches Loki and the Grafana dashboards, which is
// the same leak as putting it in a log line.
func TestExecDoesNotRecordSecretsInSpans(t *testing.T) {
	bin, argsFile := newFakeMultipass(t)
	t.Setenv("MPH_TEST_ARGS_FILE", argsFile)
	t.Setenv("MPH_TEST_ECHO", "ran")

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	var logs bytes.Buffer
	c := newSecretClient(t, bin, &logs, map[string]string{"PRO_TOKEN": testSecret})

	if _, err := c.Exec(t.Context(), "vm1", "sudo pro attach %{PRO_TOKEN}", 0); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	var execSpan sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == "multipass.exec" {
			execSpan = s
			break
		}
	}
	if execSpan == nil {
		t.Fatal("no multipass.exec span recorded, so this test would prove nothing")
	}

	var sawCommand bool
	for _, attr := range execSpan.Attributes() {
		var vals []string
		switch attr.Value.Type() {
		case attribute.STRING:
			vals = []string{attr.Value.AsString()}
		case attribute.STRINGSLICE:
			vals = attr.Value.AsStringSlice()
		}
		for _, v := range vals {
			if attr.Key == "mph.command" {
				sawCommand = true
			}
			if strings.Contains(v, testSecret) {
				t.Errorf("span attribute %s leaked the resolved value: %q", attr.Key, v)
			}
		}
	}
	if !sawCommand {
		t.Error("mph.command attribute missing, so the attribute assertions above proved nothing")
	}
}
