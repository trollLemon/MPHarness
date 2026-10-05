package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"io"
	"os"
	"strings"
	"time"
	"uuid"

	"github.com/trollLemon/MPHarness/internal/agent"
	"github.com/trollLemon/MPHarness/internal/config"
	"github.com/trollLemon/MPHarness/internal/harness"
	"github.com/trollLemon/MPHarness/internal/multipass"
	mphotel "github.com/trollLemon/MPHarness/internal/otel"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		log.Error().Err(err).Msg("mph failed")
		os.Exit(1)
	}
}

func printUsage(w io.Writer) {
	_, _ = fmt.Fprintf(w, "mph - Multipass Harness\n\n")
	_, _ = fmt.Fprintf(w, "Usage: %s [flags] <config.yaml>\n\n", os.Args[0])
	_, _ = fmt.Fprintf(w, "Flags:\n")
	flag.PrintDefaults()
	_, _ = fmt.Fprintf(w, "\nConfig YAML keys:\n")
	_, _ = fmt.Fprintf(w, "  run:\n")
	_, _ = fmt.Fprintf(w, "    name: my-run        # optional, label for this run in logs/traces; default MPH_RUN_NAME, vm.name, or mph\n")
	_, _ = fmt.Fprintf(w, "  vm:\n")
	_, _ = fmt.Fprintf(w, "    name: mph-vm        # optional, default mph-vm\n")
	_, _ = fmt.Fprintf(w, "    disk: 20G\n")
	_, _ = fmt.Fprintf(w, "    ram: 4G\n")
	_, _ = fmt.Fprintf(w, "    cpu: 2\n")
	_, _ = fmt.Fprintf(w, "    image: noble       # optional, Ubuntu image (noble/jammy/focal/bionic or 22.04, release:noble, daily:resolute); default LTS\n")
	_, _ = fmt.Fprintf(w, "  model: <id>           # LLM, e.g. unsloth/Qwen3-0.6B-Q8_0 (downloaded on first run)\n")
	_, _ = fmt.Fprintf(w, "  prompt: \"do stuff in the VM\"\n")
	_, _ = fmt.Fprintf(w, "  content_dir: ./path   # optional, local dir to copy to %s in VM\n", config.VMContentDir)
	_, _ = fmt.Fprintf(w, "  allowed_commands:     # optional, restrict VM commands\n")
	_, _ = fmt.Fprintf(w, "    - apt\n")
	_, _ = fmt.Fprintf(w, "    - git\n")
	_, _ = fmt.Fprintf(w, "    - python3\n")
	_, _ = fmt.Fprintf(w, "  llm:                  # optional, LLM sampling params\n")
	_, _ = fmt.Fprintf(w, "    temperature: 0.2\n")
	_, _ = fmt.Fprintf(w, "    top_p: 0.95\n")
	_, _ = fmt.Fprintf(w, "    top_k: 40\n")
	_, _ = fmt.Fprintf(w, "    tool_choice: auto   # auto|required|none\n")
	_, _ = fmt.Fprintf(w, "    max_output_tokens: %d\n", config.DefaultLLMMaxOutputTokens)
	_, _ = fmt.Fprintf(w, "    context_window: 8192 # 0 = auto-tune\n")
	_, _ = fmt.Fprintf(w, "  agent:                # optional, agent loop params\n")
	_, _ = fmt.Fprintf(w, "    max_iterations: %d\n", config.DefaultMaxIterations)
	_, _ = fmt.Fprintf(w, "    chat_timeout: %s\n", config.DefaultChatTimeout)
	_, _ = fmt.Fprintf(w, "    total_timeout: %s\n", config.DefaultTotalTimeout)
	_, _ = fmt.Fprintf(w, "    max_output_bytes: 0   # auto-capture inline cutoff; 0 = derive from context_window\n")
	_, _ = fmt.Fprintf(w, "    send_reasoning: true # replay reasoning_content into later turns\n")
	_, _ = fmt.Fprintf(w, "  truncation:           # optional, telemetry payload caps in bytes\n")
	_, _ = fmt.Fprintf(w, "    command_output: %d\n", config.DefaultCommandOutputBytes)
	_, _ = fmt.Fprintf(w, "    log_content: %d\n", config.DefaultLogContentBytes)
	_, _ = fmt.Fprintf(w, "    tool_result: %d\n", config.DefaultToolResultBytes)
	_, _ = fmt.Fprintf(w, "    nudge: %d\n", config.DefaultNudgeBytes)
	_, _ = fmt.Fprintf(w, "  otel:                 # optional, OpenTelemetry\n")
	_, _ = fmt.Fprintf(w, "    enabled: false\n")
	_, _ = fmt.Fprintf(w, "    endpoint: %s\n", mphotel.DefaultEndpoint)
	_, _ = fmt.Fprintf(w, "    service_name: %s\n", mphotel.DefaultServiceName)
	_, _ = fmt.Fprintf(w, "    resource_attributes:\n")
	_, _ = fmt.Fprintf(w, "      environment: dev\n")
	_, _ = fmt.Fprintf(w, "  output:               # optional, captured command output\n")
	_, _ = fmt.Fprintf(w, "    enabled: true\n")
	_, _ = fmt.Fprintf(w, "    mode: auto          # auto|always\n")
	_, _ = fmt.Fprintf(w, "    inline_max_size: 0   # 0 = derive from tool-result budget\n")
	_, _ = fmt.Fprintf(w, "    max_command_size: 64MiB\n")
	_, _ = fmt.Fprintf(w, "    max_total_size: 512MiB\n")
	_, _ = fmt.Fprintf(w, "    search_max_matches: %d\n", config.DefaultOutputSearchMatches)
	_, _ = fmt.Fprintf(w, "  compaction:           # optional, conversation compaction\n")
	_, _ = fmt.Fprintf(w, "    enabled: true\n")
	_, _ = fmt.Fprintf(w, "    threshold: 0.8      # fraction of context window, (0.5, 0.95]\n")
	_, _ = fmt.Fprintf(w, "    max_summary_tokens: %d\n", config.DefaultCompactionMaxTokens)
	_, _ = fmt.Fprintf(w, "    min_messages: %d\n", config.DefaultCompactionMinMessages)
	_, _ = fmt.Fprintf(w, "    max_attempts: %d\n\n", config.DefaultCompactionMaxAttempts)
	_, _ = fmt.Fprintf(w, "Example:\n")
	_, _ = fmt.Fprintf(w, "  mph -v ./mph.yaml\n")

}

