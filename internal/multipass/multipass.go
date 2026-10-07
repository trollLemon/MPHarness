package multipass

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/rs/zerolog"
	"github.com/trollLemon/MPHarness/internal/config"
	mphotel "github.com/trollLemon/MPHarness/internal/otel"
	"github.com/trollLemon/MPHarness/internal/secrets"
	"github.com/trollLemon/MPHarness/internal/textutil"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	MultipassBin = "multipass"
)

var (
	ErrFailedToLaunch   = errors.New("VM failed to launch")
	ErrInstanceNotFound = errors.New("VM instance not found")
	ErrTransferFailed   = errors.New("transfer failed")
)

// Client is a thin wrapper around the multipass cli application.
type Client struct {
	log zerolog.Logger
	bin string
	// secrets resolves %{NAME} placeholders in commands. A nil Resolver
	// means no placeholder can be resolved, which leaves marker-free
	// commands behaving exactly as they did before this field existed.
	secrets secrets.Resolver
	// used holds every secret value resolved so far this run, so that no
	// exec can hand one back. It needs no synchronisation: the harness runs
	// tool calls sequentially and the set is per-Client, i.e. per-run.
	used map[string]string
}

func getTracer() trace.Tracer {
	return otel.Tracer("mph")
}

// scrubValues replaces each value in values with %{NAME}, longest value first
// so overlapping secrets cannot leave fragments behind.
func scrubValues(s string, values map[string]string) string {
	if s == "" || len(values) == 0 {
		return s
	}
	type pair struct {
		name, value string
	}
	pairs := make([]pair, 0, len(values))
	for name, value := range values {
		if value == "" {
			continue
		}
		pairs = append(pairs, pair{name: name, value: value})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if len(pairs[i].value) != len(pairs[j].value) {
			return len(pairs[i].value) > len(pairs[j].value)
		}
		return pairs[i].name < pairs[j].name
	})
	for _, p := range pairs {
		s = strings.ReplaceAll(s, p.value, "%{"+p.name+"}")
	}
	return s
}

// New returns a Client. A nil resolver disables placeholder resolution.
func New(log zerolog.Logger, resolver secrets.Resolver) *Client {
	return &Client{
		log:     log.With().Str("component", MultipassBin).Logger(),
		bin:     MultipassBin,
		secrets: resolver,
		used:    make(map[string]string),
	}
}

// Info runs `multipass info --format json` for the named VM and returns the
// raw JSON response emitted by multipass.
func (c *Client) Info(ctx context.Context, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("instance name is required")
	}

	ctx, span := getTracer().Start(ctx, "multipass.info", trace.WithAttributes(
		attribute.String("multipass.command", "info"),
		attribute.String("mph.vm.name", name),
	))
	defer span.End()

	c.log.Info().Str("vm", name).Msg("fetching VM info")

	cmd := exec.CommandContext(ctx, c.bin, "info", name, "--format", "json")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		c.log.Debug().Err(err).Str("vm", name).Str("out", msg).Msg("multipass info failed")
		mphotel.FailSpan(span, err)
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
	ctx, span := getTracer().Start(ctx, "multipass.launch", trace.WithAttributes(
		attribute.String("multipass.command", "launch"),
		attribute.String("mph.vm.name", vm.Name),
		attribute.String("mph.vm.cpu", fmt.Sprint(vm.CPU)),
		attribute.String("mph.vm.ram", vm.RAM),
		attribute.String("mph.vm.disk", vm.Disk),
		attribute.String("mph.vm.image", vm.Image),
	))
	defer span.End()

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
	span.SetAttributes(attribute.StringSlice("multipass.args", args))

	cmd := exec.CommandContext(ctx, c.bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		c.log.Debug().Err(err).Str("vm", vm.Name).Str("out", msg).Msg("failed to launch VM")
		mphotel.FailSpan(span, err)
		return fmt.Errorf("%w: %s", ErrFailedToLaunch, msg)
	}

	c.log.Info().Str("vm", vm.Name).Msg("VM launched")
	return nil
}

