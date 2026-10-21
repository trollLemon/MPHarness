package harness

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/trollLemon/MPHarness/internal/agent"
	"github.com/trollLemon/MPHarness/internal/config"
	"github.com/trollLemon/MPHarness/internal/lxd"
	"github.com/trollLemon/MPHarness/internal/validation"
)

var (
	errInvalidInput      = errors.New("invalid input")
	errVMShouldNotBeUsed = errors.New("VM already exists and shouldn't be used")
	errPromptRead        = errors.New("could not read answer")
)

func outputRunDirCommand(runID string) string {
	// mkdir only: wiping the base dir here would delete live captures of
	// concurrent runs sharing one VM.
	return fmt.Sprintf("mkdir -p %q", config.OutputRunDir(runID))
}

func promptExistingVM(r *bufio.Reader, w io.Writer, name string) error {
	_, _ = fmt.Fprintf(w, "VM with name %s already exists, continue with the current VM? [y/n] ", name)

	input, err := r.ReadString('\n')
	if err != nil {
		if !errors.Is(err, io.EOF) {
			return fmt.Errorf("%w: %w", errPromptRead, err)
		}
		if strings.TrimSpace(input) == "" {
			return fmt.Errorf("%w: no answer", errPromptRead)
		}
	}

	switch strings.TrimSpace(strings.ToLower(input)) {
	case "y", "yes":
		return nil
	case "n", "no":
		return errVMShouldNotBeUsed
	default:
		return errInvalidInput
	}
}

// Options carries the run flags that change VM lifecycle behaviour.
type Options struct {
	IgnoreExisting bool
	Keep           bool
	RunID          string
}

func Start(ctx context.Context, agt *agent.Agent, client *lxd.Client, cfg config.Config, opts Options) error {
	_, infoErr := client.Info(ctx, cfg.VM.Name)
	existing := infoErr == nil
	switch {
	case errors.Is(infoErr, lxd.ErrInstanceNotFound):
		if err := client.Launch(ctx, cfg.VM); err != nil {
			return err
		}

		if strings.TrimSpace(cfg.VM.Kernel) != "" {
			if err := client.InstallKernel(ctx, cfg.VM.Name, cfg.VM.Kernel); err != nil {
				return err
			}
		}
	case infoErr != nil:
		return fmt.Errorf("failed to check if VM exists: %w", infoErr)
	case !opts.IgnoreExisting:
		reader := bufio.NewReader(os.Stdin)
		for {
			err := promptExistingVM(reader, os.Stdout, cfg.VM.Name)
			if err == nil {
				break
			}

			if errors.Is(err, errInvalidInput) {
				continue
			}

			return err
		}
	}

	if existing {
		if err := client.EnsureRunning(ctx, cfg.VM.Name); err != nil {
			return err
		}
	}

	if rid := strings.TrimSpace(opts.RunID); rid != "" {
		if _, err := client.Exec(ctx, cfg.VM.Name, outputRunDirCommand(rid)); err != nil {
			return fmt.Errorf("output run dir setup failed: %w", err)
		}
	}

	if strings.TrimSpace(cfg.ContentDir) != "" {
		if findings, scanErr := validation.ScanContentDir(cfg.ContentDir); scanErr != nil {
			return fmt.Errorf("content_dir scan failed: %w", scanErr)
		} else if len(findings) > 0 {
			for _, f := range findings {
				log.Warn().Str("file", f.File).Str("pattern", f.Pattern).Str("snippet", f.Snippet).Msg("content_dir prompt injection scan: suspicious pattern detected")
			}
		}

		if _, err := client.Exec(ctx, cfg.VM.Name, fmt.Sprintf("mkdir -p %q", config.VMContentDir)); err != nil {
			return err
		}
		if err := client.Transfer(ctx, cfg.VM.Name, cfg.ContentDir, config.VMContentDir); err != nil {
			return err
		}
	}

	if err := agt.Execute(ctx, cfg, client); err != nil {
		return err
	}

	if !opts.Keep {
		if err := client.Delete(ctx, cfg.VM.Name, true); err != nil {
			return err
		}
	}

	return nil
}
