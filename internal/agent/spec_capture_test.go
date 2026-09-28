package agent

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ardanlabs/kronk/sdk/kronk/model"
	"github.com/rs/zerolog"

	"github.com/trollLemon/MPHarness/internal/config"
	"github.com/trollLemon/MPHarness/internal/multipass"
	"github.com/trollLemon/MPHarness/internal/output"
)

type fakeOutputExec struct {
	fn func(name, command string) (string, error)
}

func (f *fakeOutputExec) Exec(_ context.Context, name, command string, _ int) (string, error) {
	return f.fn(name, command)
}

func TestMultipassExecCaptureFlagPresent(t *testing.T) {
	cfg := config.Config{Output: config.OutputConfig{Enabled: true, Mode: "auto", ChunkLines: 500, SearchMaxMatches: 200}}
	specs := Specs(cfg)
	byName := map[string]Spec{}
	for _, s := range specs {
		byName[s.Name] = s
	}
	exec, ok := byName["multipass_exec"]
	if !ok {
		t.Fatalf("missing multipass_exec")
	}
	props, _ := exec.Parameters["properties"].(map[string]any)
	if _, ok := props["capture"]; !ok {
		t.Fatalf("multipass_exec lacks capture flag: %v", exec.Parameters)
	}
	if _, ok := byName["output_search"]; !ok {
		t.Fatalf("missing output_search when output.enabled")
	}
	off := config.Config{Output: config.OutputConfig{Enabled: false}}
	for _, s := range Specs(off) {
		if s.Name == "output_search" {
			t.Fatalf("output_search must be omitted when output.enabled=false")
		}
	}
}

func TestCallAlwaysModeCapturesWithoutFlag(t *testing.T) {
	cfg := config.Config{
		VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:  "m",
		Prompt: "p",
		Output: config.OutputConfig{Enabled: true, Mode: "always", ChunkLines: 10, MaxCommandSize: 1 << 20, MaxTotalSize: 1 << 26, SearchMaxMatches: 200},
	}
	f := &fakeOutputExec{fn: func(name, cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "df -P"):
			return "99999999", nil
		case strings.Contains(cmd, "mkdir"):
			return "", nil
		case strings.Contains(cmd, "__MPH_EXIT"):
			return "__MPH_EXIT:0", nil
		case strings.Contains(cmd, "wc -c"):
			return "1000", nil
		case strings.Contains(cmd, "wc -l"):
			return "50", nil
		case strings.Contains(cmd, "split -l"):
			return "", nil
		default:
			return "", nil
		}
	}}
	store := output.NewStore(f, "vm", "/tmp/mph-output/run1", cfg.Output, 10, nil)
	got, err := Call(context.Background(), nil, cfg, store, "multipass_exec", map[string]any{"command": "echo hi"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !isCaptureEnvelope(got) {
		t.Fatalf("always mode must capture without flag, got %q", got)
	}
}

func TestCallNilStoreIgnoresCapture(t *testing.T) {
	if _, err := Call(context.Background(), nil, config.Config{}, nil, "output_search", map[string]any{"output_id": "x", "pattern": "y"}); err == nil {
		t.Fatalf("want disabled error for output_search with nil store")
	}
}

func TestCallInlineNonzeroExitReportsCode(t *testing.T) {
	cfg := config.Config{
		VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:  "m",
		Prompt: "p",
		Output: config.OutputConfig{Enabled: true, Mode: "auto", ChunkLines: 500, MaxCommandSize: 1 << 20, MaxTotalSize: 1 << 26, SearchMaxMatches: 200},
	}
	f := &fakeOutputExec{fn: func(name, cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "df -P"):
			return "99999999", nil
		case strings.Contains(cmd, "mkdir"):
			return "", nil
		case strings.Contains(cmd, "__MPH_EXIT_"):
			start := strings.Index(cmd, "__MPH_EXIT_")
			rest := cmd[start:]
			end := strings.Index(rest, ":")
			return rest[:end+1] + "3", nil
		case strings.Contains(cmd, "wc -c"):
			return "5", nil
		case strings.HasPrefix(strings.TrimSpace(cmd), "cat "):
			return "oops", nil
		default:
			return "", nil
		}
	}}
	store := output.NewStore(f, "vm", "/tmp/mph-output/run1", cfg.Output, 1<<20, nil)
	got, err := Call(context.Background(), nil, cfg, store, "multipass_exec", map[string]any{"command": "false", "capture": true})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if isCaptureEnvelope(got) || !strings.Contains(got, "(exit code: 3)") {
		t.Fatalf("want inline text with exit code, got %q", got)
	}
}

