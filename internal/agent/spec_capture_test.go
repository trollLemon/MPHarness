package agent

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestToolSpecs(t *testing.T) {
	tests := []struct {
		name        string
		tool        string
		prop        string
		enabled     bool
		wantPresent bool
	}{
		{"multipass_exec always present", "multipass_exec", "command", true, true},
		{"capture flag on multipass_exec", "multipass_exec", "capture", true, true},
		{"output_search when enabled", "output_search", "output_id", true, true},
		{"output_search pattern when enabled", "output_search", "pattern", true, true},
		{"output_search omitted when disabled", "output_search", "output_id", false, false},
		{"output_read when enabled", "output_read", "offset", true, true},
		{"output_read omitted when disabled", "output_read", "offset", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Config{Output: config.OutputConfig{Enabled: tt.enabled}}
			if tt.enabled {
				cfg.Output.Mode = "auto"
				cfg.Output.SearchMaxMatches = 200
			}
			var found *Spec
			for _, s := range Specs(cfg) {
				if s.Name == tt.tool {
					s := s
					found = &s
					break
				}
			}
			if !tt.wantPresent {
				if found != nil {
					t.Fatalf("%s must be omitted when output.enabled=false", tt.tool)
				}
				return
			}
			if found == nil {
				t.Fatalf("missing %s", tt.tool)
			}
			props, _ := found.Parameters["properties"].(map[string]any)
			if _, ok := props[tt.prop]; !ok {
				t.Fatalf("%s lacks %s: %v", tt.tool, tt.prop, found.Parameters)
			}
		})
	}
}

func TestCallMultipassExecCapture(t *testing.T) {
	tests := []struct {
		name        string
		cutoff      int64
		args        map[string]any
		sizeOut     string
		exitCode    string
		wantCapture bool
		wantSubs    []string
	}{
		{
			name:        "always mode captures without flag",
			cutoff:      10,
			args:        map[string]any{"command": "echo hi"},
			sizeOut:     "1000",
			exitCode:    "0",
			wantCapture: true,
		},
		{
			name:        "inline nonzero exit reports code",
			cutoff:      1 << 20,
			args:        map[string]any{"command": "false", "capture": true},
			sizeOut:     "5",
			exitCode:    "3",
			wantCapture: false,
			wantSubs:    []string{"(exit code: 3)"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Config{
				VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
				Model:  "m",
				Prompt: "p",
				Output: config.OutputConfig{Enabled: true, Mode: "auto", MaxCommandSize: 1 << 20, MaxTotalSize: 1 << 26, SearchMaxMatches: 200},
			}
			if tt.wantCapture {
				cfg.Output.Mode = "always"
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
					return rest[:end+1] + tt.exitCode, nil
				case strings.Contains(cmd, "wc -c"):
					return tt.sizeOut, nil
				case strings.Contains(cmd, "wc -l"):
					return "50", nil
				case strings.HasPrefix(strings.TrimSpace(cmd), "cat "):
					return "oops", nil
				default:
					return "", nil
				}
			}}
			store := output.NewStore(f, "vm", "/tmp/mph-output/run1", cfg.Output, tt.cutoff, nil)
			got, err := Call(context.Background(), nil, cfg, store, "multipass_exec", tt.args)
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if isCaptureEnvelope(got) != tt.wantCapture {
				t.Fatalf("capture=%v, got %q", tt.wantCapture, got)
			}
			for _, want := range tt.wantSubs {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q in %q", want, got)
				}
			}
		})
	}
}

