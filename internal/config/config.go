package config

import (
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/canonical/lxd/shared/validate"
	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/basicflag"
	koanfenv "github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"

	mphvalidation "github.com/trollLemon/MPHarness/v2/internal/validation"
)

var (
	ErrVMDiskRequired = errors.New("vm.disk is required (e.g. \"20G\")")
	ErrVMRAMRequired  = errors.New("vm.ram is required (e.g. \"4G\")")
	ErrVMCPURequired  = errors.New("vm.cpu must be > 0")
	ErrVMNameInvalid  = errors.New("vm.name must be a valid LXD instance name")
	ErrModelRequired  = errors.New("model is required (LLM, e.g. \"unsloth/Qwen3-0.6B-Q8_0\")")
	ErrPromptRequired = errors.New("prompt is required")

	ErrInvalidSecretsSource = errors.New(`secrets.source must be "env" or "keyring"`)
)

// VMContentDir is the fixed directory inside the VM where host content_dir is copied.
// The harness creates this path and transfers the local directory there before the agent starts.
const VMContentDir = "/home/ubuntu/content"

// OutputBaseDir is the host-side root for captured command output. One run owns
// one subdirectory of it. The harness creates that subdirectory inside the VM
// before the agent starts writing captures.
const OutputBaseDir = "/tmp/mph-output"

// OutputRunDir is the capture directory for one run.
func OutputRunDir(runID string) string {
	return OutputBaseDir + "/" + runID
}

// Agent defaults. Named so the help text in mph cannot drift from the values
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

// Output and compaction defaults. Copying this example unchanged gets current
// behaviour plus the new features at their default settings.
const (
	DefaultOutputMaxCommandBytes = 64 * 1024 * 1024
	DefaultOutputMaxTotalBytes   = 512 * 1024 * 1024
	DefaultOutputSearchMatches   = 200

	DefaultCompactionThreshold   = 0.8
	DefaultCompactionMaxTokens   = 2048
	DefaultCompactionMaxAttempts = 3
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
	Disk   string `yaml:"disk"`
	RAM    string `yaml:"ram"`
	CPU    int    `yaml:"cpu"`
	Name   string `yaml:"name"`
	Image  string `yaml:"image"`
	Kernel string `yaml:"kernel"`
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
	if err := validate.IsHostname(v.Name); err != nil {
		return fmt.Errorf("%w: %s", ErrVMNameInvalid, err)
	}
	return nil
}

var imageRemotes = map[string]string{
	"ubuntu":       "https://cloud-images.ubuntu.com/releases/",
	"ubuntu-daily": "https://cloud-images.ubuntu.com/daily/",
	"images":       "https://images.lxd.canonical.com",
}

var imagePrefixes = map[string]string{
	"ubuntu":       "ubuntu",
	"release":      "ubuntu",
	"ubuntu-daily": "ubuntu-daily",
	"daily":        "ubuntu-daily",
	"images":       "images",
}

// ImageRemote maps a vm.image value onto its simplestreams remote URL and alias.
func ImageRemote(image string) (remoteURL, alias string, err error) {
	image = strings.TrimSpace(image)
	if image == "" {
		return imageRemotes["ubuntu"], "24.04", nil
	}
	prefix, name, ok := strings.Cut(image, ":")
	if !ok {
		return imageRemotes["ubuntu"], image, nil
	}
	remote, ok := imagePrefixes[prefix]
	if !ok || strings.TrimSpace(name) == "" {
		return "", "", fmt.Errorf("unsupported image %q: supported prefixes are release:, daily:, ubuntu:, ubuntu-daily:, images:, or a bare Ubuntu alias such as noble", image)
	}
	return imageRemotes[remote], strings.TrimSpace(name), nil
}

// NormalizeSize converts a human size (20G, 4096M, 1GiB, 1073741824) into the
// binary-unit form LXD accepts (20GiB, 4GiB, 1GiB).
func NormalizeSize(size string) (string, error) {
	bytes, err := parseSizeBytes(size, false)
	if err != nil {
		return "", err
	}
	for _, unit := range []struct {
		suffix string
		size   int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if bytes%unit.size == 0 {
			return strconv.FormatInt(bytes/unit.size, 10) + unit.suffix, nil
		}
	}
	return strconv.FormatInt(bytes, 10), nil
}

