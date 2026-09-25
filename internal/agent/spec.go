package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ardanlabs/kronk/sdk/kronk/model"
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
- ` + "`" + `multipass_info` + "`" + `: Fetch current VM information as the raw JSON payload from ` + "`" + `multipass info --format json` + "`" + ` (zone, state, release, image_release, cpu_count, load, memory and disk usage, ipv4, mounts). Takes no arguments. Use this to check resource headroom before running heavy commands.
	
### Denial & Tool Failure Guardrails
No Security Workarounds: If a tool call fails because it was denied, restricted by policy, or blocked due to insufficient permissions, STOP IMMEDIATELY. Do not attempt workarounds, alternative unauthorized commands, or privilege escalation tactics to bypass the restriction. The user is aware of this restriction and the policy of failing fast rather than working around the issue.

Non-Recoverable Denial: If a tool or command returns a permission or authorization denial, and the command **must** be used to complete the task, mark the task step as blocked, report the error, and end the workflow.

### Execution Workflow & Reasoning
Reasoning Style: Maintain concise, direct reasoning ("medium depth"). Do not over-analyze simple actions.

Step-by-Step Command Execution: Execute commands via ` + "`" + `multipass_exec` + "`" + ` sequentially. Inspect ` + "`" + `stdout` + "`" + `/` + "`" + `stderr` + "`" + ` from the JSON response (` + "`" + `status: SUCCESS` + "`" + ` or ` + "`" + `status: FAILED` + "`" + `) before proceeding.

Mandatory Completion Summary: You MUST ALWAYS finish with a summary. Once ALL tasks are done (or you halt on a non-recoverable failure), send one final message that contains NO tool calls and consists only of a concise summary. The execution loop ends when you produce this final tool-call-free message. Only then is the summary surfaced to the user, so never stop after a tool call without following it with the summary. The summary must detail:

Tasks attempted and completed.

Observed outputs from relevant commands (e.g. the ` + "`" + `uname -a` + "`" + ` kernel string and notable ` + "`" + `ls /etc` + "`" + ` entries).

`

const contentPromptSuffix = `
### Additional Content
The user has provided a local directory whose contents have been copied to ` + config.VMContentDir + ` in the VM before execution. This content contains files needed to complete the task. Inspect it as needed (e.g. ` + "`" + `ls -R ` + config.VMContentDir + "`" + `).
`

func buildSystemPrompt(cfg config.Config) string {
	if strings.TrimSpace(cfg.ContentDir) != "" {
		return systemPrompt + contentPromptSuffix
	}
	return systemPrompt
}

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
		{
			Name:        "multipass_info",
			Description: "Returns the raw JSON payload from `multipass info --format json` for the configured VM: zone, state, release, image_release, cpu_count, load, memory and disk usage, ipv4, mounts. Takes no arguments. Use this to inspect configured limits and headroom before running heavy commands.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
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
	case "multipass_info":
		return callInfo(ctx, cli, cfg)
	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
}

func callInfo(ctx context.Context, cli *multipass.Client, cfg config.Config) (string, error) {
	raw, err := cli.Info(ctx, cfg.VM.Name)
	if err != nil {
		return "", err
	}
	return raw, nil
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

	out, execErr := cli.Exec(ctx, cfg.VM.Name, command, cfg.Truncation.ToolResult)
	if execErr != nil {
		return "", execErr
	}
	if out == "" {
		return "(no output)", nil
	}
	return out, nil
}

func buildToolErrorContent(err error) string {
	b, _ := json.Marshal(map[string]any{
		"status": "FAILED",
		"data":   map[string]any{"error": err.Error()},
	})
	return string(b)
}

func buildToolSuccessContent(toolName, result string) string {
	var payload map[string]any
	if toolName == "multipass_exec" {
		payload = map[string]any{
			"status": "SUCCESS",
			"data":   map[string]any{"output": result},
		}
	} else {
		payload = map[string]any{
			"status": "SUCCESS",
			"data":   map[string]any{"result": result},
		}
	}
	b, _ := json.Marshal(payload)
	return string(b)
}

func buildToolDocuments() []model.D {
	specs := Specs()
	docs := make([]model.D, 0, len(specs))
	for _, s := range specs {
		docs = append(docs, model.D{
			"type": "function",
			"function": model.D{
				"name":        s.Name,
				"description": s.Description,
				"parameters":  s.Parameters,
			},
		})
	}
	return docs
}

func buildUserPrompt(conf config.Config) string {
	var b strings.Builder
	if len(conf.AllowedCommands) > 0 {
		fmt.Fprintf(&b, "Allowed commands: %s\n\n", strings.Join(conf.AllowedCommandsList(), ", "))
	} else {
		b.WriteString("Allowed commands: (all)\n\n")
	}
	b.WriteString("Task:\n")
	b.WriteString(strings.TrimSpace(conf.Prompt))
	b.WriteString("\n")
	return b.String()
}

func buildAssistantMessage(msg *model.ResponseMessage, toolCallDocs []model.D) model.D {
	assistantMsg := model.D{
		"role":       "assistant",
		"tool_calls": toolCallDocs,
	}
	if msg.Content != "" {
		assistantMsg["content"] = msg.Content
	}
	if msg.Reasoning != "" {
		assistantMsg["reasoning_content"] = msg.Reasoning
	}
	return assistantMsg
}

func buildToolResponseMessage(tc model.ResponseToolCall, content string) model.D {
	return model.D{
		"role":         "tool",
		"tool_call_id": tc.ID,
		"name":         tc.Function.Name,
		"content":      content,
	}
}

func appendToConversation(conversation []model.D, msgs ...model.D) []model.D {
	return append(conversation, msgs...)
}

// buildLengthNudgeMessages rebuilds the truncated assistant turn plus the user
// nudge injected when a reply hit the length limit without a tool call.
func buildLengthNudgeMessages(msg *model.ResponseMessage, maxContent int) []model.D {
	msgs := make([]model.D, 0, 2)
	if msg.Reasoning != "" || msg.Content != "" {
		nudgedAssistant := model.D{"role": "assistant"}
		if msg.Content != "" {
			nudgedAssistant["content"] = truncate(msg.Content, maxContent)
		}
		if msg.Reasoning != "" {
			nudgedAssistant["reasoning_content"] = truncate(msg.Reasoning, maxContent)
		}
		msgs = append(msgs, nudgedAssistant)
	}
	return append(msgs, model.D{
		"role":    "user",
		"content": "Your previous response hit the token limit without making a tool call. Please be concise and try again and run a tool call if necessary",
	})
}

func buildChatRequest(conversation, toolDocs []model.D, llmConfig LLMConfig) model.D {
	topK := llmConfig.TopK
	if topK == 0 {
		topK = 1
	}
	return model.D{
		"messages":            conversation,
		"temperature":         llmConfig.Temperature,
		"top_p":               llmConfig.TopP,
		"top_k":               topK,
		"tools":               toolDocs,
		"tool_choice":         llmConfig.ToolChoice,
		"parallel_tool_calls": false,
	}
}
