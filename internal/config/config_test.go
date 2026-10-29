package config_test

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	. "github.com/trollLemon/MPHarness/internal/config"
)

func TestVMConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		vm      VMConfig
		wantErr error
	}{
		{"valid", VMConfig{Name: "vm", Disk: "20G", RAM: "4G", CPU: 2}, nil},
		{"missing disk", VMConfig{Name: "vm", RAM: "4G", CPU: 2}, ErrVMDiskRequired},
		{"missing ram", VMConfig{Name: "vm", Disk: "20G", CPU: 2}, ErrVMRAMRequired},
		{"zero cpu", VMConfig{Name: "vm", Disk: "20G", RAM: "4G", CPU: 0}, ErrVMCPURequired},
		{"negative cpu", VMConfig{Name: "vm", Disk: "20G", RAM: "4G", CPU: -1}, ErrVMCPURequired},
		{"underscore name", VMConfig{Name: "my_vm", Disk: "20G", RAM: "4G", CPU: 2}, ErrVMNameInvalid},
		{"numeric name", VMConfig{Name: "1234", Disk: "20G", RAM: "4G", CPU: 2}, ErrVMNameInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.vm.Validate()
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("got %v want %v", err, tt.wantErr)
			}
		})
	}
}

func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr error
	}{
		{"valid", Config{VM: VMConfig{Name: "vm", Disk: "20G", RAM: "4G", CPU: 2}, Model: "m", Prompt: "p"}, nil},
		{"vm invalid", Config{VM: VMConfig{Name: "vm", Disk: "20G", RAM: "4G", CPU: 0}, Model: "m", Prompt: "p"}, ErrVMCPURequired},
		{"vm name with underscore", Config{VM: VMConfig{Name: "my_vm", Disk: "20G", RAM: "4G", CPU: 2}, Model: "m", Prompt: "p"}, ErrVMNameInvalid},
		{"missing model", Config{VM: VMConfig{Name: "vm", Disk: "20G", RAM: "4G", CPU: 2}, Prompt: "p"}, ErrModelRequired},
		{"blank model", Config{VM: VMConfig{Name: "vm", Disk: "20G", RAM: "4G", CPU: 2}, Model: "  ", Prompt: "p"}, ErrModelRequired},
		{"missing prompt", Config{VM: VMConfig{Name: "vm", Disk: "20G", RAM: "4G", CPU: 2}, Model: "m"}, ErrPromptRequired},
		{"blank prompt", Config{VM: VMConfig{Name: "vm", Disk: "20G", RAM: "4G", CPU: 2}, Model: "m", Prompt: " \t"}, ErrPromptRequired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("got %v want %v", err, tt.wantErr)
			}
		})
	}
}

func TestAllowedCommandsList(t *testing.T) {
	tests := []struct {
		name string
		set  map[string]bool
		want []string
	}{
		{"nil", nil, nil},
		{"empty", map[string]bool{}, nil},
		{"sorted", map[string]bool{"z": true, "a": true, "m": true}, []string{"a", "m", "z"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Config{AllowedCommands: tt.set}
			got := c.AllowedCommandsList()
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v want %v", got, tt.want)
			}
			if len(got) > 1 && !sort.StringsAreSorted(got) {
				t.Fatalf("not sorted: %v", got)
			}
		})
	}
}