// Exec runs a command inside the multipass VM and returns the combined stdout and stderr.
// Exec runs a command in the VM. maxAttrBytes caps the command echoed into the
// multipass.exec span; a non-positive value means no cap.
func (c *Client) Exec(ctx context.Context, name string, command string, maxAttrBytes int) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("instance name is required")
	}
	if strings.TrimSpace(command) == "" {
		return "", fmt.Errorf("command is required")
	}

	ctx, span := getTracer().Start(ctx, "multipass.exec", trace.WithAttributes(
		attribute.String("multipass.command", "exec"),
		attribute.String("mph.vm.name", name),
		attribute.String("mph.command", textutil.Truncate(command, maxAttrBytes)),
	))
	defer span.End()

	resolved, values, err := secrets.Redact(command, c.secrets)
	if err != nil {
		c.log.Debug().Err(err).Str("vm", name).Str("cmd", command).Msg("multipass exec failed to resolve secrets")
		mphotel.FailSpan(span, err)
		return "", fmt.Errorf("multipass exec %q %q failed: %w", name, command, err)
	}
	if c.used == nil {
		c.used = make(map[string]string)
	}
	for k, v := range values {
		c.used[k] = v
	}

	execArgs := []string{"exec", name, "--", "bash", "-c", resolved}
	span.SetAttributes(attribute.StringSlice("multipass.args", []string{"exec", name, "--", "bash", "-c", command}))

	c.log.Debug().Str("vm", name).Str("cmd", command).Msgf("Running %s", command)

	cmd := exec.CommandContext(ctx, c.bin, execArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := scrubValues(strings.TrimSpace(string(out)), c.used)
		maskedArgs := []string{"exec", name, "--", "bash", "-c", command}
		c.log.Debug().Err(err).Str("vm", name).Str("cmd", command).Str("args", strings.Join(maskedArgs, ",")).Msg("multipass exec failed")
		mphotel.FailSpan(span, err)
		return "", fmt.Errorf("multipass exec %q %q failed: %w: %s", name, command, err, msg)
	}

	return scrubValues(string(out), c.used), nil
}

// Transfer copies a host directory recursively into the VM.
func (c *Client) Transfer(ctx context.Context, vmName, hostPath, vmDest string) error {
	if strings.TrimSpace(vmName) == "" {
		return fmt.Errorf("instance name is required")
	}
	if strings.TrimSpace(hostPath) == "" {
		return fmt.Errorf("host path is required")
	}
	if strings.TrimSpace(vmDest) == "" {
		return fmt.Errorf("destination is required")
	}

	ctx, span := getTracer().Start(ctx, "multipass.transfer", trace.WithAttributes(
		attribute.String("multipass.command", "transfer"),
		attribute.String("mph.vm.name", vmName),
		attribute.String("mph.transfer.source", hostPath),
		attribute.String("mph.transfer.destination", vmDest),
	))
	defer span.End()

	c.log.Info().Str("vm", vmName).Str("src", hostPath).Str("dst", vmDest).Msg("transferring content to VM")

	args := []string{"transfer", "--recursive", "--parents", hostPath, fmt.Sprintf("%s:%s", vmName, vmDest)}
	span.SetAttributes(attribute.StringSlice("multipass.args", args))

	cmd := exec.CommandContext(ctx, c.bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		c.log.Debug().Err(err).Str("vm", vmName).Str("src", hostPath).Str("dst", vmDest).Str("out", msg).Msg("multipass transfer failed")
		mphotel.FailSpan(span, err)
		return fmt.Errorf("%w: %s", ErrTransferFailed, msg)
	}

	c.log.Info().Str("vm", vmName).Str("src", hostPath).Str("dst", vmDest).Msg("transfer complete")
	return nil
}

// Delete deletes a multipass VM
func (c *Client) Delete(ctx context.Context, name string, purge bool) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("instance name is required")
	}

	ctx, span := getTracer().Start(ctx, "multipass.delete", trace.WithAttributes(
		attribute.String("multipass.command", "delete"),
		attribute.String("mph.vm.name", name),
		attribute.Bool("mph.purge", purge),
	))
	defer span.End()

	c.log.Info().Msgf("Deleting VM %s", name)

	args := []string{"delete"}
	if purge {
		args = append(args, "--purge")
	}
	args = append(args, name)
	span.SetAttributes(attribute.StringSlice("multipass.args", args))

	cmd := exec.CommandContext(ctx, c.bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		c.log.Debug().Err(err).Str("vm", name).Bool("purge", purge).Str("out", msg).Msg("multipass delete failed")
		mphotel.FailSpan(span, err)
		return fmt.Errorf("multipass delete %q failed: %w: %s", name, err, msg)
	}

	c.log.Info().Str("vm", name).Bool("purge", purge).Msg("VM deleted")
	return nil
}
