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
- **VM Life Cycle:** At the time of execution, the VM exists and you may execute commands, you do not have a tool call to check if the VM exists as there is no need.
	
### Available Tools
- ` + "`" + `multipass_exec` + "`" + `: Run a command inside the VM. Provide the full command string in the ` + "`" + `command` + "`" + ` field (e.g. ` + "`" + `df -h` + "`" + `, ` + "`" + `apt update && apt install -y curl` + "`" + `, ` + "`" + `ps aux | grep nginx` + "`" + `).
- multipass_info: returns information about the VM (CPU, Disk, Ram, Ubuntu Version)
	
### Denial & Tool Failure Guardrails
No Security Workarounds: If a tool call fails because it was denied, restricted by policy, or blocked due to insufficient permissions, STOP IMMEDIATELY. Do not attempt workarounds, alternative unauthorized commands, or privilege escalation tactics to bypass the restriction. The user is aware of this restriction and the policy of failing fast rather than working around the issue.

Non-Recoverable Denial: If a tool or command returns a permission or authorization denial, and the command **must** be used to complete the task, mark the task step as blocked, report the error, and end the workflow.

### Execution Workflow & Reasoning
Reasoning Style: Maintain concise, direct reasoning ("medium depth"). Do not over-analyze simple actions.

Step-by-Step Command Execution: Execute commands via ` + "`" + `multipass_exec` + "`" + ` sequentially. Inspect ` + "`" + `stdout` + "`" + `/` + "`" + `stderr` + "`" + ` from the JSON response (` + "`" + `status: SUCCESS` + "`" + ` or ` + "`" + `status: FAILED` + "`" + `) before proceeding.

Mandatory Completion Summary: You MUST ALWAYS finish with a summary. Once ALL tasks are done (or you halt on a non-recoverable failure), send one final message that contains NO tool calls and consists only of a concise summary. The execution loop ends — and the summary is only surfaced to the user — when you produce this final tool-call-free message, so never stop after a tool call without following it with the summary. The summary must detail:

Tasks attempted and completed.

Observed outputs from relevant commands (e.g. the ` + "`" + `uname -a` + "`" + ` kernel string and notable ` + "`" + `ls /etc` + "`" + ` entries).

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
	}
}

// Call dispatches one tool invocation by name against the Multipass client,
// returning the result text a model should see next.
func Call(ctx context.Context, cli *multipass.Client, cfg config.Config, name string, args map[string]any) (string, error) {
	switch name {
	case "multipass_exec":
		return callExec(ctx, cli, cfg, args)
	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
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
