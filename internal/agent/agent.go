package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"uuid"

	"github.com/ardanlabs/kronk/sdk/kronk/model"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/trollLemon/MPHarness/internal/config"
	"github.com/trollLemon/MPHarness/internal/multipass"
	mphotel "github.com/trollLemon/MPHarness/internal/otel"
)

type Kronk interface {
	Chat(ctx context.Context, req model.D) (model.ChatResponse, error)
	ModelConfig() model.Config
}

type LLMConfig struct {
	Temperature float64 `json:"temperature" yaml:"temperature"`
	TopP        float64 `json:"top_p" yaml:"top_p"`
	TopK        int     `json:"top_k" yaml:"top_k"`
	ToolChoice  string  `json:"tool_choice" yaml:"tool_choice"`
}

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

func (a *Agent) Execute(ctx context.Context, conf config.Config, cli *multipass.Client) error {
	ctx, cancel := context.WithTimeout(ctx, a.totalTimeout)
	defer cancel()

	toolDocs := buildToolDocuments()
	a.log.Info().Int("tools", len(toolDocs)).Msg("tool documents built")

	conversation := buildInitialConversation(conf)

	runUuid := uuid.NewV7()
	runID := runUuid.String()
	ctx = withRunID(ctx, runID)

	var commandOutputs []string
	var lastAssistantContent string

	a.log.Info().Str("uuid", runID).Msg("Starting inference")
	initAgentMetrics()
	recordContextWindow(ctx, a.krn)

	var lastContextTokens int64

	for iter := 0; iter < a.maxIterations; iter++ {
		res, err := a.runIteration(ctx, iter, conversation, toolDocs, commandOutputs, cli, conf, lastContextTokens)
		if err != nil {
			return err
		}
		conversation, commandOutputs, lastContextTokens = res.conversation, res.commandOutputs, res.contextTokens
		if res.content != "" {
			lastAssistantContent = res.content
		}
		if res.done {
			break
		}
	}

	if len(commandOutputs) > 0 {
		joined := strings.Join(commandOutputs, "\n---\n")
		a.log.Info().Str("command_output", truncate(joined, conf.Truncation.CommandOutput)).Msg("final command output")
		fmt.Println(joined)
	} else if lastAssistantContent != "" {
		a.log.Info().Str("final_output", truncate(lastAssistantContent, conf.Truncation.LogContent)).Msg("agent final answer (no command output)")
	} else {
		a.log.Info().Msg("agent completed with no command output or final answer")
	}

	return nil
}

// iterationResult is what one inference round hands back to the run loop.
type iterationResult struct {
	conversation   []model.D
	commandOutputs []string
	contextTokens  int64
	content        string
	done           bool
}

// runIteration runs one inference round.
func (a *Agent) runIteration(
	ctx context.Context, iter int, conversation, toolDocs []model.D,
	commandOutputs []string, cli *multipass.Client, conf config.Config,
	lastContextTokens int64,
) (res *iterationResult, err error) {
	res = &iterationResult{
		conversation:   conversation,
		commandOutputs: commandOutputs,
		contextTokens:  lastContextTokens,
	}

	iterCtx, iterSpan := getAgentTracer().Start(ctx, "mph.agent.iteration",
		trace.WithAttributes(runSpanAttrs(ctx, attribute.Int("mph.iteration", iter+1))...))
	if iterationsCounter != nil {
		iterationsCounter.Add(iterCtx, 1, runAttrs(iterCtx), metric.WithAttributes(attribute.Int("mph.iteration", iter+1)))
	}

	iterLog := a.log.With().Ctx(iterCtx).Logger()

	iterLog.Debug().Int("iteration", iter+1).Msg("running inference")

	resp, err := a.callChat(iterCtx, buildChatRequest(conversation, toolDocs, a.llmConfig), iter)
	if err != nil {
		iterSpan.RecordError(err)
		iterSpan.SetStatus(codes.Error, err.Error())
		iterSpan.End()
		return res, err
	}

	lastContextTokens = recordTokenUsage(iterCtx, iterSpan, iterLog, lastContextTokens, resp.Usage)
	res.contextTokens = lastContextTokens

	msg, finishReason, shouldBreak := a.extractAssistantMessage(resp)
	iterSpan.SetAttributes(attribute.String("mph.finish_reason", finishReason))
	if shouldBreak {
		iterSpan.End()
		res.done = true
		return res, nil
	}

	res.content = logModelOutput(iterCtx, iterLog, iter, msg, conf.Truncation.LogContent)
	if res.content == "" && finishReason != model.FinishReasonTool && len(msg.ToolCalls) == 0 {
		mphotel.LogEvent(iterCtx, iterLog, zerolog.DebugLevel, "empty content", map[string]any{
			"finish_reason": finishReason,
		})
	}

	toolCalls := msg.ToolCalls
	addToolCallEvents(iterSpan, toolCalls, conf.Truncation.ToolResult)

	if len(toolCalls) == 0 && finishReason == model.FinishReasonLength {
		mphotel.LogEvent(iterCtx, iterLog, zerolog.WarnLevel, "hit length limit without tool calls, injecting nudge and continuing", map[string]any{
			"iteration":     iter + 1,
			"finish_reason": finishReason,
		})
		iterSpan.AddEvent("mph.agent.length_nudge", trace.WithAttributes(attribute.Int("mph.iteration", iter+1)))
		res.conversation = appendToConversation(conversation, buildLengthNudgeMessages(msg, conf.Truncation.Nudge)...)
		iterSpan.End()
		return res, nil
	}

	if a.shouldTerminateWithoutToolCalls(toolCalls, finishReason, iter) {
		iterSpan.End()
		res.done = true
		return res, nil
	}

	toolCallDocs := a.buildToolCallDocsWithContext(iterCtx, toolCalls, iter)
	res.conversation = appendToConversation(conversation, buildAssistantMessage(msg, toolCallDocs))

	toolResponses, newOutputs := a.executeToolCalls(iterCtx, cli, conf, toolCalls)
	res.commandOutputs = append(res.commandOutputs, newOutputs...)
	res.conversation = appendToConversation(res.conversation, toolResponses...)

	if len(res.commandOutputs) > 0 {
		iterLog.Debug().Int("command_outputs", len(res.commandOutputs)).Msg("collected outputs so far")
	}
	iterSpan.End()
	return res, nil
}

