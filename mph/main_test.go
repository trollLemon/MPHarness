package main_test

import (
	"strings"
	"testing"
	"uuid"

	"github.com/trollLemon/MPHarness/internal/agent"
	"github.com/trollLemon/MPHarness/internal/config"

	main "github.com/trollLemon/MPHarness/mph"
)

func TestExitCode(t *testing.T) {
	tests := []struct {
		name    string
		outcome *agent.TaskStatusOutcome
		want    int
	}{
		{"completed", &agent.TaskStatusOutcome{CompletedStatus: "Completed", Reason: "checks passed"}, 0},
		{"failed", &agent.TaskStatusOutcome{CompletedStatus: "Failed", Reason: "outcome not reached"}, 1},
		{"error", &agent.TaskStatusOutcome{CompletedStatus: "Error", Reason: "permission denied"}, 2},
		{"missing outcome", nil, 1},
		{"unknown status", &agent.TaskStatusOutcome{CompletedStatus: "Unknown", Reason: "unexpected"}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := main.ExitCode(tt.outcome); got != tt.want {
				t.Errorf("main.ExitCode(%+v) = %d, want %d", tt.outcome, got, tt.want)
			}
		})
	}
}

func TestResolveOtelConfig(t *testing.T) {
	tests := []struct {
		name         string
		otel         config.OtelConfig
		wantEnabled  bool
		wantEndpoint string
		wantService  string
	}{
		{
			name:         "falls back to defaults",
			wantEnabled:  false,
			wantEndpoint: "localhost:4317",
			wantService:  "mph",
		},
		{
			name:         "config values",
			otel:         config.OtelConfig{Enabled: true, Endpoint: "yamlcol:4317", ServiceName: "yamlsvc"},
			wantEnabled:  true,
			wantEndpoint: "yamlcol:4317",
			wantService:  "yamlsvc",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := main.ResolveOtelConfig(tt.otel, "0198f0c1-2a3b-7c4d-8e5f-60718293a4b5", "baseline")
			if got.Enabled != tt.wantEnabled {
				t.Errorf("Enabled = %v, want %v", got.Enabled, tt.wantEnabled)
			}
			if got.Endpoint != tt.wantEndpoint {
				t.Errorf("Endpoint = %q, want %q", got.Endpoint, tt.wantEndpoint)
			}
			if got.ServiceName != tt.wantService {
				t.Errorf("ServiceName = %q, want %q", got.ServiceName, tt.wantService)
			}
			if got.RunID != "0198f0c1-2a3b-7c4d-8e5f-60718293a4b5" {
				t.Errorf("RunID = %q, want test run id", got.RunID)
			}
			if got.RunName != "baseline" {
				t.Errorf("RunName = %q, want %q", got.RunName, "baseline")
			}
		})
	}
}

func TestResolveRunName(t *testing.T) {
	tests := []struct {
		name string
		run  string
		vm   string
		want string
	}{
		{"run name wins", "from-yaml", "mph-vm", "from-yaml"},
		{"vm fallback", "", "mph-longrun", "mph-longrun"},
		{"fallback last", "", "", "mph"},
		{"blank run name falls through", "   ", "mph-vm", "mph-vm"},
		{"blank vm falls through", "", "  ", "mph"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := main.ResolveRunName(tt.run, tt.vm); got != tt.want {
				t.Errorf("main.ResolveRunName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewRunIdentity(t *testing.T) {
	got := main.NewRunIdentity(config.RunConfig{Name: "baseline"}, "mph-vm")

	if got.Name != "baseline" {
		t.Errorf("Name = %q, want %q", got.Name, "baseline")
	}
	if got.Label != "baseline:"+got.ID {
		t.Errorf("Label = %q, want %q", got.Label, "baseline:"+got.ID)
	}
	if _, err := uuid.Parse(got.ID); err != nil {
		t.Errorf("ID = %q, want a parseable uuid: %v", got.ID, err)
	}
}

func TestRunIdentityIDIsBareUUID(t *testing.T) {
	got := main.NewRunIdentity(config.RunConfig{Name: "baseline"}, "mph-vm")

	if strings.Contains(got.ID, ":") {
		t.Errorf("ID = %q, must not contain a separator: it becomes a filesystem path", got.ID)
	}
}