func TestNormalizeAllowedCommands(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want map[string]bool
	}{
		{"nil", nil, nil},
		{"empty", []string{}, nil},
		{"blanks only", []string{"", "  ", "\t"}, nil},
		{"trim and dedup", []string{" ls ", "ls", "cat", " ls "}, map[string]bool{"ls": true, "cat": true}},
		{"preserve distinct", []string{"apt", "apt-get"}, map[string]bool{"apt": true, "apt-get": true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeAllowedCommands(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}

func TestNormalizeAllowedCommandsCoreUtils(t *testing.T) {
	tests := []struct {
		name string
		in   []string
	}{
		{"meta expands to full set", []string{"coreUtils"}},
		{"meta mixes with regular commands", []string{"coreUtils", "apt", "apt-get"}},
		{"meta dedups against explicit entries", []string{"coreUtils", "ls"}},
		{"meta trimmed", []string{"  coreUtils  "}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeAllowedCommands(tt.in)
			for _, cu := range CoreUtils {
				if !got[cu] {
					t.Fatalf("missing coreutils command %q in %v", cu, got)
				}
			}
			if got["coreUtils"] {
				t.Fatalf("meta command coreUtils was not expanded: %v", got)
			}
			if len(got) < len(CoreUtils) {
				t.Fatalf("got %d commands, want at least %d", len(got), len(CoreUtils))
			}
		})
	}
}

func TestParseAllowedCommands(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want map[string]bool
	}{
		{
			name: "snake_case",
			yaml: "vm:\n  disk: 20G\n  ram: 4G\n  cpu: 2\nmodel: m\nprompt: p\nallowed_commands:\n  - ls\n  - cat\n",
			want: map[string]bool{"ls": true, "cat": true},
		},
		{
			name: "dedup and trim",
			yaml: "vm:\n  disk: 20G\n  ram: 4G\n  cpu: 2\nmodel: m\nprompt: p\nallowed_commands:\n  - \" ls \"\n  - ls\n  - \"\"\n",
			want: map[string]bool{"ls": true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Parse([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if !reflect.DeepEqual(c.AllowedCommands, tt.want) {
				t.Fatalf("got %v want %v", c.AllowedCommands, tt.want)
			}
		})
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		name     string
		yaml     string
		wantName string
		wantSet  map[string]bool
		wantErr  bool
	}{
		{
			name:     "valid with defaults",
			yaml:     "vm:\n  disk: 20G\n  ram: 4G\n  cpu: 2\nmodel: m\nprompt: p\n",
			wantName: "mph-vm",
			wantSet:  nil,
		},
		{
			name:     "custom name preserved",
			yaml:     "vm:\n  name: myvm\n  disk: 20G\n  ram: 4G\n  cpu: 2\nmodel: m\nprompt: p\n",
			wantName: "myvm",
		},
		{
			name:    "missing disk",
			yaml:    "vm:\n  ram: 4G\n  cpu: 2\nmodel: m\nprompt: p\n",
			wantErr: true,
		},
		{
			name:    "underscore vm name",
			yaml:    "vm:\n  name: my_vm\n  disk: 20G\n  ram: 4G\n  cpu: 2\nmodel: m\nprompt: p\n",
			wantErr: true,
		},
		{
			name:    "missing model",
			yaml:    "vm:\n  disk: 20G\n  ram: 4G\n  cpu: 2\nprompt: p\n",
			wantErr: true,
		},
		{
			name:     "allowed dedup sort via Parse",
			yaml:     "vm:\n  disk: 20G\n  ram: 4G\n  cpu: 2\nmodel: m\nprompt: p\nallowed_commands:\n  - z\n  - a\n  - z\n",
			wantName: "mph-vm",
			wantSet:  map[string]bool{"a": true, "z": true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tt.yaml))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if cfg.VM.Name != tt.wantName {
				t.Fatalf("name %q want %q", cfg.VM.Name, tt.wantName)
			}
			if !reflect.DeepEqual(cfg.AllowedCommands, tt.wantSet) {
				t.Fatalf("set %v want %v", cfg.AllowedCommands, tt.wantSet)
			}
		})
	}
}

func TestParseEnvBool(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    bool
		wantErr bool
	}{
		{"true", "true", true, false},
		{"uppercase true", "TRUE", true, false},
		{"false", "false", false, false},
		{"padded false", " false ", false, false},
		{"one rejected", "1", false, true},
		{"zero rejected", "0", false, true},
		{"yes rejected", "yes", false, true},
		{"no rejected", "no", false, true},
		{"off rejected", "off", false, true},
		{"blank rejected", "", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseEnvBool(tt.value)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tt.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tt.want {
				t.Errorf("ParseEnvBool(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	base := "vm:\n  disk: 20G\n  ram: 4G\n  cpu: 2\nmodel: m\nprompt: p\nrun:\n  name: yaml-run\notel:\n  enabled: %t\n  endpoint: yaml:4317\n  service_name: yamlsvc\n"
	tests := []struct {
		name         string
		yamlEnabled  bool
		environ      []string
		wantEnabled  bool
		wantEndpoint string
		wantService  string
		wantRun      string
		wantErr      bool
	}{
		{
			name:         "yaml only",
			wantEndpoint: "yaml:4317",
			wantService:  "yamlsvc",
			wantRun:      "yaml-run",
		},
		{
			name:         "env overrides yaml",
			environ:      []string{"MPH_OTEL=true", "OTEL_EXPORTER_OTLP_ENDPOINT=env:4317", "OTEL_SERVICE_NAME=envsvc", "MPH_RUN_NAME=env-run"},
			wantEnabled:  true,
			wantEndpoint: "env:4317",
			wantService:  "envsvc",
			wantRun:      "env-run",
		},
		{
			name:         "env false overrides yaml true",
			yamlEnabled:  true,
			environ:      []string{"MPH_OTEL=false"},
			wantEnabled:  false,
			wantEndpoint: "yaml:4317",
			wantService:  "yamlsvc",
			wantRun:      "yaml-run",
		},
		{
			name:         "uppercase true enables",
			environ:      []string{"MPH_OTEL=TRUE"},
			wantEnabled:  true,
			wantEndpoint: "yaml:4317",
			wantService:  "yamlsvc",
			wantRun:      "yaml-run",
		},
		{
			name:    "invalid MPH_OTEL errors",
			environ: []string{"MPH_OTEL=yes"},
			wantErr: true,
		},
		{
			name:    "blank MPH_OTEL errors",
			environ: []string{"MPH_OTEL="},
			wantErr: true,
		},
		{
			name:         "blank env ignored",
			yamlEnabled:  true,
			environ:      []string{"OTEL_EXPORTER_OTLP_ENDPOINT=   ", "OTEL_SERVICE_NAME=", "MPH_RUN_NAME=  "},
			wantEnabled:  true,
			wantEndpoint: "yaml:4317",
			wantService:  "yamlsvc",
			wantRun:      "yaml-run",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load([]byte(fmt.Sprintf(base, tt.yamlEnabled)), Sources{Environ: tt.environ})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error")
				}
				if !strings.Contains(err.Error(), "MPH_OTEL") {
					t.Fatalf("error should mention MPH_OTEL: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.Otel.Enabled != tt.wantEnabled {
				t.Errorf("enabled = %v, want %v", cfg.Otel.Enabled, tt.wantEnabled)
			}
			if cfg.Otel.Endpoint != tt.wantEndpoint {
				t.Errorf("endpoint = %q, want %q", cfg.Otel.Endpoint, tt.wantEndpoint)
			}
			if cfg.Otel.ServiceName != tt.wantService {
				t.Errorf("service = %q, want %q", cfg.Otel.ServiceName, tt.wantService)
			}
			if cfg.Run.Name != tt.wantRun {
				t.Errorf("run name = %q, want %q", cfg.Run.Name, tt.wantRun)
			}
		})
	}
}

func TestLoadFlagOverrides(t *testing.T) {
	base := "vm:\n  disk: 20G\n  ram: 4G\n  cpu: 2\nmodel: m\nprompt: p\notel:\n  enabled: %t\n"
	tests := []struct {
		name        string
		yamlEnabled bool
		environ     []string
		flag        bool
		set         bool
		want        bool
	}{
		{
			name:        "flag disables yaml",
			yamlEnabled: true,
			flag:        false,
			set:         true,
			want:        false,
		},
		{
			name: "flag enables yaml",
			flag: true,
			set:  true,
			want: true,
		},
		{
			name:    "flag overrides env",
			environ: []string{"MPH_OTEL=true"},
			flag:    false,
			set:     true,
			want:    false,
		},
		{
			name:        "unset flag leaves yaml",
			yamlEnabled: true,
			want:        true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.Bool("otel", false, "")
			if tt.set {
				if err := fs.Set("otel", fmt.Sprintf("%t", tt.flag)); err != nil {
					t.Fatalf("set flag: %v", err)
				}
			}
			cfg, err := Load([]byte(fmt.Sprintf(base, tt.yamlEnabled)), Sources{Environ: tt.environ, Flags: fs})
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.Otel.Enabled != tt.want {
				t.Errorf("enabled = %v, want %v", cfg.Otel.Enabled, tt.want)
			}
		})
	}
}

func TestLoadFileWithSources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mph.yaml")
	cfgYAML := "vm:\n  disk: 20G\n  ram: 4G\n  cpu: 2\nmodel: m\nprompt: p\nrun:\n  name: yaml-run\n"
	if err := os.WriteFile(path, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadFileWithSources(path, Sources{Environ: []string{"MPH_RUN_NAME=env-run"}})
	if err != nil {
		t.Fatalf("LoadFileWithSources: %v", err)
	}
	if cfg.Run.Name != "env-run" {
		t.Errorf("run name = %q, want %q", cfg.Run.Name, "env-run")
	}
}

func TestParseDurationValues(t *testing.T) {
	base := "vm:\n  disk: 20G\n  ram: 4G\n  cpu: 2\nmodel: m\nprompt: p\nagent:\n  chat_timeout: "
	tests := []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{"duration string", "90s", 90 * time.Second, false},
		{"numeric seconds", "90", 90 * time.Second, false},
		{"float seconds", "90.0", 90 * time.Second, false},
		{"fractional seconds truncate", "1.5", time.Second, false},
		{"compound string", "1m30s", 90 * time.Second, false},
		{"out of range", "9223372037", 0, true},
		{"invalid duration", "soon", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte(base + tt.value + "\n"))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := time.Duration(cfg.Agent.ChatTimeout); got != tt.want {
				t.Errorf("chat_timeout = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseLegacyScalarCompatibility(t *testing.T) {
	base := "vm:\n  disk: 20G\n  ram: 4G\n  cpu: 2\nmodel: m\nprompt: p\n"
	tests := []struct {
		name  string
		yaml  string
		check func(t *testing.T, cfg Config)
	}{
		{
			name: "numeric string field",
			yaml: base + "run:\n  name: 789\n",
			check: func(t *testing.T, cfg Config) {
				if cfg.Run.Name != "789" {
					t.Errorf("run name = %q, want %q", cfg.Run.Name, "789")
				}
			},
		},
		{
			name: "numeric resource attribute",
			yaml: base + "otel:\n  resource_attributes:\n    build: 42\n",
			check: func(t *testing.T, cfg Config) {
				if got := cfg.Otel.ResourceAttributes["build"]; got != "42" {
					t.Errorf("build = %q, want %q", got, "42")
				}
			},
		},
		{
			name: "yaml 1.1 booleans",
			yaml: base + "agent:\n  send_reasoning: no\notel:\n  enabled: yes\n",
			check: func(t *testing.T, cfg Config) {
				if !cfg.Otel.Enabled {
					t.Error("otel.enabled = false, want true")
				}
				if cfg.Agent.SendReasoning == nil || *cfg.Agent.SendReasoning {
					t.Error("send_reasoning = true, want false")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			tt.check(t, cfg)
		})
	}
}

func TestParseOtelDefaults(t *testing.T) {
	tests := []struct {
		name        string
		yaml        string
		wantEnabled bool
		wantEP      string
		wantService string
		wantAttrs   map[string]string
	}{
		{
			name:        "otel absent stays empty for later resolution",
			yaml:        "vm:\n  disk: 20G\n  ram: 4G\n  cpu: 2\nmodel: m\nprompt: p\n",
			wantEnabled: false,
		},
		{
			name:        "otel enabled true preserves",
			yaml:        "vm:\n  disk: 20G\n  ram: 4G\n  cpu: 2\nmodel: m\nprompt: p\notel:\n  enabled: true\n  endpoint: mycol:4317\n  service_name: custom\n",
			wantEnabled: true,
			wantEP:      "mycol:4317",
			wantService: "custom",
		},
		{
			name:        "otel enabled with no endpoint stays empty",
			yaml:        "vm:\n  disk: 20G\n  ram: 4G\n  cpu: 2\nmodel: m\nprompt: p\notel:\n  enabled: true\n",
			wantEnabled: true,
		},
		{
			name:        "otel resource_attributes",
			yaml:        "vm:\n  disk: 20G\n  ram: 4G\n  cpu: 2\nmodel: m\nprompt: p\notel:\n  resource_attributes:\n    environment: prod\n    region: eu\n",
			wantEnabled: false,
			wantAttrs:   map[string]string{"environment": "prod", "region": "eu"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if cfg.Otel.Enabled != tt.wantEnabled {
				t.Fatalf("enabled %v want %v", cfg.Otel.Enabled, tt.wantEnabled)
			}
			if cfg.Otel.Endpoint != tt.wantEP {
				t.Fatalf("endpoint %q want %q", cfg.Otel.Endpoint, tt.wantEP)
			}
			if cfg.Otel.ServiceName != tt.wantService {
				t.Fatalf("service %q want %q", cfg.Otel.ServiceName, tt.wantService)
			}
			if !reflect.DeepEqual(cfg.Otel.ResourceAttributes, tt.wantAttrs) {
				t.Fatalf("attrs %v want %v", cfg.Otel.ResourceAttributes, tt.wantAttrs)
			}
		})
	}
}

func TestParseTruncation(t *testing.T) {
	base := "vm:\n  disk: 20G\n  ram: 4G\n  cpu: 2\nmodel: m\nprompt: p\n"

	tests := []struct {
		name string
		yaml string
		want TruncationConfig
	}{
		{
			name: "absent block uses defaults",
			yaml: base,
			want: TruncationConfig{
				CommandOutput: DefaultCommandOutputBytes,
				LogContent:    DefaultLogContentBytes,
				ToolResult:    DefaultToolResultBytes,
				Nudge:         DefaultNudgeBytes,
			},
		},
		{
			name: "explicit values win",
			yaml: base + "truncation:\n  command_output: 1\n  log_content: 2\n  tool_result: 3\n  nudge: 4\n",
			want: TruncationConfig{CommandOutput: 1, LogContent: 2, ToolResult: 3, Nudge: 4},
		},
		{
			name: "partial block keeps defaults for the rest",
			yaml: base + "truncation:\n  log_content: 99\n",
			want: TruncationConfig{
				CommandOutput: DefaultCommandOutputBytes,
				LogContent:    99,
				ToolResult:    DefaultToolResultBytes,
				Nudge:         DefaultNudgeBytes,
			},
		},
		{
			name: "negative falls back to default",
			yaml: base + "truncation:\n  tool_result: -1\n",
			want: TruncationConfig{
				CommandOutput: DefaultCommandOutputBytes,
				LogContent:    DefaultLogContentBytes,
				ToolResult:    DefaultToolResultBytes,
				Nudge:         DefaultNudgeBytes,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if cfg.Truncation != tt.want {
				t.Errorf("Truncation = %+v, want %+v", cfg.Truncation, tt.want)
			}
		})
	}
}

func TestParseAgentOutputBudget(t *testing.T) {
	base := "vm:\n  disk: 20G\n  ram: 4G\n  cpu: 2\nmodel: m\nprompt: p\n"

	tests := []struct {
		name           string
		yaml           string
		wantMaxOut     int
		wantMaxTokens  int
		wantSendReason bool
	}{
		{
			name:           "unset budget stays zero for the agent to derive",
			yaml:           base,
			wantMaxOut:     0,
			wantMaxTokens:  DefaultLLMMaxOutputTokens,
			wantSendReason: true,
		},
		{
			name:           "explicit budget is preserved",
			yaml:           base + "agent:\n  max_output_bytes: 1234\n",
			wantMaxOut:     1234,
			wantMaxTokens:  DefaultLLMMaxOutputTokens,
			wantSendReason: true,
		},
		{
			name:           "reasoning can be disabled",
			yaml:           base + "agent:\n  send_reasoning: false\n",
			wantMaxOut:     0,
			wantMaxTokens:  DefaultLLMMaxOutputTokens,
			wantSendReason: false,
		},
		{
			name:           "max output tokens honoured",
			yaml:           base + "llm:\n  max_output_tokens: 512\n",
			wantMaxOut:     0,
			wantMaxTokens:  512,
			wantSendReason: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if cfg.Agent.MaxOutputBytes != tt.wantMaxOut {
				t.Errorf("MaxOutputBytes = %d, want %d", cfg.Agent.MaxOutputBytes, tt.wantMaxOut)
			}
			if cfg.LLM.MaxOutputTokens != tt.wantMaxTokens {
				t.Errorf("MaxOutputTokens = %d, want %d", cfg.LLM.MaxOutputTokens, tt.wantMaxTokens)
			}
			if got := cfg.Agent.Reasoning(); got != tt.wantSendReason {
				t.Errorf("SendReasoning = %v, want %v", got, tt.wantSendReason)
			}
		})
	}
}

func TestSizeParsing(t *testing.T) {
	base := "vm:\n  disk: 20G\n  ram: 4G\n  cpu: 2\nmodel: m\nprompt: p\n"
	tests := []struct {
		name    string
		yaml    string
		want    int64
		wantErr bool
	}{
		{"mebibytes", base + "output:\n  max_command_size: 64MiB\n", 64 * 1024 * 1024, false},
		{"larger mebibytes", base + "output:\n  max_command_size: 512MiB\n", 512 * 1024 * 1024, false},
		{"gibibytes", base + "output:\n  max_command_size: 1GiB\n", 1024 * 1024 * 1024, false},
		{"quoted bytes", base + "output:\n  max_command_size: \"1024\"\n", 1024, false},
		{"empty string", base + "output:\n  max_command_size: \"\"\n", 0, false},
		{"unquoted bytes", base + "output:\n  max_command_size: 1024\n", 1024, false},
		{"unquoted zero", base + "output:\n  max_command_size: 0\n", 0, false},
		{"negative bytes rejected", base + "output:\n  max_command_size: -1\n", 0, true},
		{"unquoted overflow", base + "output:\n  max_command_size: 9223372036854775808\n", 0, true},
		{"kilobytes are binary", base + "output:\n  max_command_size: 1024K\n", 1024 * 1024, false},
		{"kilobyte suffix is binary", base + "output:\n  max_command_size: 1KB\n", 1024, false},
		{"terabytes", base + "output:\n  max_command_size: 1T\n", 1 << 40, false},
		{"fractional with unit", base + "output:\n  max_command_size: 1.5K\n", 1536, false},
		{"fractional string bytes", base + "output:\n  max_command_size: \"1.5\"\n", 0, true},
		{"unquoted fractional rejected", base + "output:\n  max_command_size: 1.5\n", 0, true},
		{"overflow", base + "output:\n  max_command_size: \"99999999999999999999T\"\n", 0, true},
		{"float overflow", base + "output:\n  max_command_size: \"9223372036854775808.0\"\n", 0, true},
		{"invalid suffix", base + "output:\n  max_command_size: 10XB\n", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tt.yaml))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for invalid size")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if int64(cfg.Output.MaxCommandSize) != tt.want {
				t.Errorf("got %d want %d", cfg.Output.MaxCommandSize, tt.want)
			}
		})
	}
}