// logModelOutput logs the reasoning and content of a model reply and returns
// the content, which the run reports as its final answer when nothing ran.
func logModelOutput(ctx context.Context, log zerolog.Logger, iter int, msg *model.ResponseMessage, maxContent int) string {
	if msg.Reasoning != "" {
		mphotel.LogEvent(ctx, log, zerolog.InfoLevel, "agent reasoning", map[string]any{
			"iteration": iter + 1,
			"reasoning": truncate(msg.Reasoning, maxContent),
		})
	}
	if msg.Content == "" {
		return ""
	}
	mphotel.LogEvent(ctx, log, zerolog.InfoLevel, "agent output", map[string]any{
		"iteration": iter + 1,
		"content":   truncate(msg.Content, maxContent),
	})
	return msg.Content
}

func buildInitialConversation(conf config.Config) []model.D {
	return []model.D{
		{"role": "system", "content": buildSystemPrompt(conf)},
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
	return a.buildToolCallDocsWithContext(context.Background(), toolCalls, iter)
}

func (a *Agent) buildToolCallDocsWithContext(ctx context.Context, toolCalls []model.ResponseToolCall, iter int) []model.D {
	docs := make([]model.D, 0, len(toolCalls))
	for _, tc := range toolCalls {
		argsJSON, _ := json.Marshal(tc.Function.Arguments)
		mphotel.LogEvent(ctx, a.log, zerolog.InfoLevel, "agent tool call", map[string]any{
			"iteration": iter + 1,
			"tool":      tc.Function.Name,
			"id":        tc.ID,
			"arguments": json.RawMessage(argsJSON),
		})

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

func (a *Agent) executeToolCalls(ctx context.Context, cli *multipass.Client, cfg config.Config, toolCalls []model.ResponseToolCall) ([]model.D, []string) {
	var toolResponses []model.D
	var commandOutputs []string

	span := trace.SpanFromContext(ctx)
	for _, tc := range toolCalls {
		start := time.Now()
		result, err := Call(ctx, cli, cfg, tc.Function.Name, map[string]any(tc.Function.Arguments))
		duration := time.Since(start)
		durationSec := duration.Seconds()
		durationMs := duration.Milliseconds()
		var content string
		var status string
		if err != nil {
			status = "FAILED"
			mphotel.LogEvent(ctx, a.log, zerolog.ErrorLevel, "tool execution failed", map[string]any{
				"tool":  tc.Function.Name,
				"id":    tc.ID,
				"error": err,
			})
			content = buildToolErrorContent(err)
			if toolFailuresCounter != nil {
				toolFailuresCounter.Add(ctx, 1, runAttrs(ctx), metric.WithAttributes(attribute.String("mph.tool.name", tc.Function.Name)))
			}
			span.AddEvent("mph.agent.tool_result", trace.WithAttributes(
				attribute.String("mph.tool.name", tc.Function.Name),
				attribute.String("mph.tool.id", tc.ID),
				attribute.String("mph.tool.status", status),
				attribute.String("mph.tool.result", truncate(err.Error(), cfg.Truncation.ToolResult)),
				attribute.Int64("mph.tool.duration_ms", durationMs),
			))
		} else {
			result = truncate(result, cfg.Agent.MaxOutputBytes)
			status = "SUCCESS"
			mphotel.LogEvent(ctx, a.log, zerolog.InfoLevel, "tool succeeded", map[string]any{
				"tool":   tc.Function.Name,
				"id":     tc.ID,
				"result": truncate(result, cfg.Truncation.ToolResult),
			})
			if tc.Function.Name == "multipass_exec" {
				commandOutputs = append(commandOutputs, result)
				mphotel.LogEvent(ctx, a.log, zerolog.InfoLevel, "command output", map[string]any{
					"command_output": truncate(result, cfg.Truncation.CommandOutput),
				})
			}
			content = buildToolSuccessContent(tc.Function.Name, result)
			span.AddEvent("mph.agent.tool_result", trace.WithAttributes(
				attribute.String("mph.tool.name", tc.Function.Name),
				attribute.String("mph.tool.id", tc.ID),
				attribute.String("mph.tool.status", status),
				attribute.String("mph.tool.result", truncate(result, cfg.Truncation.ToolResult)),
				attribute.Int64("mph.tool.duration_ms", durationMs),
			))
		}
		if toolCallsCounter != nil {
			toolCallsCounter.Add(ctx, 1, runAttrs(ctx), metric.WithAttributes(attribute.String("mph.tool.name", tc.Function.Name)))
		}
		if toolDurationHist != nil {
			toolDurationHist.Record(ctx, durationSec, runAttrs(ctx), metric.WithAttributes(
				attribute.String("mph.tool.name", tc.Function.Name),
				attribute.String("mph.tool.status", status),
			))
		}

		toolResponses = append(toolResponses, buildToolResponseMessage(tc, content))
	}

	return toolResponses, commandOutputs
}

// truncate caps s at n bytes for logs and span attributes. A non-positive n
// means no limit, which is how a caller opts out of capping entirely.
func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return fmt.Sprintf("%s…(truncated, %d of %d bytes)", s[:n], n, len(s))
}
