package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

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
	_, _ = fmt.Fprintf(w, "    context_window: 8192 # 0 = auto-tune\n")
	_, _ = fmt.Fprintf(w, "  agent:                # optional, agent loop params\n")
	_, _ = fmt.Fprintf(w, "    max_iterations: 10\n")
	_, _ = fmt.Fprintf(w, "    chat_timeout: 300s\n")
	_, _ = fmt.Fprintf(w, "    total_timeout: 30m\n")
	_, _ = fmt.Fprintf(w, "  otel:                 # optional, OpenTelemetry\n")
	_, _ = fmt.Fprintf(w, "    enabled: false\n")
	_, _ = fmt.Fprintf(w, "    endpoint: localhost:4317\n")
	_, _ = fmt.Fprintf(w, "    service_name: mph\n")
	_, _ = fmt.Fprintf(w, "    resource_attributes:\n")
	_, _ = fmt.Fprintf(w, "      environment: dev\n\n")
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

	otelCfg := resolveOtelConfig(cfg.Otel, otelEnabled)

	if otelCfg.Enabled {
		shutdown, err := mphotel.Setup(otelCfg)
		if err != nil {
			return fmt.Errorf("otel setup: %w", err)
		}
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := shutdown(ctx); err != nil {
				log.Warn().Err(err).Msg("otel shutdown failed")
			}
		}()
		log.Logger = log.Logger.Hook(mphotel.NewHook("mph"))
		log.Info().Str("otel.endpoint", otelCfg.Endpoint).Str("otel.service_name", otelCfg.ServiceName).Msg("otel enabled")
	}

	log.Info().
		Str("config", configPath).
		Str("vm", cfg.VM.Name).
		Str("model", cfg.Model).
		Str("content_dir", cfg.ContentDir).
		Strs("allowed_commands", cfg.AllowedCommandsList()).
		Float64("llm.temperature", cfg.LLM.Temperature).
		Float64("llm.top_p", cfg.LLM.TopP).
		Int("llm.top_k", cfg.LLM.TopK).
		Str("llm.tool_choice", cfg.LLM.ToolChoice).
		Int("llm.context_window", cfg.LLM.ContextWindow).
		Int("agent.max_iterations", cfg.Agent.MaxIterations).
		Str("agent.chat_timeout", time.Duration(cfg.Agent.ChatTimeout).String()).
		Str("agent.total_timeout", time.Duration(cfg.Agent.TotalTimeout).String()).
		Bool("otel.enabled", otelCfg.Enabled).
		Msg("loaded config")

	ctx := context.Background()

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

	llmCfg := agent.LLMConfig{
		Temperature: cfg.LLM.Temperature,
		TopP:        cfg.LLM.TopP,
		TopK:        cfg.LLM.TopK,
		ToolChoice:  cfg.LLM.ToolChoice,
	}

	agt := agent.NewAgent(
		log.Logger,
		krn,
		cfg.Agent.MaxIterations,
		time.Duration(cfg.Agent.ChatTimeout),
		time.Duration(cfg.Agent.TotalTimeout),
		llmCfg,
	)

	client := multipass.New(log.Logger)

	return harness.Start(ctx, agt, client, cfg, ignoreExisting, keep)
}

func setupLogger(verbose, pretty bool) {
	zerolog.TimeFieldFormat = time.RFC3339
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

func resolveOtelConfig(yamlCfg config.OtelConfig, flagEnabled bool) mphotel.Config {
	enabled := yamlCfg.Enabled
	if flagEnabled || isEnvTrue(os.Getenv("MPH_OTEL")) {
		enabled = true
	}

	endpoint := yamlCfg.Endpoint
	if v := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")); v != "" {
		endpoint = v
	}
	if endpoint == "" {
		endpoint = "localhost:4317"
	}

	serviceName := yamlCfg.ServiceName
	if v := strings.TrimSpace(os.Getenv("OTEL_SERVICE_NAME")); v != "" {
		serviceName = v
	}
	if serviceName == "" {
		serviceName = "mph"
	}

	return mphotel.Config{
		Enabled:            enabled,
		Endpoint:           endpoint,
		ServiceName:        serviceName,
		ResourceAttributes: yamlCfg.ResourceAttributes,
	}
}

func isEnvTrue(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "true" || v == "1" || v == "yes"
}
