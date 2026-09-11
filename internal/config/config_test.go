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
			// ensure sorted
			if len(got) > 1 && !sort.StringsAreSorted(got) {
				t.Fatalf("not sorted: %v", got)
			}
		})
	}
}

func TestIsCommandAllowed(t *testing.T) {
	tests := []struct {
		name    string
		allowed map[string]bool
		cmd     string
		want    bool
	}{
		{"allow all when empty", nil, "rm -rf /", true},
		{"allow all when empty map", map[string]bool{}, "any", true},
		{"exact match", map[string]bool{"ls": true}, "ls", true},
		{"base match with args", map[string]bool{"ls": true}, "ls -la /tmp", true},
		{"base match with tab", map[string]bool{"ls": true}, "ls\t-la", true},
		{"not allowed", map[string]bool{"ls": true}, "rm -rf", false},
		{"trimmed input", map[string]bool{"ls": true}, "  ls  ", true},
		{"empty cmd", map[string]bool{"ls": true}, "  ", false},
		{"subcommand allowed via base", map[string]bool{"git": true}, "git status", true},
		{"different binary not allowed", map[string]bool{"apt": true}, "apt-get update", false},
		{"case sensitive", map[string]bool{"LS": true}, "ls", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Config{AllowedCommands: tt.allowed}
			if got := c.IsCommandAllowed(tt.cmd); got != tt.want {
				t.Fatalf("IsCommandAllowed(%q)=%v want %v (set %v)", tt.cmd, got, tt.want, tt.allowed)
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
		name      string
		yaml      string
		wantName  string
		wantSet   map[string]bool
		wantErr   bool
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
