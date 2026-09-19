package multipass

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/rs/zerolog"
	"github.com/trollLemon/MPHarness/internal/config"
)

const (
	MultipassBin = "multipass"
)

var (
	ErrFailedToLaunch   = errors.New("VM failed to launch")
	ErrInstanceNotFound = errors.New("VM instance not found")
)

// Client is a thin wrapper around the multipass cli application.
type Client struct {
	log zerolog.Logger
	bin string
}

func New(log zerolog.Logger) *Client {
	return &Client{
		log: log.With().Str("component", MultipassBin).Logger(),
		bin: MultipassBin,
	}
}

// Info runs `multipass info --format json` for the named VM and returns the
// raw JSON response emitted by multipass.
func (c *Client) Info(ctx context.Context, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("instance name is required")
	}

	c.log.Info().Str("vm", name).Msg("fetching VM info")

	cmd := exec.CommandContext(ctx, c.bin, "info", name, "--format", "json")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		c.log.Debug().Err(err).Str("vm", name).Str("output", msg).Msg("multipass info failed")
		if isInstanceNotFound(msg) {
			return "", fmt.Errorf("%w: %s", ErrInstanceNotFound, name)
		}
		return "", fmt.Errorf("multipass info %q failed: %w: %s", name, err, msg)
	}

	return string(out), nil
}

// isInstanceNotFound reports whether a multipass info error message indicates
// that the named instance does not exist.
func isInstanceNotFound(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "does not exist")
}

// Launch creates a multipas VM if one doesn't exist already.
func (c *Client) Launch(ctx context.Context, vm config.VMConfig) error {
	c.log.Info().Msgf("Creating VM %s", vm.Name)

	args := []string{
		"launch",
		"--name", vm.Name,
		"--cpus", fmt.Sprintf("%d", vm.CPU),
		"--memory", vm.RAM,
		"--disk", vm.Disk,
	}
	if img := strings.TrimSpace(vm.Image); img != "" {
		args = append(args, img)
	}

	cmd := exec.CommandContext(ctx, c.bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		c.log.Debug().Err(err).Str("vm", vm.Name).Str("output", msg).Msg("failed to launch VM")
		return fmt.Errorf("%w: %s", ErrFailedToLaunch, msg)
	}

	c.log.Info().Str("vm", vm.Name).Msg("VM launched")
	return nil
}

// Exec runs a command inside the multipass VM and returns the combined stdout and stderr.
func (c *Client) Exec(ctx context.Context, name string, command string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("instance name is required")
	}
	if strings.TrimSpace(command) == "" {
		return "", fmt.Errorf("command is required")
	}

	execArgs := []string{"exec", name, "--", "bash", "-c", command}

	c.log.Info().Msgf("Running %s", command)

	cmd := exec.CommandContext(ctx, c.bin, execArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		c.log.Debug().Err(err).Str("vm", name).Str("command", command).Str("args", strings.Join(execArgs, ",")).Msg("multipass exec failed")
		return "", fmt.Errorf("multipass exec %q %q failed: %w: %s", name, command, err, msg)
	}

	return string(out), nil
}

// Delete deletes a multipass VM
func (c *Client) Delete(ctx context.Context, name string, purge bool) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("instance name is required")
	}

	c.log.Info().Msgf("Deleting VM %s", name)

	args := []string{"delete"}
	if purge {
		args = append(args, "--purge")
	}
	args = append(args, name)

	cmd := exec.CommandContext(ctx, c.bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		c.log.Debug().Err(err).Str("vm", name).Bool("purge", purge).Str("output", msg).Msg("multipass delete failed")
		return fmt.Errorf("multipass delete %q failed: %w: %s", name, err, msg)
	}

	c.log.Info().Str("vm", name).Bool("purge", purge).Msg("VM deleted")
	return nil
}

// Stop stops a multipass VM but doesn't delete it.
func (c *Client) Stop(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("instance name is required")
	}

	c.log.Info().Msgf("stopping vm %s", name)

	cmd := exec.CommandContext(ctx, c.bin, "stop", name)

	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		c.log.Debug().Err(err).Str("vm", name).Str("output", msg).Msg("multipass stop failed")
		return fmt.Errorf("multipass stop %q failed: %w: %s", name, err, msg)
	}

	c.log.Info().Str("vm", name).Msg("VM stopped")
	return nil
}

// Start starts a multipass vm
func (c *Client) Start(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("instance name is required")
	}

	c.log.Info().Msgf("starting vm %s", name)

	cmd := exec.CommandContext(ctx, c.bin, "start", name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		c.log.Debug().Err(err).Str("vm", name).Str("output", msg).Msg("multipass start failed")
		return fmt.Errorf("multipass start %q failed: %w: %s", name, err, msg)
	}

	c.log.Info().Str("vm", name).Msg("VM started")
	return nil
}
