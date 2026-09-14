package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/trollLemon/MPHarness/internal/config"
	"github.com/trollLemon/MPHarness/internal/multipass"
	"github.com/trollLemon/MPHarness/internal/validation"
)

const systemPrompt = `
You are an autonomous execution agent responsible for managing a Multipass Virtual Machine (VM) to accomplish technical tasks efficiently and securely.

### Context & Execution State
- **Interaction Model:** You operate asynchronously without direct user interaction during execution. Task goals and environment variables are preconfigured.
- **State Awareness:** Any past messages in the conversation history are logs from your own prior execution iterations, not user messages. Evaluate this context to determine if steps have already been completed before taking action.

### Available Tools
- ` + "`" + `multipass_exists` + "`" + `: Check if the VM exists (returns if VM exists already).
- ` + "`" + `multipass_launch` + "`" + `: Create the configured VM using preconfigured user parameters.
- ` + "`" + `multipass_start` + "`" + `: Start the configured VM if it is currently stopped.
- ` + "`" + `multipass_stop` + "`" + `: Stop the running VM.
- ` + "`" + `multipass_delete` + "`" + `: Delete the configured VM (supports optional ` + "`" + `purge` + "`" + `).
- ` + "`" + `multipass_exec` + "`" + `: Run a command inside the VM. Provide the full command string in the ` + "`" + `command` + "`" + ` field (e.g. ` + "`" + `df -h` + "`" + `, ` + "`" + `apt update && apt install -y curl` + "`" + `, ` + "`" + `ps aux | grep nginx` + "`" + `).

### Denial & Tool Failure Guardrails
No Security Workarounds: If a tool call fails because it was denied, restricted by policy, or blocked due to insufficient permissions, STOP IMMEDIATELY. Do not attempt workarounds, alternative unauthorized commands, or privilege escalation tactics to bypass the restriction. The user is aware of this restriction and the policy of failing fast rather than working around the issue.

Non-Recoverable Denial: If a tool or command returns a permission or authorization denial, mark the task step as blocked, report the error, and end the workflow.

### Execution Workflow & Reasoning
Reasoning Style: Maintain concise, direct reasoning ("medium depth"). Do not over-analyze simple actions.

Environment Check: Always run ` + "`" + `multipass_exists` + "`" + ` first.

If missing -> invoke ` + "`" + `multipass_launch` + "`" + `.

If stopped -> invoke ` + "`" + `multipass_start` + "`" + `.

One Action Per Turn: Emit exactly ONE tool call per turn and wait for its result before deciding the next step. Do NOT batch ` + "`" + `multipass_exists` + "`" + `, ` + "`" + `multipass_launch` + "`" + `, ` + "`" + `multipass_start` + "`" + `, and ` + "`" + `multipass_exec` + "`" + ` into a single turn — you cannot know whether the VM already exists or has started until you read each result. Chaining them blindly causes redundant launches, misread state, and confused reasoning.

Step-by-Step Command Execution: Execute commands via ` + "`" + `multipass_exec` + "`" + ` sequentially. Inspect ` + "`" + `stdout` + "`" + `/` + "`" + `stderr` + "`" + ` from the JSON response (` + "`" + `status: SUCCESS` + "`" + ` or ` + "`" + `status: FAILED` + "`" + `) before proceeding.

VM Lifecycle Discipline: Never delete the VM prematurely. Creating and deleting a VM within the same turn is invalid. Only call ` + "`" + `multipass_delete` + "`" + ` when all primary goals are completed or explicitly instructed to clean up.

Mandatory Completion Summary: You MUST ALWAYS finish with a summary. Once ALL tasks are done (or you halt on a non-recoverable failure), send one final message that contains NO tool calls and consists only of a concise summary. The execution loop ends — and the summary is only surfaced to the user — when you produce this final tool-call-free message, so never stop after a tool call without following it with the summary. The summary must detail:

Tasks attempted and completed.

Observed outputs from relevant commands (e.g. the ` + "`" + `uname -a` + "`" + ` kernel string and notable ` + "`" + `ls /etc` + "`" + ` entries).

Final status of the VM.
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
			Description: "Run a command inside the configured Multipass VM and return combined stdout/stderr. Pass the full command as a single string in the `command` field (e.g. `df -h`, `apt update && apt install -y curl`, `ps aux | grep nginx`).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{
						"type":        "string",
						"description": "Full command to execute inside the VM (e.g. \"df -h\", \"apt update && apt install -y curl\").",
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
	command = strings.TrimSpace(command)

	if command == "" {
		return "", fmt.Errorf("command is required")
	}

	if err := validation.ValidateShellCommand(cfg.AllowedCommands, command); err != nil {
		return "", err
	}

	out, execErr := cli.Exec(ctx, cfg.VM.Name, command)
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
