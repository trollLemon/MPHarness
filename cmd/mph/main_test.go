package main

import (
	"testing"

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

			got := resolveOtelConfig(tt.yaml, tt.flagEnabled)
			if got.Enabled != tt.wantEnabled {
				t.Errorf("Enabled = %v, want %v", got.Enabled, tt.wantEnabled)
			}
			if got.Endpoint != tt.wantEndpoint {
				t.Errorf("Endpoint = %q, want %q", got.Endpoint, tt.wantEndpoint)
			}
			if got.ServiceName != tt.wantService {
				t.Errorf("ServiceName = %q, want %q", got.ServiceName, tt.wantService)
			}
		})
	}
}