func TestOutputReadSchemaGated(t *testing.T) {
	on := config.Config{Output: config.OutputConfig{Enabled: true, Mode: "auto", ChunkLines: 500, SearchMaxMatches: 200}}
	found := false
	for _, s := range Specs(on) {
		if s.Name == "output_read" {
			found = true
			props, _ := s.Parameters["properties"].(map[string]any)
			if _, ok := props["offset"]; !ok {
				t.Fatalf("output_read lacks offset: %v", s.Parameters)
			}
		}
	}
	if !found {
		t.Fatalf("missing output_read when output.enabled")
	}
	off := config.Config{Output: config.OutputConfig{Enabled: false}}
	for _, s := range Specs(off) {
		if s.Name == "output_read" {
			t.Fatalf("output_read must be omitted when output.enabled=false")
		}
	}
}

func TestCallOutputReadRoundTrip(t *testing.T) {
	cfg := config.Config{
		VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:  "m",
		Prompt: "p",
		Output: config.OutputConfig{Enabled: true, Mode: "always", ChunkLines: 10, MaxCommandSize: 1 << 20, MaxTotalSize: 1 << 26, SearchMaxMatches: 200},
	}
	f := &fakeOutputExec{fn: func(name, cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "df -P"):
			return "99999999", nil
		case strings.Contains(cmd, "mkdir"):
			return "", nil
		case strings.Contains(cmd, "__MPH_EXIT_"):
			start := strings.Index(cmd, "__MPH_EXIT_")
			rest := cmd[start:]
			end := strings.Index(rest, ":")
			return rest[:end+1] + "0", nil
		case strings.Contains(cmd, "wc -c"):
			return "1000", nil
		case strings.Contains(cmd, "wc -l"):
			return "50", nil
		case strings.Contains(cmd, "split -l"):
			return "", nil
		case strings.Contains(cmd, "sed -n"):
			return "line5\nline6\n", nil
		default:
			return "", nil
		}
	}}
	store := output.NewStore(f, "vm", "/tmp/mph-output/run1", cfg.Output, 10, nil)
	capGot, err := Call(context.Background(), nil, cfg, store, "multipass_exec", map[string]any{"command": "echo hi"})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	id := capturedOutputID(capGot)
	if id == "" {
		t.Fatalf("no output_id in %q", capGot)
	}
	got, err := Call(context.Background(), nil, cfg, store, "output_read", map[string]any{
		"output_id": id, "offset": float64(5), "limit": float64(2),
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, want := range []string{`"offset":5`, "line5", "line6", `"total_lines":50`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
}

func TestCaptureLogsAttributionLine(t *testing.T) {
	var buf bytes.Buffer
	cfg := config.Config{
		VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:  "m",
		Prompt: "p",
		Output: config.OutputConfig{Enabled: true, Mode: "always", ChunkLines: 10, MaxCommandSize: 1 << 20, MaxTotalSize: 1 << 26, SearchMaxMatches: 200},
	}
	f := &fakeOutputExec{fn: func(name, cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "df -P"):
			return "99999999", nil
		case strings.Contains(cmd, "mkdir"):
			return "", nil
		case strings.Contains(cmd, "__MPH_EXIT_"):
			start := strings.Index(cmd, "__MPH_EXIT_")
			rest := cmd[start:]
			end := strings.Index(rest, ":")
			return rest[:end+1] + "0", nil
		case strings.Contains(cmd, "wc -c"):
			return "1000", nil
		case strings.Contains(cmd, "wc -l"):
			return "50", nil
		case strings.Contains(cmd, "split -l"):
			return "", nil
		default:
			return "", nil
		}
	}}
	store := output.NewStore(f, "vm", "/tmp/mph-output/run1", cfg.Output, 10, nil)
	agt := NewAgent(zerolog.New(&buf), &mockKronk{}, 1, time.Second, time.Second, defaultLLMConfig(), "run-1")
	tc := model.ResponseToolCall{ID: "c1", Type: "function", Function: model.ResponseToolCallFunction{
		Name: "multipass_exec", Arguments: model.ToolCallArguments{"command": "echo hi"},
	}}
	_, outputs := agt.executeToolCalls(context.Background(), multipass.New(zerolog.Nop()), cfg, store, []model.ResponseToolCall{tc})
	if len(outputs) != 1 || !strings.HasPrefix(outputs[0], "<captured output_id=") {
		t.Fatalf("want capture placeholder, got %v", outputs)
	}
	if !strings.Contains(buf.String(), "too large for inline context") {
		t.Fatalf("missing capture attribution log line:\n%s", buf.String())
	}
}