func TestParseOutputCompaction(t *testing.T) {
	base := "vm:\n  disk: 20G\n  ram: 4G\n  cpu: 2\nmodel: m\nprompt: p\n"
	tests := []struct {
		name  string
		yaml  string
		check func(t *testing.T, cfg Config)
	}{
		{
			name: "absent blocks take defaults",
			yaml: base,
			check: func(t *testing.T, cfg Config) {
				if !cfg.Output.Enabled || cfg.Output.Mode != "auto" {
					t.Errorf("output defaults: %+v", cfg.Output)
				}
				if cfg.Output.MaxCommandSize != DefaultOutputMaxCommandBytes {
					t.Errorf("max_command_size %d", cfg.Output.MaxCommandSize)
				}
				if cfg.Output.MaxTotalSize != DefaultOutputMaxTotalBytes {
					t.Errorf("max_total_size %d", cfg.Output.MaxTotalSize)
				}
				if cfg.Output.SearchMaxMatches != 200 {
					t.Errorf("search_max_matches %d", cfg.Output.SearchMaxMatches)
				}
				if !cfg.Compaction.Enabled || cfg.Compaction.Threshold != 0.8 || cfg.Compaction.MaxSummaryTokens != 2048 {
					t.Errorf("compaction defaults: %+v", cfg.Compaction)
				}
				if cfg.Compaction.MaxAttempts != 3 {
					t.Errorf("compaction defaults: %+v", cfg.Compaction)
				}
			},
		},
		{
			name: "output can be disabled",
			yaml: base + "output:\n  enabled: false\n",
			check: func(t *testing.T, cfg Config) {
				if cfg.Output.Enabled {
					t.Errorf("expected output disabled")
				}
			},
		},
		{
			name: "block without enabled opts in",
			yaml: base + "output:\n  mode: always\ncompaction:\n  threshold: 0.7\n",
			check: func(t *testing.T, cfg Config) {
				if !cfg.Output.Enabled || cfg.Output.Mode != "always" {
					t.Errorf("output: %+v", cfg.Output)
				}
				if !cfg.Compaction.Enabled || cfg.Compaction.Threshold != 0.7 {
					t.Errorf("compaction: %+v", cfg.Compaction)
				}
			},
		},
		{
			name: "explicit zero size disables cap",
			yaml: base + "output:\n  max_command_size: \"0\"\n  max_total_size: \"0\"\n",
			check: func(t *testing.T, cfg Config) {
				if cfg.Output.MaxCommandSize != 0 || cfg.Output.MaxTotalSize != 0 {
					t.Errorf("explicit 0 must stay disabled, got %+v", cfg.Output)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			tt.check(t, cfg)
		})
	}
}

func TestOutputValidation(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{"bogus output mode", "vm:\n  disk: 20G\n  ram: 4G\n  cpu: 2\nmodel: m\nprompt: p\noutput:\n  enabled: true\n  mode: bogus\n"},
		{"compaction threshold too low", "vm:\n  disk: 20G\n  ram: 4G\n  cpu: 2\nmodel: m\nprompt: p\ncompaction:\n  enabled: true\n  threshold: 0.4\n"},
		{"compaction threshold too high", "vm:\n  disk: 20G\n  ram: 4G\n  cpu: 2\nmodel: m\nprompt: p\ncompaction:\n  enabled: true\n  threshold: 0.99\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse([]byte(tt.yaml)); err == nil {
				t.Errorf("expected validation error for %q", tt.yaml)
			}
		})
	}
}