func run() error {
	var (
		configPath     string
		showVersion    bool
		verbose        bool
		pretty         bool
		needHelp       bool
		ignoreExisting bool
		keep           bool
		otelEnabled    bool
	)

	flag.StringVar(&configPath, "config", "", "path to yaml config file (or positional arg)")
	flag.StringVar(&configPath, "c", "", "path to yaml config file (shorthand)")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.BoolVar(&verbose, "verbose", false, "verbose logging (debug)")
	flag.BoolVar(&verbose, "v", false, "verbose logging (debug) shorthand")
	flag.BoolVar(&pretty, "pretty", false, "pretty-print logs (human-readable console) instead of JSON")
	flag.BoolVar(&needHelp, "help", false, "show help")
	flag.BoolVar(&needHelp, "h", false, "show help (shorthand)")
	flag.BoolVar(&ignoreExisting, "i", false, "ignore that a VM with the given name exists already and run against that VM")
	flag.BoolVar(&keep, "k", false, "keep the VM after execution rather than deleting it")
	flag.BoolVar(&otelEnabled, "otel", false, "enable OpenTelemetry tracing (also via MPH_OTEL=true)")

	flag.Usage = func() { printUsage(os.Stderr) }

	flag.Parse()

	if needHelp {
		printUsage(os.Stdout)
		return nil
	}

	setupLogger(verbose, pretty)

	if showVersion {
		fmt.Printf("mph %s\n", version)
		return nil
	}

	if configPath == "" && flag.NArg() > 0 {
		configPath = flag.Arg(0)
	}
	if configPath == "" {
		printUsage(os.Stderr)
		return errors.New("config file required")
	}

	cfg, err := config.LoadFile(configPath)
	if err != nil {
		return err
	}

	ctx := context.Background()

	ident := newRunIdentity(cfg.Run, cfg.VM.Name)
	otelCfg := resolveOtelConfig(cfg.Otel, otelEnabled, ident.Label, ident.Name)

	if otelCfg.Enabled {
		shutdown, err := mphotel.Setup(ctx, otelCfg)
		if err != nil {
			return fmt.Errorf("otel setup: %w", err)
		}
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if err := shutdown(shutdownCtx); err != nil {
				log.Warn().Err(err).Msg("otel flush/shutdown failed (some telemetry may be lost)")
			} else {
				log.Debug().Msg("otel flush/shutdown complete")
			}
		}()
		log.Logger = log.Logger.Hook(mphotel.NewHook("mph"))
		log.Info().Str("endpoint", otelCfg.Endpoint).Str("svc", otelCfg.ServiceName).Msg("otel enabled")
	}

	log.Info().
		Str("cfg", configPath).
		Str("vm", cfg.VM.Name).
		Str("model", cfg.Model).
		Str("dir", cfg.ContentDir).
		Int("cmds", len(cfg.AllowedCommandsList())).
		Float64("temp", cfg.LLM.Temperature).
		Float64("top_p", cfg.LLM.TopP).
		Int("top_k", cfg.LLM.TopK).
		Str("tools", cfg.LLM.ToolChoice).
		Int("ctx_win", cfg.LLM.ContextWindow).
		Int("iters", cfg.Agent.MaxIterations).
		Str("chat_to", time.Duration(cfg.Agent.ChatTimeout).String()).
		Str("total_to", time.Duration(cfg.Agent.TotalTimeout).String()).
		Bool("otel", otelCfg.Enabled).
		Bool("out_on", cfg.Output.Enabled).
		Str("out_mode", cfg.Output.Mode).
		Bool("compact", cfg.Compaction.Enabled).
		Float64("compact_thr", cfg.Compaction.Threshold).
		Msg("loaded config")
	log.Debug().Strs("cmds", cfg.AllowedCommandsList()).Msg("allowed commands")

	mp, err := agent.InitializeModelFiles(ctx, log.Logger, cfg.Model)
	if err != nil {
		return err
	}

	krn, err := agent.NewKronk(mp, log.Logger, cfg.LLM.ContextWindow)
	if err != nil {
		return err
	}
	defer func() {
		if err := krn.Unload(ctx); err != nil {
			log.Warn().Err(err).Msg("LLM unload failed")
		}
	}()

	agt := agent.NewAgent(log.Logger, krn, agent.Options{
		MaxIterations: cfg.Agent.MaxIterations,
		ChatTimeout:   time.Duration(cfg.Agent.ChatTimeout),
		TotalTimeout:  time.Duration(cfg.Agent.TotalTimeout),
		LLM:           cfg.LLM,
		RunID:         ident.ID,
	})

	client := multipass.New(log.Logger)

	return harness.Start(ctx, agt, client, cfg, harness.Options{
		IgnoreExisting: ignoreExisting,
		Keep:           keep,
		RunID:          ident.ID,
		RunLabel:       ident.Label,
		RunName:        ident.Name,
	})
}