// parseSizeBytes parses the one size syntax shared by VM sizing and output
// caps. Units are binary: K and KB both mean 1024, matching the suffix LXD
// expects. allowZero keeps an explicit 0 meaningful for caps that use it to
// disable a limit.
func parseSizeBytes(size string, allowZero bool) (int64, error) {
	s := strings.TrimSpace(size)
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	value, err := strconv.ParseFloat(s[:i], 64)
	if i == 0 || err != nil || value < 0 || (!allowZero && value == 0) {
		return 0, fmt.Errorf("invalid size %q", size)
	}
	var multiplier float64
	switch strings.ToUpper(strings.TrimSpace(s[i:])) {
	case "", "B":
		multiplier = 1
	case "K", "KB", "KIB":
		multiplier = 1 << 10
	case "M", "MB", "MIB":
		multiplier = 1 << 20
	case "G", "GB", "GIB":
		multiplier = 1 << 30
	case "T", "TB", "TIB":
		multiplier = 1 << 40
	default:
		return 0, fmt.Errorf("invalid size %q: supported units are K, M, G, T (optionally B or iB), or bytes", size)
	}
	if value > float64(math.MaxInt64)/multiplier {
		return 0, fmt.Errorf("invalid size %q: value overflows", size)
	}
	if multiplier == 1 && value != math.Trunc(value) {
		return 0, fmt.Errorf("invalid size %q: bytes must be a whole number", size)
	}
	bytes := int64(value * multiplier)
	if bytes < 0 || (!allowZero && bytes == 0) {
		return 0, fmt.Errorf("invalid size %q", size)
	}
	return bytes, nil
}

type LLMConfig struct {
	Temperature     float64 `yaml:"temperature"`
	TopP            float64 `yaml:"top_p"`
	TopK            int     `yaml:"top_k"`
	ContextWindow   int     `yaml:"context_window"`
	MaxOutputTokens int     `yaml:"max_output_tokens"`
}

// WithDefaults fills the LLM settings that have a fixed fallback. It is
// idempotent, so each layer can apply it without duplicating the literals.
func (l LLMConfig) WithDefaults() LLMConfig {
	if l.MaxOutputTokens == 0 {
		l.MaxOutputTokens = DefaultLLMMaxOutputTokens
	}
	return l
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

var (
	durationType = reflect.TypeOf(Duration(0))
	sizeType     = reflect.TypeOf(Size(0))
)

const (
	maxDurationSeconds = int64(math.MaxInt64) / int64(time.Second)
	minDurationSeconds = int64(math.MinInt64) / int64(time.Second)
)

func durationDecodeHook(_, to reflect.Type, data any) (any, error) {
	if to != durationType {
		return data, nil
	}
	switch value := data.(type) {
	case nil:
		return Duration(0), nil
	case string:
		d, err := time.ParseDuration(value)
		if err != nil {
			return nil, fmt.Errorf("invalid duration %q: %w", value, err)
		}
		return Duration(d), nil
	case int:
		return durationFromSeconds(int64(value))
	case int64:
		return durationFromSeconds(value)
	case uint64:
		if value > uint64(maxDurationSeconds) {
			return nil, fmt.Errorf("invalid duration %d seconds: out of range", value)
		}
		return Duration(time.Duration(value) * time.Second), nil
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) || value < float64(minDurationSeconds) || value > float64(maxDurationSeconds) {
			return nil, fmt.Errorf("invalid duration %v seconds", value)
		}
		return durationFromSeconds(int64(value))
	default:
		return nil, fmt.Errorf("invalid duration %v", data)
	}
}

func durationFromSeconds(seconds int64) (Duration, error) {
	if seconds < minDurationSeconds || seconds > maxDurationSeconds {
		return 0, fmt.Errorf("invalid duration %d seconds: out of range", seconds)
	}
	return Duration(time.Duration(seconds) * time.Second), nil
}

func scalarDecodeHook(_, to reflect.Type, data any) (any, error) {
	switch to.Kind() {
	case reflect.String:
		switch value := data.(type) {
		case string:
			return value, nil
		case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
			return fmt.Sprint(value), nil
		}
	case reflect.Bool:
		if value, ok := data.(string); ok {
			switch strings.ToLower(strings.TrimSpace(value)) {
			case "y", "yes", "on":
				return true, nil
			case "n", "no", "off":
				return false, nil
			}
		}
	}
	return data, nil
}

