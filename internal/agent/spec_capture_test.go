package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/trollLemon/MPHarness/v2/internal/agent"
	"github.com/trollLemon/MPHarness/v2/internal/config"
	"github.com/trollLemon/MPHarness/v2/internal/output"
)

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
		{"capture flag removed from vm_exec", "vm_exec", "capture", true, false},
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
			if found == nil {
				if tt.wantPresent {
					t.Fatalf("missing %s", tt.tool)
				}
				return
			}
			props, _ := found.Parameters["properties"].(map[string]any)
			_, hasProp := props[tt.prop]
			if hasProp != tt.wantPresent {
				t.Fatalf("%s property %q present = %v, want %v: %v", tt.tool, tt.prop, hasProp, tt.wantPresent, found.Parameters)
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
		mode        string
		cutoff      int64
		vmOutput    string
		wantCapture bool
		wantSubs    []string
	}{
		{
			name:        "auto mode small output below cutoff returns inline",
			mode:        "auto",
			cutoff:      100,
			vmOutput:    "hello\n",
			wantCapture: false,
			wantSubs:    []string{"hello\n"},
		},
		{
			name:        "auto mode output over cutoff automatically returns handle",
			mode:        "auto",
			cutoff:      5,
			vmOutput:    "line1\nline2\nline3\n",
			wantCapture: true,
			wantSubs:    []string{`"captured":true`, `"total_bytes":18`},
		},
		{
			name:        "always mode captures even small output",
			mode:        "always",
			cutoff:      1000,
			vmOutput:    "hi\n",
			wantCapture: true,
			wantSubs:    []string{`"captured":true`, `"total_bytes":3`},
		},
		{
			name:        "empty output returns NoOutput sentinel",
			mode:        "auto",
			cutoff:      100,
			vmOutput:    "",
			wantCapture: false,
			wantSubs:    []string{output.NoOutput},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Config{
				VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
				Model:  "m",
				Prompt: "p",
				Output: config.OutputConfig{
					Enabled: true, Mode: tt.mode,
					MaxCommandSize: 1 << 20, MaxTotalSize: 1 << 26, SearchMaxMatches: 200,
				},
			}
			cli := newScriptedTestClient(t, func(command string) (string, int) {
				if command == "echo test" {
					return tt.vmOutput, 0
				}
				return identityScripted(command)
			})
			store := output.NewStore(cfg.Output, tt.cutoff)
			got, err := agent.Call(context.Background(), cli, cfg, store, "vm_exec", map[string]any{"command": "echo test"})
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

func TestCallOutputSearchAndReadRoundTrip(t *testing.T) {
	tests := []struct {
		name      string
		tool      string
		args      func(id string) map[string]any
		wantSubs  []string
		wantNotIn []string
		wantErr   bool
	}{
		{
			name: "output_search returns matching lines",
			tool: "output_search",
			args: func(id string) map[string]any {
				return map[string]any{"output_id": id, "pattern": `patch state:\s+applied`}
			},
			wantSubs:  []string{`"total_matches":1`, `"matches":["patch state:  applied"]`},
			wantNotIn: []string{`"chunk"`, `"chunks_matched"`},
		},
		{
			name: "output_search with no match serializes empty matches array",
			tool: "output_search",
			args: func(id string) map[string]any {
				return map[string]any{"output_id": id, "pattern": "absent"}
			},
			wantSubs:  []string{`"matches":[]`, `"total_matches":0`},
			wantNotIn: []string{"null"},
		},
		{
			name: "output_search rejects unknown output_id",
			tool: "output_search",
			args: func(string) map[string]any {
				return map[string]any{"output_id": "does-not-exist", "pattern": "x"}
			},
			wantErr: true,
		},
		{
			name: "output_read returns requested slice",
			tool: "output_read",
			args: func(id string) map[string]any {
				return map[string]any{"output_id": id, "offset": json.Number("0"), "limit": json.Number("6")}
			},
			wantSubs: []string{`"offset":0`, `"length":6`, `"content":"kernel"`},
		},
		{
			name: "output_read rejects unknown output_id",
			tool: "output_read",
			args: func(string) map[string]any {
				return map[string]any{"output_id": "does-not-exist"}
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Config{
				VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
				Model:  "m",
				Prompt: "p",
				Output: config.OutputConfig{
					Enabled: true, Mode: "always",
					MaxCommandSize: 5, MaxTotalSize: 1 << 26, SearchMaxMatches: 200,
				},
			}
			cli := newScriptedTestClient(t, func(command string) (string, int) {
				if command == "status" {
					return "kernel: 5.15.0-25-generic\npatch state:  applied\nversion: 105.1\n", 0
				}
				return identityScripted(command)
			})
			store := output.NewStore(cfg.Output, 0)
			capGot, err := agent.Call(context.Background(), cli, cfg, store, "vm_exec", map[string]any{"command": "status"})
			if err != nil {
				t.Fatalf("capture: %v", err)
			}
			env, ok := parseToolEnvelope(capGot)
			if !ok || env.Data.Result.OutputID == "" {
				t.Fatalf("expected capture envelope, got %q", capGot)
			}

			got, err := agent.Call(context.Background(), cli, cfg, store, tt.tool, tt.args(env.Data.Result.OutputID))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %s, got %q", tt.tool, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: %v", tt.tool, err)
			}
			for _, want := range tt.wantSubs {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in %q", want, got)
				}
			}
			for _, absent := range tt.wantNotIn {
				if strings.Contains(got, absent) {
					t.Errorf("must not contain %q in %q", absent, got)
				}
			}
		})
	}
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