func TestSearchMaxMatchesZeroMeansUnlimited(t *testing.T) {
	base := "vm:\n  name: vm\n  cpu: 1\n  ram: 1G\n  disk: 5G\nmodel: m\nprompt: p\noutput:\n  enabled: true\n"
	tests := []struct {
		name string
		yaml string
		want int
	}{
		{"omitted falls back to the default", base, DefaultOutputSearchMatches},
		{"explicit zero is honoured", base + "  search_max_matches: 0\n", 0},
		{"explicit value is honoured", base + "  search_max_matches: 7\n", 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if cfg.Output.SearchMaxMatches != tt.want {
				t.Fatalf("search_max_matches %d want %d", cfg.Output.SearchMaxMatches, tt.want)
			}
		})
	}
}

func TestOutputRunDir(t *testing.T) {
	tests := []struct {
		name    string
		runID   string
		wantDir string
	}{
		{"simple id", "run-123", "/tmp/mph-output/run-123"},
		{"uuid id", "01a0ec35-3962-7a15-be5e-a9372f3401b7", "/tmp/mph-output/01a0ec35-3962-7a15-be5e-a9372f3401b7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if dir := OutputRunDir(tt.runID); dir != tt.wantDir {
				t.Fatalf("got %q want %q", dir, tt.wantDir)
			}
		})
	}
}

