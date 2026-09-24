package harness

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/trollLemon/MPHarness/internal/agent"
	"github.com/trollLemon/MPHarness/internal/config"
	"github.com/trollLemon/MPHarness/internal/multipass"
)

var (
	errInvalidInput      = errors.New("invalid input")
	errVMShouldNotBeUsed = errors.New("VM already exists and shouldn't be used")
)

func determineIfExistingVMIsOK(name string) error {
	reader := bufio.NewReader(os.Stdin)

	fmt.Printf("VM with name %s already exists, continue with the current VM? [y/n] ", name)

	input, err := reader.ReadString('\n')
	if err != nil {
		fmt.Println("Error reading input:", err)
		return nil
	}

	input = strings.TrimSpace(strings.ToLower(input))

	switch input {
	case "y", "yes":
		return nil
	case "n", "no":
		return errVMShouldNotBeUsed
	default:
		return errInvalidInput
	}
}

func Start(ctx context.Context, agt *agent.Agent, client *multipass.Client, cfg config.Config, ignoreExisting, keep bool) error {
	tracer := otel.Tracer("mph")
	ctx, span := tracer.Start(ctx, "mph.run", trace.WithAttributes(
		attribute.String("mph.vm.name", cfg.VM.Name),
		attribute.String("mph.model", cfg.Model),
		attribute.String("mph.prompt", truncate(cfg.Prompt, 8000)),
		attribute.Bool("mph.keep", keep),
		attribute.Bool("mph.ignore_existing", ignoreExisting),
		attribute.StringSlice("mph.allowed_commands", cfg.AllowedCommandsList()),
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
			cSpan.RecordError(err)
			cSpan.SetStatus(codes.Error, err.Error())
		}
		cSpan.End()
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return err
		}
	case infoErr != nil:
		err := fmt.Errorf("failed to check if VM exists: %w", infoErr)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	case !ignoreExisting:
		for {
			err := determineIfExistingVMIsOK(cfg.VM.Name)
			if err == nil {
				break
			}

			if errors.Is(err, errInvalidInput) {
				continue
			}

			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return err
		}
	}

	if err := agt.Execute(ctx, cfg, client); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	if !keep {
		cCtx, cSpan := tracer.Start(ctx, "mph.vm.delete", trace.WithAttributes(
			attribute.String("mph.vm.name", cfg.VM.Name),
			attribute.Bool("mph.purge", true),
		))
		err := client.Delete(cCtx, cfg.VM.Name, true)
		if err != nil {
			cSpan.RecordError(err)
			cSpan.SetStatus(codes.Error, err.Error())
		}
		cSpan.End()
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return err
		}
	}

	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}
