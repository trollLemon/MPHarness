package config

import (
	"reflect"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.vm.Validate()
			if err != tt.wantErr {
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
		{"valid", Config{VM: VMConfig{Disk: "20G", RAM: "4G", CPU: 2}, Model: "m", Prompt: "p"}, nil},
		{"vm invalid", Config{VM: VMConfig{Disk: "20G", RAM: "4G", CPU: 0}, Model: "m", Prompt: "p"}, ErrVMCPURequired},
		{"missing model", Config{VM: VMConfig{Disk: "20G", RAM: "4G", CPU: 2}, Prompt: "p"}, ErrModelRequired},
		{"blank model", Config{VM: VMConfig{Disk: "20G", RAM: "4G", CPU: 2}, Model: "  ", Prompt: "p"}, ErrModelRequired},
		{"missing prompt", Config{VM: VMConfig{Disk: "20G", RAM: "4G", CPU: 2}, Model: "m"}, ErrPromptRequired},
		{"blank prompt", Config{VM: VMConfig{Disk: "20G", RAM: "4G", CPU: 2}, Model: "m", Prompt: " \t"}, ErrPromptRequired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if err != tt.wantErr {
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
			got := normalizeAllowedCommands(tt.in)
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
			got := normalizeAllowedCommands(tt.in)
			for _, cu := range coreUtils {
				if !got[cu] {
					t.Fatalf("missing coreutils command %q in %v", cu, got)
				}
			}
			if got["coreUtils"] {
				t.Fatalf("meta command coreUtils was not expanded: %v", got)
			}
			if len(got) < len(coreUtils) {
				t.Fatalf("got %d commands, want at least %d", len(got), len(coreUtils))
			}
		})
	}
}

func TestUnmarshalYAML_AllowedCommands(t *testing.T) {
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
			var c Config
			if err := yaml.Unmarshal([]byte(tt.yaml), &c); err != nil {
				t.Fatalf("unmarshal: %v", err)
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
		{"bare bytes", base + "output:\n  max_command_size: 1024\n", 1024, false},
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
				t.Errorf("got %d want %d", int64(cfg.Output.MaxCommandSize), tt.want)
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
				if int64(cfg.Output.MaxCommandSize) != DefaultOutputMaxCommandBytes {
					t.Errorf("max_command_size %d", int64(cfg.Output.MaxCommandSize))
				}
				if int64(cfg.Output.MaxTotalSize) != DefaultOutputMaxTotalBytes {
					t.Errorf("max_total_size %d", int64(cfg.Output.MaxTotalSize))
				}
				if cfg.Output.SearchMaxMatches != 200 {
					t.Errorf("search_max_matches %d", cfg.Output.SearchMaxMatches)
				}
				if !cfg.Compaction.Enabled || cfg.Compaction.Threshold != 0.8 || cfg.Compaction.MaxSummaryTokens != 2048 {
					t.Errorf("compaction defaults: %+v", cfg.Compaction)
				}
				if cfg.Compaction.RecentTurns != 3 || cfg.Compaction.MinMessages != 6 || cfg.Compaction.MaxAttempts != 3 {
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
			name: "explicit recent_turns zero is kept",
			yaml: base + "compaction:\n  recent_turns: 0\n",
			check: func(t *testing.T, cfg Config) {
				if cfg.Compaction.RecentTurns != 0 {
					t.Errorf("explicit recent_turns: 0 must be kept, got %d", cfg.Compaction.RecentTurns)
				}
			},
		},
		{
			name: "explicit zero size disables cap",
			yaml: base + "output:\n  max_command_size: 0\n  max_total_size: 0\n",
			check: func(t *testing.T, cfg Config) {
				if int64(cfg.Output.MaxCommandSize) != 0 || int64(cfg.Output.MaxTotalSize) != 0 {
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

// The size caps treat an explicit 0 as "disabled" and only default a key the
// user left out. search_max_matches must behave the same way, otherwise a
// deliberate 0 is silently rewritten to 200 and the cap cannot be lifted.
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
