package harness

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

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
	_, infoErr := client.Info(ctx, cfg.VM.Name)
	switch {
	case errors.Is(infoErr, multipass.ErrInstanceNotFound):
		if err := client.Launch(ctx, cfg.VM); err != nil {
			return err
		}
	case infoErr != nil:
		return fmt.Errorf("failed to check if VM exists: %w", infoErr)
	case !ignoreExisting:
		for {
			err := determineIfExistingVMIsOK(cfg.VM.Name)
			if err == nil {
				return client.Launch(ctx, cfg.VM)

			}

			if errors.Is(err, errInvalidInput) {
				continue
			}

			return err
		}
	}

	if err := agt.Execute(cfg, client); err != nil {
		return err
	}

	if !keep {
		return client.Delete(ctx, cfg.VM.Name, true)
	}

	return nil
}
