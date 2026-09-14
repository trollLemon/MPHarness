package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/trollLemon/MPHarness/internal/agent"
	"github.com/trollLemon/MPHarness/internal/config"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		log.Error().Err(err).Msg("mph failed")
		os.Exit(1)
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, "mph - Multipass Harness\n\n")
	fmt.Fprintf(w, "Usage: %s [flags] <config.yaml>\n\n", os.Args[0])
	fmt.Fprintf(w, "Flags:\n")
	flag.PrintDefaults()
	fmt.Fprintf(w, "\nConfig YAML keys:\n")
	fmt.Fprintf(w, "  vm:\n")
	fmt.Fprintf(w, "    name: mph-vm        # optional, default mph-vm\n")
	fmt.Fprintf(w, "    disk: 20G\n")
	fmt.Fprintf(w, "    ram: 4G\n")
	fmt.Fprintf(w, "    cpu: 2\n")
	fmt.Fprintf(w, "    image: noble       # optional, Ubuntu image (noble/jammy/focal/bionic or 22.04, release:noble, daily:resolute); default LTS\n")
	fmt.Fprintf(w, "  model: <id>           # LLM, e.g. unsloth/Qwen3-0.6B-Q8_0 (downloaded on first run)\n")
	fmt.Fprintf(w, "  prompt: \"do stuff in the VM\"\n")
	fmt.Fprintf(w, "  allowed_commands:     # optional, restrict VM commands\n")
	fmt.Fprintf(w, "    - apt\n")
	fmt.Fprintf(w, "    - git\n")
	fmt.Fprintf(w, "    - python3\n")
	fmt.Fprintf(w, "  llm:                  # optional, LLM sampling params\n")
	fmt.Fprintf(w, "    temperature: 0.2\n")
	fmt.Fprintf(w, "    top_p: 0.95\n")
	fmt.Fprintf(w, "    top_k: 40\n")
	fmt.Fprintf(w, "    tool_choice: auto   # auto|required|none\n")
	fmt.Fprintf(w, "    context_window: 8192 # 0 = auto-tune\n")
	fmt.Fprintf(w, "  agent:                # optional, agent loop params\n")
	fmt.Fprintf(w, "    max_iterations: 10\n")
	fmt.Fprintf(w, "    chat_timeout: 300s\n")
	fmt.Fprintf(w, "    total_timeout: 30m\n\n")
	fmt.Fprintf(w, "Example:\n")
	fmt.Fprintf(w, "  mph -v ./mph.yaml\n")

}

func run() error {
	var (
		configPath  string
		showVersion bool
		verbose     bool
		pretty      bool
		needHelp    bool
	)

	flag.StringVar(&configPath, "config", "", "path to yaml config file (or positional arg)")
	flag.StringVar(&configPath, "c", "", "path to yaml config file (shorthand)")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.BoolVar(&verbose, "verbose", false, "verbose logging (debug)")
	flag.BoolVar(&verbose, "v", false, "verbose logging (debug) shorthand")
	flag.BoolVar(&pretty, "pretty", false, "pretty-print logs (human-readable console) instead of JSON")
	flag.BoolVar(&pretty, "prettyprint", false, "alias for --pretty (pretty-print logs)")
	flag.BoolVar(&needHelp, "help", false, "show help")
	flag.BoolVar(&needHelp, "h", false, "show help (shorthand)")

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

	log.Info().
		Str("config", configPath).
		Str("vm", cfg.VM.Name).
		Str("model", cfg.Model).
		Strs("allowed_commands", cfg.AllowedCommandsList()).
		Float64("llm.temperature", cfg.LLM.Temperature).
		Float64("llm.top_p", cfg.LLM.TopP).
		Int("llm.top_k", cfg.LLM.TopK).
		Str("llm.tool_choice", cfg.LLM.ToolChoice).
		Int("llm.context_window", cfg.LLM.ContextWindow).
		Int("agent.max_iterations", cfg.Agent.MaxIterations).
		Str("agent.chat_timeout", time.Duration(cfg.Agent.ChatTimeout).String()).
		Str("agent.total_timeout", time.Duration(cfg.Agent.TotalTimeout).String()).
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

	if err := agt.Execute(cfg); err != nil {
		return err
	}

	return nil
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
