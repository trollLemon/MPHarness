package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var (
	ErrVMDiskRequired = errors.New("vm.disk is required (e.g. \"20G\")")
	ErrVMRAMRequired  = errors.New("vm.ram is required (e.g. \"4G\")")
	ErrVMCPURequired  = errors.New("vm.cpu must be > 0")
	ErrModelRequired  = errors.New("model is required (LLM, e.g. \"unsloth/Qwen3-0.6B-Q8_0\")")
	ErrPromptRequired = errors.New("prompt is required")
)

// VMContentDir is the fixed directory inside the VM where host content_dir is copied.
// The harness creates this path and transfers the local directory there before the agent starts.
const VMContentDir = "/home/ubuntu/content"

// Agent defaults. Named so the help text in cmd/mph cannot drift from the values
// applyDefaults actually installs.
const (
	DefaultMaxIterations = 10
	DefaultChatTimeout   = 300 * time.Second
	DefaultTotalTimeout  = 30 * time.Minute

	// DefaultLLMMaxOutputTokens caps one model turn so a runaway completion
	// cannot decode until it exhausts the context window.
	DefaultLLMMaxOutputTokens = 2048
)

// Truncation defaults, in bytes. Each caps a different kind of payload on its way
// into a log line or span attribute, where an unbounded value would blow up the
// collector or bury the signal.
const (
	DefaultCommandOutputBytes = 600
	DefaultLogContentBytes    = 8000
	DefaultToolResultBytes    = 4000
	DefaultNudgeBytes         = 2000
)

// coreUtils is an array of the core utils commands, for when the user config specifies the meta command `coreUtils`.
var coreUtils = [...]string{
	// File utilities
	"chgrp", "chmod", "chown", "cp", "dd", "df", "dir", "dircolors", "du",
	"install", "ln", "ls", "mkdir", "mkfifo", "mknod", "mktemp", "mv",
	"rm", "rmdir", "shred", "sync", "touch", "vdir",

	// Text utilities
	"base32", "base64", "cat", "cksum", "comm", "csplit", "cut", "expand",
	"fmt", "fold", "head", "join", "md5sum", "nl", "od", "paste", "ptx",
	"pr", "sha1sum", "sha224sum", "sha256sum", "sha384sum", "sha512sum",
	"shuf", "sort", "split", "sum", "tac", "tail", "tr", "tsort",
	"unexpand", "uniq", "wc",

	// Shell & System utilities
	"[", "arch", "basename", "chcon", "date", "dirname", "echo", "env",
	"expr", "factor", "false", "groups", "hostid", "id", "link", "logname",
	"nice", "nohup", "nproc", "numfmt", "pathchk", "pinky", "printenv",
	"printf", "pwd", "readlink", "realpath", "runcon", "seq", "sleep",
	"stat", "stty", "tee", "test", "timeout", "true", "tty", "uname",
	"unlink", "uptime", "users", "who", "whoami", "yes",
}

type VMConfig struct {
	Disk  string `yaml:"disk"`
	RAM   string `yaml:"ram"`
	CPU   int    `yaml:"cpu"`
	Name  string `yaml:"name"`
	Image string `yaml:"image"`
}

func (v VMConfig) Validate() error {
	if v.Disk == "" {
		return ErrVMDiskRequired
	}
	if v.RAM == "" {
		return ErrVMRAMRequired
	}
	if v.CPU <= 0 {
		return ErrVMCPURequired
	}
	return nil
}

type LLMConfig struct {
	Temperature     float64 `yaml:"temperature"`
	TopP            float64 `yaml:"top_p"`
	TopK            int     `yaml:"top_k"`
	ToolChoice      string  `yaml:"tool_choice"`
	ContextWindow   int     `yaml:"context_window"`
	MaxOutputTokens int     `yaml:"max_output_tokens"`
}

type AgentConfig struct {
	MaxIterations int      `yaml:"max_iterations"`
	ChatTimeout   Duration `yaml:"chat_timeout"`
	TotalTimeout  Duration `yaml:"total_timeout"`
	// MaxOutputBytes caps one tool result handed to the model. Zero means the
	// agent derives a cap from the context window the model actually loaded,
	// which the harness auto-tunes to the host and the user never picks.
	MaxOutputBytes int `yaml:"max_output_bytes"`
	// SendReasoning replays the model's reasoning_content into later turns. It
	// is a pointer so an omitted block can be told apart from an explicit false;
	// it defaults to true.
	SendReasoning *bool `yaml:"send_reasoning"`
}

// Reasoning reports whether reasoning_content is replayed into the conversation.
func (a AgentConfig) Reasoning() bool {
	return a.SendReasoning == nil || *a.SendReasoning
}

// TruncationConfig caps payload sizes on their way into telemetry. A value of 0
// or less is resolved to the package default, so an omitted block behaves exactly
// as before.
type TruncationConfig struct {
	CommandOutput int `yaml:"command_output"`
	LogContent    int `yaml:"log_content"`
	ToolResult    int `yaml:"tool_result"`
	Nudge         int `yaml:"nudge"`
}

// Duration wraps time.Duration to support YAML string parsing like "60s", "10m".
type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var i int64
	if err := node.Decode(&i); err == nil {
		*d = Duration(time.Duration(i) * time.Second)
		return nil
	}
	var s string
	if err := node.Decode(&s); err == nil {
		dur, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		*d = Duration(dur)
		return nil
	}
	var td time.Duration
	if err := node.Decode(&td); err == nil {
		*d = Duration(td)
		return nil
	}
	return fmt.Errorf("invalid duration %q", node.Value)
}