func TestParseDecodesEveryKey(t *testing.T) {
	cfg, err := Parse([]byte(`
vm:
  name: my-vm
  disk: 20G
  ram: 4G
  cpu: 2
  image: noble
model: org/model
prompt: do things
allowed_commands:
  - apt
  - git
llm:
  temperature: 0.2
  top_p: 0.9
  top_k: 40
  context_window: 8192
  max_output_tokens: 512
agent:
  max_iterations: 7
  chat_timeout: 90s
  total_timeout: 20m
  max_output_bytes: 4096
  send_reasoning: false
truncation:
  command_output: 1
  log_content: 2
  tool_result: 3
  nudge: 4
otel:
  enabled: true
  endpoint: host:4317
  service_name: svc
output:
  enabled: true
  mode: always
  search_max_matches: 7
compaction:
  enabled: true
  threshold: 0.7
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"vm.name", cfg.VM.Name, "my-vm"},
		{"vm.disk", cfg.VM.Disk, "20G"},
		{"vm.ram", cfg.VM.RAM, "4G"},
		{"vm.cpu", cfg.VM.CPU, 2},
		{"vm.image", cfg.VM.Image, "noble"},
		{"model", cfg.Model, "org/model"},
		{"prompt", cfg.Prompt, "do things"},
		{"content_dir", cfg.ContentDir, ""},
		{"llm.temperature", cfg.LLM.Temperature, 0.2},
		{"llm.top_p", cfg.LLM.TopP, 0.9},
		{"llm.top_k", cfg.LLM.TopK, 40},
		{"llm.context_window", cfg.LLM.ContextWindow, 8192},
		{"llm.max_output_tokens", cfg.LLM.MaxOutputTokens, 512},
		{"agent.max_iterations", cfg.Agent.MaxIterations, 7},
		{"agent.chat_timeout", time.Duration(cfg.Agent.ChatTimeout), 90 * time.Second},
		{"agent.total_timeout", time.Duration(cfg.Agent.TotalTimeout), 20 * time.Minute},
		{"agent.max_output_bytes", cfg.Agent.MaxOutputBytes, 4096},
		{"truncation.command_output", cfg.Truncation.CommandOutput, 1},
		{"truncation.log_content", cfg.Truncation.LogContent, 2},
		{"truncation.tool_result", cfg.Truncation.ToolResult, 3},
		{"truncation.nudge", cfg.Truncation.Nudge, 4},
		{"otel.enabled", cfg.Otel.Enabled, true},
		{"otel.endpoint", cfg.Otel.Endpoint, "host:4317"},
		{"otel.service_name", cfg.Otel.ServiceName, "svc"},
		{"output.enabled", cfg.Output.Enabled, true},
		{"output.mode", cfg.Output.Mode, "always"},
		{"output.search_max_matches", cfg.Output.SearchMaxMatches, 7},
		{"compaction.enabled", cfg.Compaction.Enabled, true},
		{"compaction.threshold", cfg.Compaction.Threshold, 0.7},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v want %v", c.name, c.got, c.want)
		}
	}
	if got := cfg.Agent.Reasoning(); got {
		t.Error("send_reasoning: false must be honoured")
	}
	if got := len(cfg.AllowedCommandsList()); got != 2 {
		t.Errorf("allowed_commands has %d entries want 2", got)
	}
}

func TestSecretsConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		sc      SecretsConfig
		wantErr error
	}{
		{"unset source", SecretsConfig{}, nil},
		{"env", SecretsConfig{Source: "env"}, nil},
		{"keyring", SecretsConfig{Source: "keyring"}, nil},
		{"unknown source", SecretsConfig{Source: "vault"}, ErrInvalidSecretsSource},
		{"source is case sensitive", SecretsConfig{Source: "ENV"}, ErrInvalidSecretsSource},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.sc.Validate()
			if err != tt.wantErr {
				t.Fatalf("got %v want %v", err, tt.wantErr)
			}
		})
	}
}

// TestParseSecrets checks that the block round-trips and that a bad source is
// rejected at load time, not at the first placeholder. Defaulting is deliberately
// not here: secrets.New owns it, so there is one home for the default values and
// no import cycle.
func TestParseSecrets(t *testing.T) {
	const base = "vm:\n  disk: 20G\n  ram: 4G\n  cpu: 2\nmodel: m\nprompt: p\n"
	tests := []struct {
		name      string
		yaml      string
		wantSrc   string
		wantSvc   string
		wantErrIs error
	}{
		{"absent block leaves fields unset", base, "", "", nil},
		{"source only", base + "secrets:\n  source: keyring\n", "keyring", "", nil},
		{"source and service", base + "secrets:\n  source: keyring\n  service: corp\n", "keyring", "corp", nil},
		{"env source", base + "secrets:\n  source: env\n", "env", "", nil},
		{"unknown source is rejected at load", base + "secrets:\n  source: vault\n", "", "", ErrInvalidSecretsSource},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tt.yaml))

			if tt.wantErrIs != nil {
				if !errors.Is(err, tt.wantErrIs) {
					t.Fatalf("got %v want %v", err, tt.wantErrIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.Secrets.Source != tt.wantSrc {
				t.Errorf("secrets.source = %q want %q", cfg.Secrets.Source, tt.wantSrc)
			}
			if cfg.Secrets.Service != tt.wantSvc {
				t.Errorf("secrets.service = %q want %q", cfg.Secrets.Service, tt.wantSvc)
			}
		})
	}
}

func TestIsSafeValue(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{"token", "token123", true},
		{"github pat", "ghp_abc123", true},
		{"jwt dots", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.signature", true},
		{"base64", "aGVsbG8rL3dvcmxkPQ==", true},
		{"uuid", "01a0ec35-3962-7a15-be5e-a9372f3401b7", true},
		{"arn colons", "arn:aws:iam::123456789012:user/name", true},
		{"empty", "", false},
		{"space", "not safe", false},
		{"command substitution", "$(id -u)", false},
		{"backtick", "`id`", false},
		{"semicolon", "a;b", false},
		{"pipe", "a|b", false},
		{"tilde", "~/.token", false},
		{"quote", "a'b", false},
		{"backslash", `a\b`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSafeValue(tt.value); got != tt.want {
				t.Fatalf("IsSafeValue(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestImageRemote(t *testing.T) {
	tests := []struct {
		name      string
		image     string
		wantURL   string
		wantAlias string
		wantErr   bool
	}{
		{"empty defaults to ubuntu 24.04", "", "https://cloud-images.ubuntu.com/releases/", "24.04", false},
		{"bare alias noble", "noble", "https://cloud-images.ubuntu.com/releases/", "noble", false},
		{"bare alias version", "22.04", "https://cloud-images.ubuntu.com/releases/", "22.04", false},
		{"bare alias resolute", "resolute", "https://cloud-images.ubuntu.com/releases/", "resolute", false},
		{"release prefix", "release:24.04", "https://cloud-images.ubuntu.com/releases/", "24.04", false},
		{"daily prefix", "daily:24.04", "https://cloud-images.ubuntu.com/daily/", "24.04", false},
		{"ubuntu prefix", "ubuntu:24.04", "https://cloud-images.ubuntu.com/releases/", "24.04", false},
		{"ubuntu prefix trims alias whitespace", "ubuntu: noble ", "https://cloud-images.ubuntu.com/releases/", "noble", false},
		{"ubuntu-daily prefix", "ubuntu-daily:24.04", "https://cloud-images.ubuntu.com/daily/", "24.04", false},
		{"images prefix", "images:alpine/3.20", "https://images.lxd.canonical.com", "alpine/3.20", false},
		{"unknown remote", "fedora:40", "", "", true},
		{"prefixed but empty alias", "ubuntu:", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url, alias, err := ImageRemote(tt.image)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ImageRemote(%q) succeeded, want error", tt.image)
				}
				for _, p := range []string{"release:", "daily:", "ubuntu:", "ubuntu-daily:", "images:"} {
					if !strings.Contains(err.Error(), p) {
						t.Fatalf("error %q does not name supported prefix %s", err, p)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("ImageRemote(%q): %v", tt.image, err)
			}
			if url != tt.wantURL || alias != tt.wantAlias {
				t.Fatalf("ImageRemote(%q) = %q, %q; want %q, %q", tt.image, url, alias, tt.wantURL, tt.wantAlias)
			}
		})
	}
}

func TestNormalizeSize(t *testing.T) {
	tests := []struct {
		name    string
		size    string
		want    string
		wantErr bool
	}{
		{"decimal to binary", "20G", "20GiB", false},
		{"decimal to smaller unit", "4096M", "4GiB", false},
		{"megabytes stay binary", "512M", "512MiB", false},
		{"kilobytes become mebibytes", "1024K", "1MiB", false},
		{"terabytes", "1T", "1TiB", false},
		{"bare bytes", "1073741824", "1GiB", false},
		{"binary already", "1GiB", "1GiB", false},
		{"gb accepted", "20GB", "20GiB", false},
		{"fractional", "1.5G", "1536MiB", false},
		{"empty", "", "", true},
		{"no unit garbage", "abc", "", true},
		{"unknown unit", "20X", "", true},
		{"negative", "-5G", "", true},
		{"zero", "0", "", true},
		{"fractional bare bytes", "0.5", "", true},
		{"fractional bare bytes above one", "1.5", "", true},
		{"overflow", "99999999999999999999T", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeSize(tt.size)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NormalizeSize(%q) = %q, want error", tt.size, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeSize(%q): %v", tt.size, err)
			}
			if got != tt.want {
				t.Fatalf("NormalizeSize(%q) = %q, want %q", tt.size, got, tt.want)
			}
		})
	}
}

// Property-based tests for config using rapid.

// normalizeAllowedCommands returns nil for empty or whitespace-only input.
func TestNormalizeAllowedCommandsEmptyReturnsNil(t *testing.T) {
	tests := []struct {
		name string
		in   []string
	}{
		{"nil", nil},
		{"empty", []string{}},
		{"whitespace entries", []string{"", " ", "\t", "  \n  "}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeAllowedCommands(tt.in); got != nil {
				t.Fatalf("NormalizeAllowedCommands(%v) = %v, want nil", tt.in, got)
			}
		})
	}
}

// normalizeAllowedCommands trims whitespace from entries.
func TestNormalizeAllowedCommandsTrimsWhitespace(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		cmd := rapid.StringN(1, 20, 50).Draw(t, "cmd")
		// Skip commands with whitespace
		if strings.TrimSpace(cmd) != cmd {
			return
		}
		paddedCmd := "   " + cmd + "   "
		got := NormalizeAllowedCommands([]string{paddedCmd})
		if got == nil || !got[cmd] {
			t.Fatalf("NormalizeAllowedCommands([%q]) = %v, want %q present", paddedCmd, got, cmd)
		}
	})
}

// normalizeAllowedCommands deduplicates entries.
func TestNormalizeAllowedCommandsDedupes(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		cmd := rapid.StringN(1, 20, 50).Draw(t, "cmd")
		if strings.TrimSpace(cmd) != cmd {
			return
		}
		got := NormalizeAllowedCommands([]string{cmd, cmd, cmd, cmd})
		if len(got) != 1 {
			t.Fatalf("NormalizeAllowedCommands(dupes) = %v, want len 1", got)
		}
		if !got[cmd] {
			t.Fatalf("NormalizeAllowedCommands(dupes) missing %q", cmd)
		}
	})
}

// normalizeAllowedCommands is idempotent.
func TestNormalizeAllowedCommandsIdempotent(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		cmds := rapid.SliceOf(rapid.StringN(0, 20, 50)).Draw(t, "cmds")
		first := NormalizeAllowedCommands(cmds)
		// Convert map to slice for second call
		var asSlice []string
		for k := range first {
			asSlice = append(asSlice, k)
		}
		second := NormalizeAllowedCommands(asSlice)
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("idempotency failed: first=%v, second=%v", first, second)
		}
	})
}

// NormalizeAllowedCommands expands coreUtils.
func TestNormalizeAllowedCommandsExpandsCoreUtils(t *testing.T) {
	got := NormalizeAllowedCommands([]string{"coreUtils"})
	if got == nil {
		t.Fatalf("NormalizeAllowedCommands([coreUtils]) = nil")
	}
	for _, cu := range CoreUtils {
		if !got[cu] {
			t.Fatalf("missing coreutils command %q", cu)
		}
	}
	if got["coreUtils"] {
		t.Fatalf("meta command coreUtils was not expanded")
	}
}

// VMConfig validation: valid configs always pass.
func TestVMConfigValidConfigsPass(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate valid VM configs with random values
		disk := rapid.OneOf(rapid.Just("20G"), rapid.Just("50G"), rapid.Just("100G"), rapid.Just("1T")).Draw(t, "disk")
		ram := rapid.OneOf(rapid.Just("4G"), rapid.Just("8G"), rapid.Just("16G"), rapid.Just("32G")).Draw(t, "ram")
		cpu := rapid.IntRange(1, 64).Draw(t, "cpu")
		// Generate valid hostname (alphanumeric + hyphens, starting with letter)
		prefix := rapid.StringMatching(`[a-z][a-z0-9-]{0,10}`).Draw(t, "prefix")
		suffix := rapid.IntRange(0, 999).Draw(t, "suffix")
		name := fmt.Sprintf("%s-%d", prefix, suffix)

		cfg := VMConfig{Disk: disk, RAM: ram, CPU: cpu, Name: name}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("valid config should pass: %v", err)
		}
	})
}

// VMConfig validation: missing fields produce correct errors.
func TestVMConfigMissingFields(t *testing.T) {
	tests := []struct {
		name    string
		cfg     VMConfig
		wantErr error
	}{
		{"missing disk", VMConfig{RAM: "4G", CPU: 2, Name: "valid-vm"}, ErrVMDiskRequired},
		{"missing ram", VMConfig{Disk: "20G", CPU: 2, Name: "valid-vm"}, ErrVMRAMRequired},
		{"zero cpu", VMConfig{Disk: "20G", RAM: "4G", Name: "valid-vm"}, ErrVMCPURequired},
		{"negative cpu", VMConfig{Disk: "20G", RAM: "4G", CPU: -1, Name: "valid-vm"}, ErrVMCPURequired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.Validate(); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// IsSafeValue rejects empty strings.
func TestIsSafeValueRejectsEmpty(t *testing.T) {
	if IsSafeValue("") {
		t.Fatal("IsSafeValue(\"\") should be false")
	}
}

// IsSafeValue accepts valid alphanumeric strings.
func TestIsSafeValueAcceptsValid(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate strings from the allowed character set
		valid := rapid.StringMatching(`[A-Za-z0-9._+/=:@-]+`).Draw(t, "valid")
		if valid == "" {
			return
		}
		if !IsSafeValue(valid) {
			t.Fatalf("IsSafeValue(%q) should be true", valid)
		}
	})
}

// IsSafeValue rejects strings with disallowed characters.
func TestIsSafeValueRejectsInvalid(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate strings that contain at least one disallowed char
		invalid := rapid.StringMatching(`.*[\s$\|`+"`"+`\"'~;<>()\[\]{}\\].*`).Draw(t, "invalid")
		if invalid == "" {
			return
		}
		if IsSafeValue(invalid) {
			t.Fatalf("IsSafeValue(%q) should be false", invalid)
		}
	})
}

// Config.Validate requires model and prompt.
func TestConfigRequiresModelAndPrompt(t *testing.T) {
	validVM := VMConfig{Disk: "20G", RAM: "4G", CPU: 2, Name: "vm"}
	cfg := Config{VM: validVM}
	if err := cfg.Validate(); !errors.Is(err, ErrModelRequired) {
		t.Fatalf("missing model: got %v, want %v", err, ErrModelRequired)
	}
	cfg.Model = "model"
	if err := cfg.Validate(); !errors.Is(err, ErrPromptRequired) {
		t.Fatalf("missing prompt: got %v, want %v", err, ErrPromptRequired)
	}
}

// AllowedCommandsList returns sorted results.
func TestAllowedCommandsListSorted(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate a set of command names
		cmds := rapid.SliceOf(rapid.StringN(1, 20, 50)).Draw(t, "cmds")
		set := make(map[string]bool)
		for _, c := range cmds {
			if strings.TrimSpace(c) != "" {
				set[c] = true
			}
		}
		cfg := Config{AllowedCommands: set}
		got := cfg.AllowedCommandsList()
		if len(got) > 1 && !sort.StringsAreSorted(got) {
			t.Fatalf("AllowedCommandsList not sorted: %v", got)
		}
	})
}
