package harness

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/trollLemon/MPHarness/v2/internal/agent"
	"github.com/trollLemon/MPHarness/v2/internal/config"
	"github.com/trollLemon/MPHarness/v2/internal/lxd"
	"github.com/trollLemon/MPHarness/v2/internal/validation"
)

const commandHistoryFlushTimeout = 30 * time.Second

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
	CmdOutput      io.Writer // optional JSONL command history destination
}

type commandHistoryWriter interface {
	WriteCommandHistory(ctx context.Context, w io.Writer) error
}

// writeCommandHistory writes tracked commands to w using a bounded context
// detached from the run context, so a timed-out run still flushes.
func writeCommandHistory(ctx context.Context, hist commandHistoryWriter, w io.Writer) error {
	if w == nil {
		return nil
	}
	flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commandHistoryFlushTimeout)
	defer cancel()
	return hist.WriteCommandHistory(flushCtx, w)
}

func scanSkillsDir(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	findings, err := validation.ScanContentDir(dir)
	if err != nil {
		return err
	}
	for _, f := range findings {
		log.Warn().Str("file", f.File).Str("pattern", f.Pattern).Str("snippet", f.Snippet).Msg("skills_dir prompt injection scan: suspicious pattern detected")
	}
	return nil
}

func Start(ctx context.Context, agt *agent.Agent, client *lxd.Client, cfg config.Config, opts Options) (*agent.TaskStatusOutcome, error) {
	_, infoErr := client.Info(ctx, cfg.VM.Name)
	existing := infoErr == nil
	switch {
	case errors.Is(infoErr, lxd.ErrInstanceNotFound):
		if err := client.Launch(ctx, cfg.VM); err != nil {
			return nil, err
		}

		if strings.TrimSpace(cfg.VM.Kernel) != "" {
			if err := client.InstallKernel(ctx, cfg.VM.Name, cfg.VM.Kernel); err != nil {
				return nil, err
			}
		}
	case infoErr != nil:
		return nil, fmt.Errorf("failed to check if VM exists: %w", infoErr)
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

			return nil, err
		}
	}

	if existing {
		if err := client.EnsureRunning(ctx, cfg.VM.Name); err != nil {
			return nil, err
		}
	}

	if rid := strings.TrimSpace(opts.RunID); rid != "" {
		if _, err := client.Exec(ctx, cfg.VM.Name, outputRunDirCommand(rid)); err != nil {
			return nil, fmt.Errorf("output run dir setup failed: %w", err)
		}
	}

	if err := scanSkillsDir(cfg.SkillsDir); err != nil {
		return nil, fmt.Errorf("skills_dir scan failed: %w", err)
	}

	if strings.TrimSpace(cfg.ContentDir) != "" {
		if findings, scanErr := validation.ScanContentDir(cfg.ContentDir); scanErr != nil {
			return nil, fmt.Errorf("content_dir scan failed: %w", scanErr)
		} else if len(findings) > 0 {
			for _, f := range findings {
				log.Warn().Str("file", f.File).Str("pattern", f.Pattern).Str("snippet", f.Snippet).Msg("content_dir prompt injection scan: suspicious pattern detected")
			}
		}

		if _, err := client.Exec(ctx, cfg.VM.Name, fmt.Sprintf("mkdir -p %q", config.VMContentDir)); err != nil {
			return nil, err
		}
		if err := client.Transfer(ctx, cfg.VM.Name, cfg.ContentDir, config.VMContentDir); err != nil {
			return nil, err
		}
	}

	outcome, err := agt.Execute(ctx, cfg, client)
	if opts.CmdOutput != nil {
		if err := writeCommandHistory(ctx, client, opts.CmdOutput); err != nil {
			log.Warn().Err(err).Msg("write command history failed")
		}
	}
	if err != nil {
		return nil, err
	}

	if !opts.Keep {
		if err := client.Delete(ctx, cfg.VM.Name, true); err != nil {
			return nil, err
		}
	}

	return outcome, nil
}