func TestCallOutputSearchReportsAbsoluteLines(t *testing.T) {
	tests := []struct {
		name      string
		grepLines string
		grepExit  string
		wantSubs  []string
		wantNotIn []string
	}{
		{
			name:      "flat line numbers, no chunk grouping",
			grepLines: "1513:hit\n2000000:last",
			grepExit:  "0",
			wantSubs:  []string{`"matches":[1513,2000000]`, `"total_matches":2`, `"truncated":false`},
			wantNotIn: []string{`"chunk"`, `"chunks_matched"`, `"lines"`},
		},
		{
			name:      "no match serializes an empty list",
			grepExit:  "1",
			wantSubs:  []string{`"matches":[]`, `"total_matches":0`},
			wantNotIn: []string{"null"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Config{
				VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
				Model:  "m",
				Prompt: "p",
				Output: config.OutputConfig{Enabled: true, Mode: "always", MaxCommandSize: 1 << 20, MaxTotalSize: 1 << 26, SearchMaxMatches: 200},
			}
			f := &fakeOutputExec{fn: func(name, cmd string) (string, error) {
				switch {
				case strings.Contains(cmd, "__MPH_GREP_EXIT_"):
					return tt.grepLines + "\n" + grepMarker(cmd) + tt.grepExit, nil
				case strings.Contains(cmd, "grep -a -c"):
					return "2", nil
				case strings.Contains(cmd, "__MPH_EXIT_"):
					return exitMarker(cmd) + "0", nil
				case strings.Contains(cmd, "wc -c"):
					return "1000", nil
				case strings.Contains(cmd, "wc -l"):
					return "2000000", nil
				default:
					return "", nil
				}
			}}
			store := output.NewStore(f, "vm", "/tmp/mph-output/run1", cfg.Output, 10, nil)
			id := capturedOutputID(captureEnvelope(t, cfg, store))
			res, err := Call(context.Background(), nil, cfg, store, "output_search", map[string]any{
				"output_id": id, "pattern": "hit",
			})
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			for _, want := range tt.wantSubs {
				if !strings.Contains(res, want) {
					t.Errorf("missing %q in %q", want, res)
				}
			}
			for _, absent := range tt.wantNotIn {
				if strings.Contains(res, absent) {
					t.Errorf("must not contain %q: %q", absent, res)
				}
			}
		})
	}
}

func TestCaptureSuccessContentHasNoChunkFields(t *testing.T) {
	got := buildCaptureSuccessContent(&output.Handle{
		ID: "abc", Path: "/tmp/x/output.txt", TotalLines: 64000, TotalBytes: 7340032, ExitCode: 0,
	})
	for _, want := range []string{`"output_id":"abc"`, `"total_lines":64000`, `"total_bytes":7340032`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	for _, absent := range []string{"chunk", "chunks"} {
		if strings.Contains(got, absent) {
			t.Errorf("must not contain %q: %q", absent, got)
		}
	}
}

// captureEnvelope captures "echo hi" through the agent dispatcher and returns
// the handle envelope, so a test can reuse the registered output_id.
func captureEnvelope(t *testing.T, cfg config.Config, store *output.Store) string {
	t.Helper()
	got, err := Call(context.Background(), nil, cfg, store, "multipass_exec", map[string]any{"command": "echo hi"})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if !isCaptureEnvelope(got) {
		t.Fatalf("want capture envelope, got %q", got)
	}
	return got
}

// grepMarker extracts the per-capture exit marker the search command echoes.
func grepMarker(cmd string) string { return markerIn(cmd, "__MPH_GREP_EXIT_") }

// exitMarker extracts the per-capture exit marker the capture command echoes.
func exitMarker(cmd string) string { return markerIn(cmd, "__MPH_EXIT_") }

func markerIn(cmd, prefix string) string {
	rest := cmd[strings.Index(cmd, prefix):]
	return rest[:strings.Index(rest, ":")+1]
}

func TestCallNilStoreDisabled(t *testing.T) {
	tests := []struct {
		name string
		tool string
		args map[string]any
	}{
		{"output_search", "output_search", map[string]any{"output_id": "x", "pattern": "y"}},
		{"output_read", "output_read", map[string]any{"output_id": "x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Call(context.Background(), nil, config.Config{}, nil, tt.tool, tt.args); err == nil {
				t.Fatalf("want disabled error for %s with nil store", tt.tool)
			}
		})
	}
}