type OtelConfig struct {
	Enabled            bool              `yaml:"enabled"`
	Endpoint           string            `yaml:"endpoint"`
	ServiceName        string            `yaml:"service_name"`
	ResourceAttributes map[string]string `yaml:"resource_attributes"`
}

// Size is a byte count. It accepts a unit string such as "64MiB" or a bare
// integer byte count such as 1024, and the empty string as zero. 0 means
// "derive" for inline_max_size and "disabled" for the caps.
type Size int64

func sizeDecodeHook(_, to reflect.Type, data any) (any, error) {
	if to != sizeType {
		return data, nil
	}
	switch value := data.(type) {
	case string:
		n, err := parseSize(value)
		if err != nil {
			return nil, err
		}
		return Size(n), nil
	case int:
		return Size(value), nil
	case int64:
		return Size(value), nil
	case uint64:
		if value > math.MaxInt64 {
			return nil, fmt.Errorf("size %d overflows", value)
		}
		return Size(value), nil
	default:
		return nil, fmt.Errorf("size must be a string like %q or a whole number of bytes, got %T", "64MiB", data)
	}
}

func parseSize(s string) (int64, error) {
	if strings.TrimSpace(s) == "" {
		return 0, nil
	}
	return parseSizeBytes(s, true)
}

type OutputConfig struct {
	Enabled          bool   `yaml:"enabled"`
	Mode             string `yaml:"mode"`
	InlineMaxSize    Size   `yaml:"inline_max_size"`
	MaxCommandSize   Size   `yaml:"max_command_size"`
	MaxTotalSize     Size   `yaml:"max_total_size"`
	SearchMaxMatches int    `yaml:"search_max_matches"`
}

func (o OutputConfig) Validate() error {
	if !o.Enabled {
		return nil
	}
	if o.Mode != "auto" && o.Mode != "always" {
		return fmt.Errorf("output.mode must be auto or always, got %q", o.Mode)
	}
	if o.SearchMaxMatches < 0 {
		return fmt.Errorf("output.search_max_matches must be >= 0, got %d", o.SearchMaxMatches)
	}
	if o.InlineMaxSize < 0 || o.MaxCommandSize < 0 || o.MaxTotalSize < 0 {
		return fmt.Errorf("output sizes must be >= 0")
	}
	return nil
}

type CompactionConfig struct {
	Enabled          bool    `yaml:"enabled"`
	Threshold        float64 `yaml:"threshold"`
	MaxSummaryTokens int     `yaml:"max_summary_tokens"`
	MaxAttempts      int     `yaml:"max_attempts"`
}

func (c CompactionConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.Threshold <= 0.5 || c.Threshold > 0.95 {
		return fmt.Errorf("compaction.threshold must be in (0.5, 0.95], got %v", c.Threshold)
	}
	if c.MaxSummaryTokens <= 0 {
		return fmt.Errorf("compaction.max_summary_tokens must be > 0, got %d", c.MaxSummaryTokens)
	}
	if c.MaxAttempts <= 0 {
		return fmt.Errorf("compaction.max_attempts must be > 0, got %d", c.MaxAttempts)
	}
	return nil
}

// SecretsConfig selects where %{NAME} placeholders in agent commands are
// resolved from.
type SecretsConfig struct {
	// Source is "env" or "keyring"; Parse defaults it to "env".
	Source string `yaml:"source"`
	// Service is the freedesktop Secret Service service name. Entries are
	// stored under it with the placeholder name as the username.
	Service string `yaml:"service"`
}

// Validate reports whether Source names a supported resolver.
func (s SecretsConfig) Validate() error {
	switch s.Source {
	case "", "env", "keyring":
		return nil
	default:
		return ErrInvalidSecretsSource
	}
}

// IsSafeValue reports whether a secret value can be substituted raw into a
// command string that becomes `bash -c`. Values must match
// [A-Za-z0-9._+/=:@-]+ (non-empty): this covers GitHub PATs, JWTs, base64
// (+/=), UUIDs and AWS/Azure key shapes, while rejecting anything the remote
// shell would reinterpret (whitespace, quotes, `$`, backticks, `;`, `|`, ...).
// `~` is excluded on purpose: at the start of a word it triggers tilde
// expansion.
func IsSafeValue(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case c == '_' || c == '.' || c == '+' || c == '/' || c == '=' || c == ':' || c == '@' || c == '-':
		default:
			return false
		}
	}
	return true
}

type RunConfig struct {
	Name string `yaml:"name"`
}

