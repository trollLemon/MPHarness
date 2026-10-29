package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/trollLemon/MPHarness/internal/agent"
	"github.com/trollLemon/MPHarness/internal/config"
	"github.com/trollLemon/MPHarness/internal/output"
)

type fakeOutputExec struct {
	fn func(name, command string) (string, error)
}

func (f *fakeOutputExec) Exec(_ context.Context, name, command string) (string, error) {
	return f.fn(name, command)
}

type toolEnvelope struct {
	Status string `json:"status"`
	Data   struct {
		Result struct {
			OutputID string `json:"output_id"`
			Captured bool   `json:"captured"`
		} `json:"result"`
	} `json:"data"`
}

func parseToolEnvelope(s string) (toolEnvelope, bool) {
	var v toolEnvelope
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return toolEnvelope{}, false
	}
	return v, v.Status == "SUCCESS" && v.Data.Result.OutputID != ""
}

func TestToolSpecs(t *testing.T) {
	tests := []struct {
		name        string
		tool        string
		prop        string
		enabled     bool
		wantPresent bool
	}{
		{"vm_exec always present", "vm_exec", "command", true, true},
		{"capture flag on vm_exec", "vm_exec", "capture", true, true},
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
			var found *agent.Spec
			for _, s := range agent.Specs(cfg) {
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

func TestTaskStatusSpec(t *testing.T) {
	var found *agent.Spec
	for _, s := range agent.Specs(config.Config{}) {
		if s.Name == "task_status" {
			s := s
			found = &s
			break
		}
	}
	if found == nil {
		t.Fatal("missing task_status")
	}

	props, _ := found.Parameters["properties"].(map[string]any)
	status, _ := props["status"].(map[string]any)
	enum, _ := status["enum"].([]string)
	wantEnum := []string{"Completed", "Failed", "Error"}
	if len(enum) != len(wantEnum) {
		t.Fatalf("status enum = %v, want %v", enum, wantEnum)
	}
	for i, want := range wantEnum {
		if enum[i] != want {
			t.Errorf("status enum[%d] = %q, want %q", i, enum[i], want)
		}
	}

	reason, _ := props["reason"].(map[string]any)
	if reason["type"] != "string" {
		t.Errorf("reason type = %v, want string", reason["type"])
	}

	required, _ := found.Parameters["required"].([]string)
	if len(required) != 2 || required[0] != "status" || required[1] != "reason" {
		t.Errorf("required = %v, want [status reason]", required)
	}
}

func TestCallVMExecCapture(t *testing.T) {
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
			store := output.NewStore(f, "vm", "/tmp/mph-output/run1", cfg.Output, tt.cutoff)
			got, err := agent.Call(context.Background(), nil, cfg, store, "vm_exec", tt.args)
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			env, ok := parseToolEnvelope(got)
			if (ok && env.Data.Result.Captured) != tt.wantCapture {
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
			store := output.NewStore(f, "vm", "/tmp/mph-output/run1", cfg.Output, 10)
			id := capturedID(t, captureEnvelope(t, cfg, store))
			res, err := agent.Call(context.Background(), nil, cfg, store, "output_search", map[string]any{
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
	cfg := config.Config{
		VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:  "m",
		Prompt: "p",
		Output: config.OutputConfig{Enabled: true, Mode: "always", MaxCommandSize: 1 << 30, MaxTotalSize: 1 << 26, SearchMaxMatches: 200},
	}
	f := &fakeOutputExec{fn: func(name, cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "df -P"):
			return "99999999", nil
		case strings.Contains(cmd, "mkdir"):
			return "", nil
		case strings.Contains(cmd, "__MPH_EXIT_"):
			return exitMarker(cmd) + "0", nil
		case strings.Contains(cmd, "wc -c"):
			return "7340032", nil
		case strings.Contains(cmd, "wc -l"):
			return "64000", nil
		default:
			return "", nil
		}
	}}
	store := output.NewStore(f, "vm", "/tmp/mph-output/run1", cfg.Output, 10)
	got := captureEnvelope(t, cfg, store)
	for _, want := range []string{`"captured":true`, `"total_lines":64000`, `"total_bytes":7340032`} {
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
	got, err := agent.Call(context.Background(), nil, cfg, store, "vm_exec", map[string]any{"command": "echo hi"})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if env, ok := parseToolEnvelope(got); !ok || !env.Data.Result.Captured {
		t.Fatalf("want capture envelope, got %q", got)
	}
	return got
}

func capturedID(t *testing.T, envelope string) string {
	t.Helper()
	env, ok := parseToolEnvelope(envelope)
	if !ok {
		t.Fatalf("want tool envelope, got %q", envelope)
	}
	return env.Data.Result.OutputID
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
			if _, err := agent.Call(context.Background(), nil, config.Config{}, nil, tt.tool, tt.args); err == nil {
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
			return exitMarker(cmd) + "0", nil
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
	store := output.NewStore(f, "vm", "/tmp/mph-output/run1", cfg.Output, 10)
	capGot, err := agent.Call(context.Background(), nil, cfg, store, "vm_exec", map[string]any{"command": "echo hi"})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	id := capturedID(t, capGot)
	if id == "" {
		t.Fatalf("no output_id in %q", capGot)
	}
	got, err := agent.Call(context.Background(), nil, cfg, store, "output_read", map[string]any{
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

func TestCallUnknownTool(t *testing.T) {
	if _, err := agent.Call(context.Background(), nil, config.Config{}, nil, "bogus", nil); err == nil {
		t.Fatal("want error for an unknown tool")
	}
}

func TestCallExecError(t *testing.T) {
	cfg := config.Config{VM: config.VMConfig{Name: "vm"}, AllowedCommands: map[string]bool{"echo": true}}
	cli := newScriptedTestClient(t, func(command string) (string, int) {
		switch {
		case strings.HasPrefix(command, "id -u"), strings.HasPrefix(command, "id -g"):
			return "1000\n", 0
		case strings.HasPrefix(command, "getent passwd"):
			return "ubuntu:x:1000:1000:Ubuntu:/home/ubuntu:/bin/bash\n", 0
		default:
			return "boom", 1
		}
	})
	if _, err := agent.Call(context.Background(), cli, cfg, nil, "vm_exec", map[string]any{"command": "echo"}); err == nil {
		t.Fatal("want error when the VM command exits non-zero")
	}
}

func TestCallExecEmptyCommand(t *testing.T) {
	_, err := agent.Call(context.Background(), nil, config.Config{}, nil, "vm_exec", map[string]any{"command": "   "})
	if err == nil {
		t.Fatal("want error for empty command")
	}
}

func TestCallExecDisallowedCommand(t *testing.T) {
	cfg := config.Config{AllowedCommands: map[string]bool{"echo": true}}
	_, err := agent.Call(context.Background(), nil, cfg, nil, "vm_exec", map[string]any{"command": "rm -rf /"})
	if err == nil {
		t.Fatal("want validation error for a disallowed command")
	}
}

func TestCallOutputSearchStoreError(t *testing.T) {
	cfg := config.Config{Output: config.OutputConfig{Enabled: true, Mode: "auto", SearchMaxMatches: 200}}
	f := &fakeOutputExec{fn: func(string, string) (string, error) { return "", nil }}
	store := output.NewStore(f, "vm", "/tmp/mph-output/run1", cfg.Output, 10)
	_, err := agent.Call(context.Background(), nil, cfg, store, "output_search", map[string]any{
		"output_id": "does-not-exist", "pattern": "x",
	})
	if err == nil {
		t.Fatal("want lookup error for an unknown output_id")
	}
}

func TestCallExecCaptureError(t *testing.T) {
	cfg := config.Config{
		VM:     config.VMConfig{Name: "vm"},
		Output: config.OutputConfig{Enabled: true, Mode: "auto", MaxCommandSize: 1 << 20, MaxTotalSize: 1 << 26, SearchMaxMatches: 200},
	}
	f := &fakeOutputExec{fn: func(name, cmd string) (string, error) {
		if strings.Contains(cmd, "df -P") {
			return "0", nil
		}
		return "", nil
	}}
	store := output.NewStore(f, "vm", "/tmp/mph-output/run1", cfg.Output, 10)
	_, err := agent.Call(context.Background(), nil, cfg, store, "vm_exec", map[string]any{"command": "echo hi", "capture": true})
	if err == nil {
		t.Fatal("want preflight error when there is no free space for the capture")
	}
}

func TestCallExecNoStoreEmptyOutput(t *testing.T) {
	cfg := config.Config{VM: config.VMConfig{Name: "vm"}, AllowedCommands: map[string]bool{"echo": true}}
	cli := newScriptedTestClient(t, func(command string) (string, int) {
		switch {
		case strings.HasPrefix(command, "id -u"), strings.HasPrefix(command, "id -g"):
			return "1000\n", 0
		case strings.HasPrefix(command, "getent passwd"):
			return "ubuntu:x:1000:1000:Ubuntu:/home/ubuntu:/bin/bash\n", 0
		default:
			return "", 0
		}
	})
	got, err := agent.Call(context.Background(), cli, cfg, nil, "vm_exec", map[string]any{"command": "echo"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if got != output.NoOutput {
		t.Fatalf("got %q, want %q", got, output.NoOutput)
	}
}

// callExec forwards Result.Inline with no empty-string fallback, so the capture
// path must substitute output.NoOutput itself. output.TestCaptureNeverReturnsEmptyInline
// pins the other half of this contract.
func TestCallExecCaptureEmptyOutput(t *testing.T) {
	cfg := config.Config{
		VM:              config.VMConfig{Name: "vm"},
		AllowedCommands: map[string]bool{"echo": true},
		Output:          config.OutputConfig{Enabled: true, Mode: "auto", MaxCommandSize: 1 << 20, MaxTotalSize: 1 << 26, SearchMaxMatches: 200},
	}
	f := &fakeOutputExec{fn: func(name, cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "df -P"):
			return "99999999", nil
		case strings.Contains(cmd, "__MPH_EXIT_"):
			return exitMarker(cmd) + "0", nil
		case strings.Contains(cmd, "wc -c"):
			return "0", nil
		default:
			return "", nil
		}
	}}
	store := output.NewStore(f, "vm", "/tmp/mph-output/run1", cfg.Output, 4096)

	got, err := agent.Call(context.Background(), nil, cfg, store, "vm_exec", map[string]any{"command": "echo", "capture": true})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if got != output.NoOutput {
		t.Fatalf("got %q, want %q", got, output.NoOutput)
	}
}

func TestCallOutputReadCorrectsOffsetBelowOne(t *testing.T) {
	cfg := config.Config{
		VM:     config.VMConfig{Name: "vm"},
		Output: config.OutputConfig{Enabled: true, Mode: "always", MaxCommandSize: 1 << 20, MaxTotalSize: 1 << 26, SearchMaxMatches: 200},
	}
	f := &fakeOutputExec{fn: func(name, cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "df -P"):
			return "99999999", nil
		case strings.Contains(cmd, "mkdir"):
			return "", nil
		case strings.Contains(cmd, "__MPH_EXIT_"):
			return exitMarker(cmd) + "0", nil
		case strings.Contains(cmd, "wc -c"):
			return "1000", nil
		case strings.Contains(cmd, "wc -l"):
			return "50", nil
		case strings.Contains(cmd, "sed -n"):
			return "line1\nline2\n", nil
		default:
			return "", nil
		}
	}}
	store := output.NewStore(f, "vm", "/tmp/mph-output/run1", cfg.Output, 10)
	id := capturedID(t, captureEnvelope(t, cfg, store))
	got, err := agent.Call(context.Background(), nil, cfg, store, "output_read", map[string]any{"output_id": id, "offset": json.Number("0")})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(got, `"offset":1`) {
		t.Fatalf("offset below 1 must be corrected to 1, got %q", got)
	}
}

func TestCallOutputReadPastEndReturnsEmptyLines(t *testing.T) {
	cfg := config.Config{
		VM:     config.VMConfig{Name: "vm"},
		Output: config.OutputConfig{Enabled: true, Mode: "always", MaxCommandSize: 1 << 20, MaxTotalSize: 1 << 26, SearchMaxMatches: 200},
	}
	f := &fakeOutputExec{fn: func(name, cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "df -P"):
			return "99999999", nil
		case strings.Contains(cmd, "mkdir"):
			return "", nil
		case strings.Contains(cmd, "__MPH_EXIT_"):
			return exitMarker(cmd) + "0", nil
		case strings.Contains(cmd, "wc -c"):
			return "1000", nil
		case strings.Contains(cmd, "wc -l"):
			return "50", nil
		default:
			return "", nil
		}
	}}
	store := output.NewStore(f, "vm", "/tmp/mph-output/run1", cfg.Output, 10)
	id := capturedID(t, captureEnvelope(t, cfg, store))
	got, err := agent.Call(context.Background(), nil, cfg, store, "output_read", map[string]any{"output_id": id, "offset": json.Number("1000")})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(got, `"lines":[]`) {
		t.Fatalf("a read past the end must serialize an empty lines array, got %q", got)
	}
}

func TestCallOutputReadStoreError(t *testing.T) {
	cfg := config.Config{Output: config.OutputConfig{Enabled: true, Mode: "auto", SearchMaxMatches: 200}}
	f := &fakeOutputExec{fn: func(string, string) (string, error) { return "", nil }}
	store := output.NewStore(f, "vm", "/tmp/mph-output/run1", cfg.Output, 10)
	_, err := agent.Call(context.Background(), nil, cfg, store, "output_read", map[string]any{"output_id": "does-not-exist"})
	if err == nil {
		t.Fatal("want lookup error for an unknown output_id")
	}
}
