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
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/trollLemon/MPHarness/internal/agent"
	"github.com/trollLemon/MPHarness/internal/config"
	"github.com/trollLemon/MPHarness/internal/multipass"
	mphotel "github.com/trollLemon/MPHarness/internal/otel"
	"github.com/trollLemon/MPHarness/internal/textutil"
	"github.com/trollLemon/MPHarness/internal/validation"
)

var (
	errInvalidInput      = errors.New("invalid input")
	errVMShouldNotBeUsed = errors.New("VM already exists and shouldn't be used")
	errPromptRead        = errors.New("could not read answer")
)

func outputRunDirCommands(runID string) []string {
	// mkdir only: wiping the base dir here would delete live captures of
	// concurrent runs sharing one VM.
	return []string{
		fmt.Sprintf("mkdir -p %q", config.OutputRunDir(runID)),
	}
}

func promptExistingVM(r *bufio.Reader, w io.Writer, name string) error {
	fmt.Fprintf(w, "VM with name %s already exists, continue with the current VM? [y/n] ", name)

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

func Start(ctx context.Context, agt *agent.Agent, client *multipass.Client, cfg config.Config, opts Options) error {
	tracer := otel.Tracer("mph")
	ctx, span := tracer.Start(ctx, "mph.run", trace.WithAttributes(
		attribute.String("mph.vm.name", cfg.VM.Name),
		attribute.String("mph.model", cfg.Model),
		attribute.String("mph.prompt", textutil.Truncate(cfg.Prompt, cfg.Truncation.LogContent)),
		attribute.Bool("mph.keep", opts.Keep),
		attribute.Bool("mph.ignore_existing", opts.IgnoreExisting),
		attribute.StringSlice("mph.allowed_commands", cfg.AllowedCommandsList()),
		attribute.String("mph.content_dir", cfg.ContentDir),
		attribute.String("mph.content.destination", config.VMContentDir),
	))
	defer span.End()

	_, infoErr := client.Info(ctx, cfg.VM.Name)
	switch {
	case errors.Is(infoErr, multipass.ErrInstanceNotFound):
		cCtx, cSpan := tracer.Start(ctx, "mph.vm.create", trace.WithAttributes(
			attribute.String("mph.vm.name", cfg.VM.Name),
			attribute.Int("mph.vm.cpu", cfg.VM.CPU),
			attribute.String("mph.vm.ram", cfg.VM.RAM),
			attribute.String("mph.vm.disk", cfg.VM.Disk),
			attribute.String("mph.vm.image", cfg.VM.Image),
		))
		err := client.Launch(cCtx, cfg.VM)
		if err != nil {
			mphotel.FailSpan(cSpan, err)
		}
		cSpan.End()
		if err != nil {
			mphotel.FailSpan(span, err)
			return err
		}
	case infoErr != nil:
		err := fmt.Errorf("failed to check if VM exists: %w", infoErr)
		mphotel.FailSpan(span, err)
		return err
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

			mphotel.FailSpan(span, err)
			return err
		}
	}

	if rid := strings.TrimSpace(opts.RunID); rid != "" {
		for _, cmd := range outputRunDirCommands(rid) {
			if _, err := client.Exec(ctx, cfg.VM.Name, cmd, cfg.Truncation.ToolResult); err != nil {
				err = fmt.Errorf("output run dir setup failed: %w", err)
				mphotel.FailSpan(span, err)
				return err
			}
		}
		span.SetAttributes(attribute.String("mph.output.dir", config.OutputRunDir(rid)))
	}

	if strings.TrimSpace(cfg.ContentDir) != "" {
		if findings, scanErr := validation.ScanContentDir(cfg.ContentDir); scanErr != nil {
			err := fmt.Errorf("content_dir scan failed: %w", scanErr)
			mphotel.FailSpan(span, err)
			return err
		} else if len(findings) > 0 {
			for _, f := range findings {
				log.Warn().Str("file", f.File).Str("pattern", f.Pattern).Str("snippet", f.Snippet).Msg("content_dir prompt injection scan: suspicious pattern detected")
				span.AddEvent("mph.content.scan_finding", trace.WithAttributes(
					attribute.String("mph.content.file", f.File),
					attribute.String("mph.content.pattern", f.Pattern),
					attribute.String("mph.content.snippet", f.Snippet),
				))
			}
			span.SetAttributes(attribute.Int("mph.content.scan_findings", len(findings)))
		}

		cCtx, cSpan := tracer.Start(ctx, "mph.vm.content", trace.WithAttributes(
			attribute.String("mph.vm.name", cfg.VM.Name),
			attribute.String("mph.content.source", cfg.ContentDir),
			attribute.String("mph.content.destination", config.VMContentDir),
		))
		if _, err := client.Exec(cCtx, cfg.VM.Name, fmt.Sprintf("mkdir -p %q", config.VMContentDir), cfg.Truncation.ToolResult); err != nil {
			mphotel.FailSpan(cSpan, err)
			cSpan.End()
			mphotel.FailSpan(span, err)
			return err
		}
		if err := client.Transfer(cCtx, cfg.VM.Name, cfg.ContentDir, config.VMContentDir); err != nil {
			mphotel.FailSpan(cSpan, err)
			cSpan.End()
			mphotel.FailSpan(span, err)
			return err
		}
		cSpan.End()
	}

	if err := agt.Execute(ctx, cfg, client); err != nil {
		mphotel.FailSpan(span, err)
		return err
	}

	if !opts.Keep {
		cCtx, cSpan := tracer.Start(ctx, "mph.vm.delete", trace.WithAttributes(
			attribute.String("mph.vm.name", cfg.VM.Name),
			attribute.Bool("mph.purge", true),
		))
		err := client.Delete(cCtx, cfg.VM.Name, true)
		if err != nil {
			mphotel.FailSpan(cSpan, err)
		}
		cSpan.End()
		if err != nil {
			mphotel.FailSpan(span, err)
			return err
		}
	}

	return nil
}