type Config struct {
	Run             RunConfig        `yaml:"run"`
	VM              VMConfig         `yaml:"vm"`
	Model           string           `yaml:"model"`
	Prompt          string           `yaml:"prompt"`
	AllowedCommands map[string]bool  `yaml:"-"`
	ContentDir      string           `yaml:"content_dir"`
	LLM             LLMConfig        `yaml:"llm"`
	Agent           AgentConfig      `yaml:"agent"`
	Truncation      TruncationConfig `yaml:"truncation"`
	Otel            OtelConfig       `yaml:"otel"`
	Output          OutputConfig     `yaml:"output"`
	Compaction      CompactionConfig `yaml:"compaction"`
	Secrets         SecretsConfig    `yaml:"secrets"`
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
	if err := c.Output.Validate(); err != nil {
		return err
	}
	if err := c.Compaction.Validate(); err != nil {
		return err
	}
	if err := c.Secrets.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(c.ContentDir) != "" {
		if _, err := mphvalidation.ContentDirPath(c.ContentDir); err != nil {
			return err
		}
	}
	return nil
}

// AllowedCommandsList returns the allowed commands as a sorted slice.
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

// Sources are the non-file configuration layers applied after YAML.
type Sources struct {
	Environ []string
	Flags   *flag.FlagSet
}

func LoadFileWithSources(path string, sources Sources) (Config, error) {
	cleanPath := filepath.Clean(path)
	data, err := os.ReadFile(cleanPath)
	if err != nil {
		return Config{}, fmt.Errorf("could not read config %q: %w", cleanPath, err)
	}
	return load(data, sources)
}

func LoadFile(path string) (Config, error) {
	return LoadFileWithSources(path, Sources{})
}

func Parse(data []byte) (Config, error) {
	return load(data, Sources{})
}

func load(data []byte, sources Sources) (Config, error) {
	k := koanf.New(".")
	if err := k.Load(rawbytes.Provider(data), yaml.Parser()); err != nil {
		return Config{}, fmt.Errorf("could not parse config YAML: %w", err)
	}
	if len(sources.Environ) > 0 {
		if err := validateEnv(sources.Environ); err != nil {
			return Config{}, fmt.Errorf("invalid environment configuration: %w", err)
		}
		err := k.Load(koanfenv.Provider(".", koanfenv.Opt{
			EnvironFunc:   func() []string { return sources.Environ },
			TransformFunc: transformEnv,
		}), nil)
		if err != nil {
			return Config{}, fmt.Errorf("could not apply environment overrides: %w", err)
		}
	}
	if sources.Flags != nil {
		provider := basicflag.ProviderWithValue(sources.Flags, ".", transformFlag, k)
		if err := k.Load(provider, nil); err != nil {
			return Config{}, fmt.Errorf("could not apply command-line overrides: %w", err)
		}
	}

	var cfg Config
	if err := k.UnmarshalWithConf("", &cfg, unmarshalConf()); err != nil {
		return Config{}, fmt.Errorf("could not decode configuration: %w", err)
	}
	var allowed []string
	if k.Exists("allowed_commands") {
		if err := k.UnmarshalWithConf("allowed_commands", &allowed, unmarshalConf()); err != nil {
			return Config{}, fmt.Errorf("could not decode allowed_commands: %w", err)
		}
	}
	cfg.AllowedCommands = normalizeAllowedCommands(allowed)
	cfg.ContentDir = strings.TrimSpace(cfg.ContentDir)

	applyDefaults(k, &cfg)

	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	if cfg.ContentDir != "" {
		cfg.ContentDir = filepath.Clean(cfg.ContentDir)
	}

	return cfg, nil
}

func unmarshalConf() koanf.UnmarshalConf {
	return koanf.UnmarshalConf{
		Tag: "yaml",
		DecoderConfig: &mapstructure.DecoderConfig{
			DecodeHook: mapstructure.ComposeDecodeHookFunc(durationDecodeHook, sizeDecodeHook, scalarDecodeHook),
		},
	}
}

func transformEnv(key, value string) (string, any) {
	switch key {
	case "MPH_RUN_NAME":
		if value = strings.TrimSpace(value); value != "" {
			return "run.name", value
		}
	case "MPH_OTEL":
		if enabled, err := parseEnvBool(value); err == nil {
			return "otel.enabled", enabled
		}
	case "OTEL_EXPORTER_OTLP_ENDPOINT":
		if value = strings.TrimSpace(value); value != "" {
			return "otel.endpoint", value
		}
	case "OTEL_SERVICE_NAME":
		if value = strings.TrimSpace(value); value != "" {
			return "otel.service_name", value
		}
	}
	return "", nil
}

