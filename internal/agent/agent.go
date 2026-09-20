package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ardanlabs/kronk/sdk/kronk/model"
	"github.com/rs/zerolog"

	"github.com/trollLemon/MPHarness/internal/config"
	"github.com/trollLemon/MPHarness/internal/multipass"
)

// Kronk is the minimal interface of the kronk client used by Agent.
// Defining this allows unit tests to mock the client, rather than have to run a real model
// each time .
type Kronk interface {
	Chat(ctx context.Context, req model.D) (model.ChatResponse, error)
}

// LLMConfig holds the configuration for the LLM when creating an Agent.
// With this struct, you can control how deterministic or non deterministic you want
// the agent to be.
type LLMConfig struct {
	Temperature float64 `json:"temperature" yaml:"temperature"`
	TopP        float64 `json:"top_p" yaml:"top_p"`
	TopK        int     `json:"top_k" yaml:"top_k"`
	ToolChoice  string  `json:"tool_choice" yaml:"tool_choice"`
}

// Agent is the logical bundling of a logger, LLM client, timeout, and LLM configs, as
// a single unit to perform tasks.
type Agent struct {
	log           zerolog.Logger
	krn           Kronk
	maxIterations int
	chatTimeout   time.Duration
	totalTimeout  time.Duration
	llmConfig     LLMConfig
}

func NewAgent(log zerolog.Logger, krn Kronk, maxIterations int, chatTimeout time.Duration, totalTimeout time.Duration, llmConfig LLMConfig) *Agent {
	if llmConfig.ToolChoice == "" {
		llmConfig.ToolChoice = "auto"
	}
	log = log.With().Str("component", "agent").Logger()
	return &Agent{
		log:           log,
		krn:           krn,
		maxIterations: maxIterations,
		chatTimeout:   chatTimeout,
		totalTimeout:  totalTimeout,
		llmConfig:     llmConfig,
	}
}

// Execute runs a chat session where the model completes the task defined in the VM config.
// This call is blocking, and will exit when the LLM determines the task is complete, ran into a failure
// it couldn't recover from, the totalTimeout fires, or if the max iterations was reached.
func (a *Agent) Execute(conf config.Config, cli *multipass.Client) error {
	ctx, cancel := context.WithTimeout(context.Background(), a.totalTimeout)
	defer cancel()

	toolDocs := buildToolDocuments()
	a.log.Info().Int("tools", len(toolDocs)).Msg("tool documents built")

	conversation := buildInitialConversation(conf)

	var commandOutputs []string
	var lastAssistantContent string

	a.log.Info().Msg("Starting inference")

	for iter := 0; iter < a.maxIterations; iter++ {
		a.log.Debug().Int("iteration", iter+1).Msg("running inference")
		topK := a.llmConfig.TopK
		if topK == 0 {
			topK = 1
		}
		req := model.D{
			"messages":            conversation,
			"temperature":         a.llmConfig.Temperature,
			"top_p":               a.llmConfig.TopP,
			"top_k":               topK,
			"tools":               toolDocs,
			"tool_choice":         a.llmConfig.ToolChoice,
			"parallel_tool_calls": false,
		}

		resp, err := a.callChat(ctx, req, iter)
		if err != nil {
			return err
		}

		if resp.Usage != nil {
			a.log.Info().
				Int("prompt_tokens", resp.Usage.PromptTokens).
				Int("completion_tokens", resp.Usage.CompletionTokens).
				Int("total_tokens", resp.Usage.TotalTokens).
				Float64("tps", resp.Usage.TokensPerSecond).
				Msg("token usage")
		}

		msg, finishReason, shouldBreak := a.extractAssistantMessage(resp)
		if shouldBreak {
			break
		}
		if msg.Reasoning != "" {
			a.log.Info().Int("iteration", iter+1).Str("reasoning", truncate(msg.Reasoning, 8000)).Msg("agent reasoning")
		}
		if msg.Content != "" {
			a.log.Info().Int("iteration", iter+1).Str("content", truncate(msg.Content, 8000)).Msg("agent output")
			lastAssistantContent = msg.Content
		} else if finishReason != model.FinishReasonTool && len(msg.ToolCalls) == 0 {
			a.log.Debug().Str("finish_reason", finishReason).Msg("empty content")
		}

		toolCalls := msg.ToolCalls
		if len(toolCalls) == 0 && finishReason == model.FinishReasonLength {
			a.log.Warn().Int("iteration", iter+1).Str("finish_reason", finishReason).Msg("hit length limit without tool calls, injecting nudge and continuing")
			if msg.Reasoning != "" || msg.Content != "" {
				nudgedAssistant := model.D{"role": "assistant"}
				if msg.Content != "" {
					nudgedAssistant["content"] = truncate(msg.Content, 2000)
				}
				if msg.Reasoning != "" {
					nudgedAssistant["reasoning_content"] = truncate(msg.Reasoning, 2000)
				}
				conversation = appendToConversation(conversation, nudgedAssistant)
			}
			conversation = appendToConversation(conversation, model.D{
				"role":    "user",
				"content": "Your previous response hit the token limit without making a tool call. Please be concise and try again and run a tool call if necessary",
			})
			continue
		}
		if a.shouldTerminateWithoutToolCalls(toolCalls, finishReason, iter) {
			break
		}

		toolCallDocs := a.buildToolCallDocs(toolCalls, iter)
		assistantMsg := buildAssistantMessage(msg, toolCallDocs)
		conversation = appendToConversation(conversation, assistantMsg)

		toolResponses, newOutputs := a.executeToolCalls(ctx, cli, conf, toolCalls)
		commandOutputs = append(commandOutputs, newOutputs...)
		conversation = appendToConversation(conversation, toolResponses...)

		if len(commandOutputs) > 0 {
			a.log.Debug().Int("command_outputs", len(commandOutputs)).Msg("collected outputs so far")
		}
	}

	if len(commandOutputs) > 0 {
		joined := strings.Join(commandOutputs, "\n---\n")
		a.log.Info().Str("command_output", truncate(joined, 600)).Msg("final command output")
		fmt.Println(joined)
	} else if lastAssistantContent != "" {
		a.log.Info().Str("final_output", truncate(lastAssistantContent, 8000)).Msg("agent final answer (no command output)")
	} else {
		a.log.Info().Msg("agent completed with no command output or final answer")
	}

	return nil
}

