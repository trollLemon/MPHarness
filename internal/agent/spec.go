package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/trollLemon/MPHarness/internal/config"
	"github.com/trollLemon/MPHarness/internal/multipass"
)

const systemPrompt = `You are an autonomous agent that manages a Multipass VM to complete user tasks.

You have access to tools for Multipass VM lifecycle and command execution:
- multipass_exists: check whether the configured VM exists (running or stopped)
- multipass_launch: create the configured VM. This tool will create the VM with the users desired parameters, you do not need to input them yourself. 
- multipass_exec: run a shell command inside the VM (requires command, optional args) and return combined stdout/stderr
- multipass_start: start the configured VM if stopped
- multipass_stop: stop the running VM
- multipass_delete: delete the configured VM (optional purge)

Guidelines:
- Always check if the VM exists before launching. If it exists and is stopped, start it before executing commands.
- Use multipass_exec to run commands one at a time and inspect output before proceeding.
- Think step-by-step: plan, call tools, observe results, then continue.
- When a tool succeeds you will receive JSON with status SUCCESS and data; when it fails you will receive status FAILED with an error. Use that to decide next steps.
- After completing the task, summarize what you did and the observed command outputs.
- Prefer small, verifiable steps. If a command fails, explain why and try an alternative if appropriate.

Reasoning: high
`

// Spec describes one tool's calling contract in a transport-neutral shape:
// callers (e.g. the kronk chat client adapter) translate this into whatever
// tool-document format the model backend expects.
type Spec struct {
	Name        string
	Description string
	// Parameters is a JSON-schema object document, e.g.
	// {"type":"object","properties":{...},"required":[...]}.
	Parameters map[string]any
}

// Specs returns the tool set exposed to the agent for interacting with
// Multipass via internal/multipass.Client.
func Specs() []Spec {
	return []Spec{
		{
			Name:        "multipass_exists",
			Description: "Check whether the configured Multipass VM instance exists (whether running or stopped).",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "multipass_launch",
			Description: "Launch (create) the configured Multipass VM. Fails if a VM with the same name already exists.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "multipass_exec",
			Description: "Run a command inside the configured Multipass VM and return combined stdout/stderr.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{
						"type":        "string",
						"description": "Binary or command to execute inside the VM (e.g. \"ls\", \"bash\").",
					},
					"args": map[string]any{
						"type":        "array",
						"description": "Optional arguments for the command.",
						"items": map[string]any{
							"type": "string",
						},
					},
				},
				"required": []string{"command"},
			},
		},
		{
			Name:        "multipass_stop",
			Description: "Stop the configured Multipass VM without deleting it.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "multipass_start",
			Description: "Start the configured Multipass VM.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "multipass_delete",
			Description: "Delete the configured Multipass VM instance.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"purge": map[string]any{
						"type":        "boolean",
						"description": "If true, purge the instance storage immediately (equivalent to --purge). Defaults to false.",
					},
				},
			},
		},
	}
}

// Call dispatches one tool invocation by name against the Multipass client,
// returning the result text a model should see next.
func Call(ctx context.Context, cli *multipass.Client, cfg config.Config, name string, args map[string]any) (string, error) {
	switch name {
	case "multipass_exists":
		return callExists(ctx, cli, cfg)
	case "multipass_launch":
		return callLaunch(ctx, cli, cfg)
	case "multipass_exec":
		return callExec(ctx, cli, cfg, args)
	case "multipass_stop":
		return callStop(ctx, cli, cfg)
	case "multipass_start":
		return callStart(ctx, cli, cfg)
	case "multipass_delete":
		return callDelete(ctx, cli, cfg, args)
	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
}

func callExists(ctx context.Context, cli *multipass.Client, cfg config.Config) (string, error) {
	exists, err := cli.Exists(ctx, cfg.VM.Name)
	if err != nil {
		return "", err
	}
	if exists {
		return fmt.Sprintf("VM %q exists: true", cfg.VM.Name), nil
	}
	return fmt.Sprintf("VM %q exists: false", cfg.VM.Name), nil
}

func callLaunch(ctx context.Context, cli *multipass.Client, cfg config.Config) (string, error) {
	vm := cfg.VM
	if err := cli.Launch(ctx, vm); err != nil {
		return "", err
	}
	return fmt.Sprintf("VM %q launched (cpus=%d memory=%s disk=%s)", vm.Name, vm.CPU, vm.RAM, vm.Disk), nil
}

func callExec(ctx context.Context, cli *multipass.Client, cfg config.Config, args map[string]any) (string, error) {
	command, _ := args["command"].(string)

	if !cfg.IsCommandAllowed(command) {
		return "", errors.New("Command not in allowed list")
	}

	var execArgs []string
	if raw, ok := args["args"]; ok && raw != nil {
		switch v := raw.(type) {
		case []string:
			execArgs = v
		case []any:
			for _, e := range v {
				if s, ok := e.(string); ok {
					execArgs = append(execArgs, s)
				}
			}
		case string:
			execArgs = []string{v}
		}
	}
	out, execErr := cli.Exec(ctx, cfg.VM.Name, command, execArgs...)
	if execErr != nil {
		return "", execErr
	}
	if out == "" {
		return "(no output)", nil
	}
	return out, nil
}

func callStop(ctx context.Context, cli *multipass.Client, cfg config.Config) (string, error) {
	if err := cli.Stop(ctx, cfg.VM.Name); err != nil {
		return "", err
	}
	return fmt.Sprintf("VM %q stopped", cfg.VM.Name), nil
}

func callStart(ctx context.Context, cli *multipass.Client, cfg config.Config) (string, error) {
	if err := cli.Start(ctx, cfg.VM.Name); err != nil {
		return "", err
	}
	return fmt.Sprintf("VM %q started", cfg.VM.Name), nil
}

func callDelete(ctx context.Context, cli *multipass.Client, cfg config.Config, args map[string]any) (string, error) {
	purge, _ := args["purge"].(bool)
	if err := cli.Delete(ctx, cfg.VM.Name, purge); err != nil {
		return "", err
	}
	if purge {
		return fmt.Sprintf("VM %q deleted (purged)", cfg.VM.Name), nil
	}
	return fmt.Sprintf("VM %q deleted", cfg.VM.Name), nil
}