func validateEnv(environ []string) error {
	for _, entry := range environ {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key != "MPH_OTEL" {
			continue
		}
		if _, err := parseEnvBool(value); err != nil {
			return fmt.Errorf("MPH_OTEL: %w", err)
		}
	}
	return nil
}

func parseEnvBool(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("must be \"true\" or \"false\", got %q", value)
	}
}

func transformFlag(key, value string) (string, any) {
	if key != "otel" {
		return "", nil
	}
	if value == "" {
		// basicflag probes explicitly set flags with an empty value.
		return "otel.enabled", nil
	}
	enabled, err := strconv.ParseBool(value)
	if err != nil {
		return "", nil
	}
	return "otel.enabled", enabled
}

func defaultIfZero[T comparable](value *T, def T) {
	var zero T
	if *value == zero {
		*value = def
	}
}

func defaultIfNotPositive(value *int, def int) {
	if *value <= 0 {
		*value = def
	}
}

func applyDefaults(k *koanf.Koanf, cfg *Config) {
	defaultIfZero(&cfg.VM.Name, "mph-vm")

	// Otel endpoint and service name are defaulted later, in
	// mph.resolveOtelConfig.

	cfg.LLM = cfg.LLM.WithDefaults()
	defaultIfZero(&cfg.Agent.MaxIterations, DefaultMaxIterations)
	defaultIfZero(&cfg.Agent.ChatTimeout, Duration(DefaultChatTimeout))
	defaultIfZero(&cfg.Agent.TotalTimeout, Duration(DefaultTotalTimeout))
	// Agent.MaxOutputBytes is deliberately left at 0 when unset. The agent reads
	// that as "derive a cap from the live context window"; defaulting it here
	// would make an explicit user value indistinguishable from the absence of
	// one, and the window is not known until the model has loaded.

	defaultIfNotPositive(&cfg.Truncation.CommandOutput, DefaultCommandOutputBytes)
	defaultIfNotPositive(&cfg.Truncation.LogContent, DefaultLogContentBytes)
	defaultIfNotPositive(&cfg.Truncation.ToolResult, DefaultToolResultBytes)
	defaultIfNotPositive(&cfg.Truncation.Nudge, DefaultNudgeBytes)

	if !k.Exists("output") {
		cfg.Output = OutputConfig{
			Enabled: true, Mode: "auto",
			MaxCommandSize: DefaultOutputMaxCommandBytes, MaxTotalSize: DefaultOutputMaxTotalBytes,
			SearchMaxMatches: DefaultOutputSearchMatches,
		}
	} else {
		// A present block without `enabled:` opts in; only an explicit
		// `enabled: false` opts out.
		if !k.Exists("output.enabled") {
			cfg.Output.Enabled = true
		}
		defaultIfZero(&cfg.Output.Mode, "auto")
		if !k.Exists("output.max_command_size") {
			defaultIfZero(&cfg.Output.MaxCommandSize, DefaultOutputMaxCommandBytes)
		}
		if !k.Exists("output.max_total_size") {
			defaultIfZero(&cfg.Output.MaxTotalSize, DefaultOutputMaxTotalBytes)
		}
		// An explicit 0 disables the cap, so only default an absent key.
		if !k.Exists("output.search_max_matches") {
			defaultIfZero(&cfg.Output.SearchMaxMatches, DefaultOutputSearchMatches)
		}
	}

	if !k.Exists("compaction") {
		cfg.Compaction = CompactionConfig{
			Enabled: true, Threshold: DefaultCompactionThreshold,
			MaxSummaryTokens: DefaultCompactionMaxTokens,
			MaxAttempts:      DefaultCompactionMaxAttempts,
		}
	} else {
		if !k.Exists("compaction.enabled") {
			cfg.Compaction.Enabled = true
		}
		defaultIfZero(&cfg.Compaction.Threshold, DefaultCompactionThreshold)
		defaultIfZero(&cfg.Compaction.MaxSummaryTokens, DefaultCompactionMaxTokens)
		defaultIfZero(&cfg.Compaction.MaxAttempts, DefaultCompactionMaxAttempts)
	}
}