func buildInitialConversation(conf config.Config) []model.D {
	return []model.D{
		{"role": "system", "content": systemPrompt},
		{"role": "user", "content": buildUserPrompt(conf)},
	}
}

func (a *Agent) callChat(ctx context.Context, req model.D, iter int) (model.ChatResponse, error) {
	callCtx, callCancel := context.WithTimeout(ctx, a.chatTimeout)
	defer callCancel()

	resp, err := a.krn.Chat(callCtx, req)
	if err != nil {
		return model.ChatResponse{}, fmt.Errorf("kronk chat iteration %d: %w", iter+1, err)
	}
	return resp, nil
}

func (a *Agent) extractAssistantMessage(resp model.ChatResponse) (*model.ResponseMessage, string, bool) {
	if len(resp.Choices) == 0 {
		a.log.Warn().Msg("no choices in response, ending loop")
		return nil, "", true
	}

	choice := resp.Choices[0]
	fr := choice.FinishReason()

	msg := choice.Message
	if msg == nil {
		msg = choice.Delta
	}
	if msg == nil {
		a.log.Warn().Str("finish_reason", fr).Msg("empty message, ending loop")
		return nil, fr, true
	}

	return msg, fr, false
}

func (a *Agent) shouldTerminateWithoutToolCalls(toolCalls []model.ResponseToolCall, finishReason string, iter int) bool {
	if len(toolCalls) != 0 {
		return false
	}
	if finishReason == model.FinishReasonStop || finishReason == model.FinishReasonLength || finishReason == "" {
		a.log.Info().Int("iteration", iter+1).Str("finish_reason", finishReason).Msg("agent finished without tool calls")
		return true
	}
	a.log.Info().Str("finish_reason", finishReason).Msg("no tool calls, ending loop")
	return true
}

func (a *Agent) buildToolCallDocs(toolCalls []model.ResponseToolCall, iter int) []model.D {
	docs := make([]model.D, 0, len(toolCalls))
	for _, tc := range toolCalls {
		argsJSON, _ := json.Marshal(tc.Function.Arguments)
		a.log.Info().
			Int("iteration", iter+1).
			Str("tool", tc.Function.Name).
			Str("id", tc.ID).
			RawJSON("arguments", argsJSON).
			Msg("agent tool call")

		docs = append(docs, model.D{
			"id":   tc.ID,
			"type": "function",
			"function": model.D{
				"name":      tc.Function.Name,
				"arguments": string(argsJSON),
			},
		})
	}
	return docs
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

func appendToConversation(conversation []model.D, msgs ...model.D) []model.D {
	return append(conversation, msgs...)
}

func (a *Agent) executeToolCalls(ctx context.Context, cli *multipass.Client, cfg config.Config, toolCalls []model.ResponseToolCall) ([]model.D, []string) {
	var toolResponses []model.D
	var commandOutputs []string

	for _, tc := range toolCalls {
		result, err := Call(ctx, cli, cfg, tc.Function.Name, map[string]any(tc.Function.Arguments))
		var content string
		if err != nil {
			a.log.Error().Err(err).Str("tool", tc.Function.Name).Str("id", tc.ID).Msg("tool execution failed")
			content = buildToolErrorContent(err)
		} else {
			a.log.Info().Str("tool", tc.Function.Name).Str("id", tc.ID).Str("result", truncate(result, 4000)).Msg("tool succeeded")
			if tc.Function.Name == "multipass_exec" {
				commandOutputs = append(commandOutputs, result)
				a.log.Info().Str("command_output", truncate(result, 600)).Msg("command output")
			}
			content = buildToolSuccessContent(tc.Function.Name, result)
		}

		toolResponses = append(toolResponses, model.D{
			"role":         "tool",
			"tool_call_id": tc.ID,
			"name":         tc.Function.Name,
			"content":      content,
		})
	}

	return toolResponses, commandOutputs
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}