type OtelConfig struct {
	Enabled            bool              `yaml:"enabled"`
	Endpoint           string            `yaml:"endpoint"`
	ServiceName        string            `yaml:"service_name"`
	ResourceAttributes map[string]string `yaml:"resource_attributes"`
}

type Config struct {
	VM              VMConfig         `yaml:"vm"`
	Model           string           `yaml:"model"`
	Prompt          string           `yaml:"prompt"`
	AllowedCommands map[string]bool  `yaml:"-"`
	ContentDir      string           `yaml:"content_dir"`
	LLM             LLMConfig        `yaml:"llm"`
	Agent           AgentConfig      `yaml:"agent"`
	Truncation      TruncationConfig `yaml:"truncation"`
	Otel            OtelConfig       `yaml:"otel"`
}

func (c Config) Validate() error {
	if err := c.VM.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(c.Model) == "" {
		return ErrModelRequired
	}
	if strings.TrimSpace(c.Prompt) == "" {
		return ErrPromptRequired
	}
	if strings.TrimSpace(c.ContentDir) != "" {
		cleaned := filepath.Clean(strings.TrimSpace(c.ContentDir))
		info, err := os.Stat(cleaned)
		if err != nil {
			return fmt.Errorf("content_dir %q: %w", cleaned, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("content_dir %q is not a directory", cleaned)
		}
	}
	return nil
}

// configRaw is defined at package level to avoid infinite recursion in
// Config.UnmarshalYAML. Decoding directly into Config would re-enter this
// method; decoding into an alias without the custom method breaks the cycle.
// It also lets us decode allowed_commands as []string then convert to
// map[string]bool.
type configRaw struct {
	VM              VMConfig         `yaml:"vm"`
	Model           string           `yaml:"model"`
	Prompt          string           `yaml:"prompt"`
	AllowedCommands []string         `yaml:"allowed_commands"`
	ContentDir      string           `yaml:"content_dir"`
	LLM             LLMConfig        `yaml:"llm"`
	Agent           AgentConfig      `yaml:"agent"`
	Truncation      TruncationConfig `yaml:"truncation"`
	Otel            OtelConfig       `yaml:"otel"`
}

func (c *Config) UnmarshalYAML(node *yaml.Node) error {
	var raw configRaw
	if err := node.Decode(&raw); err != nil {
		return err
	}
	c.VM = raw.VM
	c.Model = raw.Model
	c.Prompt = raw.Prompt
	c.AllowedCommands = normalizeAllowedCommands(raw.AllowedCommands)
	c.ContentDir = strings.TrimSpace(raw.ContentDir)
	c.LLM = raw.LLM
	c.Agent = raw.Agent
	c.Truncation = raw.Truncation
	c.Otel = raw.Otel
	return nil
}

// AllowedCommandsList returns the allowed commands as a sorted slice, useful for display.
func (c Config) AllowedCommandsList() []string {
	if len(c.AllowedCommands) == 0 {
		return nil
	}
	out := make([]string, 0, len(c.AllowedCommands))
	for k := range c.AllowedCommands {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func normalizeAllowedCommands(cmds []string) map[string]bool {
	if len(cmds) == 0 {
		return nil
	}
	set := make(map[string]bool, len(cmds))
	for _, c := range cmds {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if c == "coreUtils" {
			for _, cu := range coreUtils {
				set[cu] = true
			}
			continue
		}
		set[c] = true
	}
	if len(set) == 0 {
		return nil
	}
	return set
}

func LoadFile(path string) (Config, error) {
	cleanPath := filepath.Clean(path)
	data, err := os.ReadFile(cleanPath)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", cleanPath, err)
	}
	return Parse(data)
}

func Parse(data []byte) (Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse yaml: %w", err)
	}

	if cfg.VM.Name == "" {
		cfg.VM.Name = "mph-vm"
	}

	// Otel endpoint and service name are resolved later, in
	// cmd/mph.resolveOtelConfig, which layers the environment over the YAML.
	// Defaulting them here would make that resolution order impossible to read.

	if cfg.LLM.ToolChoice == "" {
		cfg.LLM.ToolChoice = "auto"
	}
	if cfg.Agent.MaxIterations == 0 {
		cfg.Agent.MaxIterations = DefaultMaxIterations
	}
	if time.Duration(cfg.Agent.ChatTimeout) == 0 {
		cfg.Agent.ChatTimeout = Duration(DefaultChatTimeout)
	}
	if time.Duration(cfg.Agent.TotalTimeout) == 0 {
		cfg.Agent.TotalTimeout = Duration(DefaultTotalTimeout)
	}
	if cfg.LLM.MaxOutputTokens == 0 {
		cfg.LLM.MaxOutputTokens = DefaultLLMMaxOutputTokens
	}
	// Agent.MaxOutputBytes is deliberately left at 0 when unset. The agent reads
	// that as "derive a cap from the live context window"; defaulting it here
	// would make an explicit user value indistinguishable from the absence of
	// one, and the window is not known until the model has loaded.

	if cfg.Truncation.CommandOutput <= 0 {
		cfg.Truncation.CommandOutput = DefaultCommandOutputBytes
	}
	if cfg.Truncation.LogContent <= 0 {
		cfg.Truncation.LogContent = DefaultLogContentBytes
	}
	if cfg.Truncation.ToolResult <= 0 {
		cfg.Truncation.ToolResult = DefaultToolResultBytes
	}
	if cfg.Truncation.Nudge <= 0 {
		cfg.Truncation.Nudge = DefaultNudgeBytes
	}

	if strings.TrimSpace(cfg.ContentDir) != "" {
		cfg.ContentDir = filepath.Clean(strings.TrimSpace(cfg.ContentDir))
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate: %w", err)
	}

	return cfg, nil
}