func TestCallOutputReadRoundTrip(t *testing.T) {
	cfg := config.Config{
		VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:  "m",
		Prompt: "p",
		Output: config.OutputConfig{Enabled: true, Mode: "always", MaxCommandSize: 1 << 20, MaxTotalSize: 1 << 26, SearchMaxMatches: 200},
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
		"output_id": id, "offset": json.Number("5"), "limit": json.Number("2"),
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
		Output: config.OutputConfig{Enabled: true, Mode: "always", MaxCommandSize: 1 << 20, MaxTotalSize: 1 << 26, SearchMaxMatches: 200},
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
		default:
			if strings.Contains(cmd, "split ") {
				t.Fatalf("chunking must be gone, got %q", cmd)
			}
			return "", nil
		}
	}}
	store := output.NewStore(f, "vm", "/tmp/mph-output/run1", cfg.Output, 10, nil)
	agt := NewAgent(zerolog.New(&buf), &mockKronk{}, Options{
		MaxIterations: 1,
		ChatTimeout:   time.Second,
		TotalTimeout:  time.Second,
		LLM:           defaultLLMConfig(),
		RunID:         "run-1",
	})
	agt.store = store
	tc := model.ResponseToolCall{ID: "c1", Type: "function", Function: model.ResponseToolCallFunction{
		Name: "multipass_exec", Arguments: model.ToolCallArguments{"command": "echo hi"},
	}}
	_, outputs := agt.executeToolCalls(context.Background(), multipass.New(zerolog.Nop(), nil), cfg, []model.ResponseToolCall{tc})
	if len(outputs) != 1 || !strings.HasPrefix(outputs[0], "<captured output_id=") {
		t.Fatalf("want capture placeholder, got %v", outputs)
	}
	if !strings.Contains(buf.String(), "too large for inline context") {
		t.Fatalf("missing capture attribution log line:\n%s", buf.String())
	}
}

// Each command's own output must be observable while the run is in flight.
// Only the final output is logged at the end, so without a per-command record a
// long multi-command run gives no way to attribute output to a step.
func TestExecuteToolCallsLogsPerCommandOutput(t *testing.T) {
	var buf bytes.Buffer
	cfg := config.Config{
		VM:         config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:      "m",
		Prompt:     "p",
		Output:     config.OutputConfig{Enabled: true, Mode: "auto", MaxCommandSize: 1 << 20, MaxTotalSize: 1 << 26, SearchMaxMatches: 200},
		Truncation: config.TruncationConfig{CommandOutput: 8, LogContent: 8, ToolResult: 8, Nudge: 8},
	}
	f := &fakeOutputExec{fn: func(name, cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "grep -a -c"):
			return "0", nil
		case strings.Contains(cmd, "__MPH_EXIT_"):
			return exitMarker(cmd) + "0", nil
		case strings.Contains(cmd, "cat "):
			return "hi there", nil
		case strings.Contains(cmd, "wc -c"):
			return "4", nil
		case strings.Contains(cmd, "wc -l"):
			return "1", nil
		}
		return "", nil
	}}
	store := output.NewStore(f, "vm", "/tmp/mph-output/run1", cfg.Output, 1<<20, nil)
	agt := NewAgent(zerolog.New(&buf), &mockKronk{}, Options{
		MaxIterations: 1,
		ChatTimeout:   time.Second,
		TotalTimeout:  time.Second,
		LLM:           defaultLLMConfig(),
		RunID:         "run-cmdlog",
	})
	agt.store = store
	tc := model.ResponseToolCall{ID: "c1", Type: "function", Function: model.ResponseToolCallFunction{
		Name: "multipass_exec", Arguments: model.ToolCallArguments{"command": "echo hi", "capture": true},
	}}
	agt.executeToolCalls(context.Background(), multipass.New(zerolog.Nop(), nil), cfg, []model.ResponseToolCall{tc})

	found := false
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["message"] != "command output" {
			continue
		}
		found = true
		if rec["command_output"] == nil {
			t.Errorf("missing command_output key: %v", rec)
		}
	}
	if !found {
		t.Fatalf("no per-command command output event:\n%s", buf.String())
	}
}