func setupLogger(verbose, pretty bool) {
	zerolog.TimeFieldFormat = time.RFC3339
	zerolog.CallerMarshalFunc = func(_ uintptr, file string, line int) string {
		if i := strings.LastIndexByte(file, '/'); i >= 0 {
			file = file[i+1:]
		}
		return file + ":" + strings.TrimSpace(fmt.Sprint(line))
	}
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	if verbose {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	}

	if pretty {
		log.Logger = zerolog.New(zerolog.ConsoleWriter{
			Out:        os.Stdout,
			TimeFormat: "15:04:05",
		}).With().Timestamp().Caller().Logger()
		return
	}

	log.Logger = zerolog.New(os.Stdout).With().Timestamp().Caller().Logger()
}

// runIdentity splits one run's identity into the three forms the codebase
// needs: a bare uuid for output paths (a composite containing ":" would be an
// invalid path), a self-describing composite for telemetry, and the bare
// human-readable name.
type runIdentity struct {
	ID    string
	Label string
	Name  string
}

func resolveRunName(yamlName, envName, vmName string) string {
	for _, candidate := range []string{yamlName, envName, vmName} {
		if name := strings.TrimSpace(candidate); name != "" {
			return name
		}
	}
	return "mph"
}

func newRunIdentity(yamlCfg config.RunConfig, vmName string) runIdentity {
	name := resolveRunName(yamlCfg.Name, os.Getenv("MPH_RUN_NAME"), vmName)
	id := uuid.NewV7().String()
	return runIdentity{ID: id, Label: name + ":" + id, Name: name}
}

func resolveOtelConfig(yamlCfg config.OtelConfig, flagEnabled bool, runID, runName string) mphotel.Config {
	enabled := yamlCfg.Enabled
	if flagEnabled || isEnvTrue(os.Getenv("MPH_OTEL")) {
		enabled = true
	}

	endpoint := yamlCfg.Endpoint
	if v := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")); v != "" {
		endpoint = v
	}
	if endpoint == "" {
		endpoint = mphotel.DefaultEndpoint
	}

	serviceName := yamlCfg.ServiceName
	if v := strings.TrimSpace(os.Getenv("OTEL_SERVICE_NAME")); v != "" {
		serviceName = v
	}
	if serviceName == "" {
		serviceName = mphotel.DefaultServiceName
	}

	return mphotel.Config{
		Enabled:            enabled,
		Endpoint:           endpoint,
		ServiceName:        serviceName,
		ResourceAttributes: yamlCfg.ResourceAttributes,
		RunID:              runID,
		RunName:            runName,
	}
}

func isEnvTrue(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "true" || v == "1" || v == "yes"
}
