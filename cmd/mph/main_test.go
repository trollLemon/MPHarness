package main

import (
	"os"
	"strings"
	"testing"
	"uuid"

	"github.com/trollLemon/MPHarness/internal/config"
)

func TestResolveOtelConfig(t *testing.T) {
	tests := []struct {
		name         string
		yaml         config.OtelConfig
		flagEnabled  bool
		envEndpoint  string
		envService   string
		envMPHOtel   string
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
			name:         "yaml is used when no env is set",
			yaml:         config.OtelConfig{Enabled: true, Endpoint: "yamlcol:4317", ServiceName: "yamlsvc"},
			wantEnabled:  true,
			wantEndpoint: "yamlcol:4317",
			wantService:  "yamlsvc",
		},
		{
			name:         "env endpoint overrides yaml",
			yaml:         config.OtelConfig{Endpoint: "yamlcol:4317"},
			envEndpoint:  "envcol:4317",
			wantEndpoint: "envcol:4317",
			wantService:  "mph",
		},
		{
			name:         "env service name overrides yaml",
			yaml:         config.OtelConfig{ServiceName: "yamlsvc"},
			envService:   "envsvc",
			wantEndpoint: "localhost:4317",
			wantService:  "envsvc",
		},
		{
			name:         "blank env endpoint is ignored",
			yaml:         config.OtelConfig{Endpoint: "yamlcol:4317"},
			envEndpoint:  "   ",
			wantEndpoint: "yamlcol:4317",
			wantService:  "mph",
		},
		{
			name:         "flag enables otel",
			flagEnabled:  true,
			wantEnabled:  true,
			wantEndpoint: "localhost:4317",
			wantService:  "mph",
		},
		{
			name:         "MPH_OTEL env enables otel",
			envMPHOtel:   "true",
			wantEnabled:  true,
			wantEndpoint: "localhost:4317",
			wantService:  "mph",
		},
		{
			name:         "MPH_OTEL false does not enable otel",
			envMPHOtel:   "false",
			wantEnabled:  false,
			wantEndpoint: "localhost:4317",
			wantService:  "mph",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", tt.envEndpoint)
			t.Setenv("OTEL_SERVICE_NAME", tt.envService)
			t.Setenv("MPH_OTEL", tt.envMPHOtel)

			got := resolveOtelConfig(tt.yaml, tt.flagEnabled, "0198f0c1-2a3b-7c4d-8e5f-60718293a4b5", "baseline")
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
		yaml string
		env  string
		vm   string
		want string
	}{
		{"yaml wins", "from-yaml", "from-env", "mph-vm", "from-yaml"},
		{"env over vm", "", "from-env", "mph-vm", "from-env"},
		{"vm over fallback", "", "", "mph-longrun", "mph-longrun"},
		{"fallback last", "", "", "", "mph"},
		{"blank yaml falls through", "   ", "", "mph-vm", "mph-vm"},
		{"blank env falls through", "", "\t", "mph-vm", "mph-vm"},
		{"blank vm falls through", "", "", "  ", "mph"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("MPH_RUN_NAME", tt.env)
			if got := resolveRunName(tt.yaml, os.Getenv("MPH_RUN_NAME"), tt.vm); got != tt.want {
				t.Errorf("resolveRunName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewRunIdentity(t *testing.T) {
	t.Setenv("MPH_RUN_NAME", "")

	got := newRunIdentity(config.RunConfig{Name: "baseline"}, "mph-vm")

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
	t.Setenv("MPH_RUN_NAME", "")

	got := newRunIdentity(config.RunConfig{Name: "baseline"}, "mph-vm")

	if strings.Contains(got.ID, ":") {
		t.Errorf("ID = %q, must not contain a separator: it becomes a filesystem path", got.ID)
	}
}

// TestNewSecretsResolver covers the fail-fast path: a prompt naming a secret the
// configured source cannot supply must be rejected before the VM is created, not
// part-way through a run.
func TestNewSecretsResolver(t *testing.T) {
	tests := []struct {
		name      string
		prompt    string
		source    string
		envSecret string
		wantErr   bool
	}{
		{
			name:   "prompt without a placeholder",
			prompt: "check the disk usage",
			source: "env",
		},
		{
			name:      "placeholder resolvable from the environment",
			prompt:    "run sudo pro attach %{PRO_TOKEN}",
			source:    "env",
			envSecret: "ghp_abc123",
		},
		{
			name:    "placeholder not present in the environment",
			prompt:  "run sudo pro attach %{PRO_TOKEN}",
			source:  "env",
			wantErr: true,
		},
		{
			name:    "unknown source",
			prompt:  "check the disk usage",
			source:  "vault",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envSecret != "" {
				t.Setenv("PRO_TOKEN", tt.envSecret)
			}

			cfg := config.Config{
				Prompt:  tt.prompt,
				Secrets: config.SecretsConfig{Source: tt.source},
			}

			r, err := newSecretsResolver(cfg)

			if tt.wantErr {
				if err == nil {
					t.Fatal("newSecretsResolver succeeded, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if r == nil {
				t.Fatal("newSecretsResolver returned a nil resolver with no error")
			}
		})
	}
}
