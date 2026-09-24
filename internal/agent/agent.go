package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ardanlabs/kronk/sdk/kronk/model"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
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

var (
	tokensHist            metric.Int64Histogram
	tpsHist               metric.Float64Histogram
	iterationsCounter     metric.Int64Counter
	toolDurationHist      metric.Float64Histogram
	toolCallsCounter      metric.Int64Counter
	toolFailuresCounter   metric.Int64Counter
	agentMetricsOnce      sync.Once
)

func getAgentTracer() trace.Tracer {
	return otel.Tracer("mph")
}

func getAgentMeter() metric.Meter {
	return otel.Meter("mph")
}

func initAgentMetrics() {
	agentMetricsOnce.Do(func() {
		meter := getAgentMeter()
		var err error
		tokensHist, err = meter.Int64Histogram("mph.tokens")
		if err != nil {
			tokensHist = nil
		}
		tpsHist, err = meter.Float64Histogram("mph.tokens_per_second")
		if err != nil {
			tpsHist = nil
		}
		iterationsCounter, err = meter.Int64Counter("mph.iterations")
		if err != nil {
			iterationsCounter = nil
		}
		toolDurationHist, err = meter.Float64Histogram("mph.agent.tool.duration")
		if err != nil {
			toolDurationHist = nil
		}
		toolCallsCounter, err = meter.Int64Counter("mph.agent.tool.calls")
		if err != nil {
			toolCallsCounter = nil
		}
		toolFailuresCounter, err = meter.Int64Counter("mph.agent.tool.failures")
		if err != nil {
			toolFailuresCounter = nil
		}
	})
}

func recordTokenUsage(ctx context.Context, span trace.Span, log zerolog.Logger, usage *model.Usage) {
	if usage == nil {
		return
	}
	mphotel.LogEvent(ctx, log, zerolog.InfoLevel, "token usage", map[string]any{
		"prompt_tokens":     usage.PromptTokens,
		"completion_tokens": usage.CompletionTokens,
		"total_tokens":      usage.TotalTokens,
		"tps":               usage.TokensPerSecond,
	})
	span.SetAttributes(
		attribute.Int("mph.tokens.prompt", usage.PromptTokens),
		attribute.Int("mph.tokens.completion", usage.CompletionTokens),
		attribute.Int("mph.tokens.total", usage.TotalTokens),
		attribute.Float64("mph.tokens_per_second", usage.TokensPerSecond),
	)
	if tokensHist != nil {
		tokensHist.Record(ctx, int64(usage.PromptTokens), metric.WithAttributes(attribute.String("tokens.type", "prompt")))
		tokensHist.Record(ctx, int64(usage.CompletionTokens), metric.WithAttributes(attribute.String("tokens.type", "completion")))
		tokensHist.Record(ctx, int64(usage.TotalTokens), metric.WithAttributes(attribute.String("tokens.type", "total")))
	}
	if tpsHist != nil {
		tpsHist.Record(ctx, usage.TokensPerSecond)
	}
}

func (a *Agent) Execute(ctx context.Context, conf config.Config, cli *multipass.Client) error {
	ctx, cancel := context.WithTimeout(ctx, a.totalTimeout)
	defer cancel()

	toolDocs := buildToolDocuments()
	a.log.Info().Int("tools", len(toolDocs)).Msg("tool documents built")

	conversation := buildInitialConversation(conf)

	var commandOutputs []string
	var lastAssistantContent string

	a.log.Info().Msg("Starting inference")

	for iter := 0; iter < a.maxIterations; iter++ {
		iterCtx, iterSpan := getAgentTracer().Start(ctx, "mph.agent.iteration", trace.WithAttributes(attribute.Int("mph.iteration", iter+1)))
		initAgentMetrics()
		if iterationsCounter != nil {
			iterationsCounter.Add(iterCtx, 1, metric.WithAttributes(attribute.Int("mph.iteration", iter+1)))
		}

		iterLog := a.log.With().Ctx(iterCtx).Logger()

		iterLog.Debug().Int("iteration", iter+1).Msg("running inference")
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

		resp, err := a.callChat(iterCtx, req, iter)
		if err != nil {
			iterSpan.RecordError(err)
			iterSpan.SetStatus(codes.Error, err.Error())
			iterSpan.End()
			return err
		}

		recordTokenUsage(iterCtx, iterSpan, iterLog, resp.Usage)

		msg, finishReason, shouldBreak := a.extractAssistantMessage(resp)
		iterSpan.SetAttributes(attribute.String("mph.finish_reason", finishReason))
		if shouldBreak {
			iterSpan.End()
			break
		}
		if msg.Reasoning != "" {
			mphotel.LogEvent(iterCtx, iterLog, zerolog.InfoLevel, "agent reasoning", map[string]any{
				"iteration": iter + 1,
				"reasoning": truncate(msg.Reasoning, 8000),
			})
		}
		if msg.Content != "" {
			mphotel.LogEvent(iterCtx, iterLog, zerolog.InfoLevel, "agent output", map[string]any{
				"iteration": iter + 1,
				"content":   truncate(msg.Content, 8000),
			})
			lastAssistantContent = msg.Content
		} else if finishReason != model.FinishReasonTool && len(msg.ToolCalls) == 0 {
			mphotel.LogEvent(iterCtx, iterLog, zerolog.DebugLevel, "empty content", map[string]any{
				"finish_reason": finishReason,
			})
		}

		toolCalls := msg.ToolCalls
		for _, tc := range toolCalls {
			argsJSON, _ := json.Marshal(tc.Function.Arguments)
			iterSpan.AddEvent("mph.agent.tool_call", trace.WithAttributes(
				attribute.String("mph.tool.name", tc.Function.Name),
				attribute.String("mph.tool.id", tc.ID),
				attribute.String("mph.tool.arguments", truncate(string(argsJSON), 4000)),
			))
		}

		if len(toolCalls) == 0 && finishReason == model.FinishReasonLength {
			mphotel.LogEvent(iterCtx, iterLog, zerolog.WarnLevel, "hit length limit without tool calls, injecting nudge and continuing", map[string]any{
				"iteration":     iter + 1,
				"finish_reason": finishReason,
			})
			iterSpan.AddEvent("mph.agent.length_nudge", trace.WithAttributes(attribute.Int("mph.iteration", iter+1)))
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
			iterSpan.End()
			continue
		}
		if a.shouldTerminateWithoutToolCalls(toolCalls, finishReason, iter) {
			iterSpan.End()
			break
		}

		toolCallDocs := a.buildToolCallDocsWithContext(iterCtx, toolCalls, iter)
		assistantMsg := buildAssistantMessage(msg, toolCallDocs)
		conversation = appendToConversation(conversation, assistantMsg)

		toolResponses, newOutputs := a.executeToolCalls(iterCtx, cli, conf, toolCalls)
		commandOutputs = append(commandOutputs, newOutputs...)
		conversation = appendToConversation(conversation, toolResponses...)

		if len(commandOutputs) > 0 {
			iterLog.Debug().Int("command_outputs", len(commandOutputs)).Msg("collected outputs so far")
		}
		iterSpan.End()
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
			initAgentMetrics()
			if toolFailuresCounter != nil {
				toolFailuresCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("mph.tool.name", tc.Function.Name)))
			}
			span.AddEvent("mph.agent.tool_result", trace.WithAttributes(
				attribute.String("mph.tool.name", tc.Function.Name),
				attribute.String("mph.tool.id", tc.ID),
				attribute.String("mph.tool.status", status),
				attribute.String("mph.tool.result", truncate(err.Error(), 4000)),
				attribute.Int64("mph.tool.duration_ms", durationMs),
			))
		} else {
			status = "SUCCESS"
			mphotel.LogEvent(ctx, a.log, zerolog.InfoLevel, "tool succeeded", map[string]any{
				"tool":   tc.Function.Name,
				"id":     tc.ID,
				"result": truncate(result, 4000),
			})
			if tc.Function.Name == "multipass_exec" {
				commandOutputs = append(commandOutputs, result)
				mphotel.LogEvent(ctx, a.log, zerolog.InfoLevel, "command output", map[string]any{
					"command_output": truncate(result, 600),
				})
			}
			content = buildToolSuccessContent(tc.Function.Name, result)
			span.AddEvent("mph.agent.tool_result", trace.WithAttributes(
				attribute.String("mph.tool.name", tc.Function.Name),
				attribute.String("mph.tool.id", tc.ID),
				attribute.String("mph.tool.status", status),
				attribute.String("mph.tool.result", truncate(result, 4000)),
				attribute.Int64("mph.tool.duration_ms", durationMs),
			))
		}
		initAgentMetrics()
		if toolCallsCounter != nil {
			toolCallsCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("mph.tool.name", tc.Function.Name)))
		}
		if toolDurationHist != nil {
			toolDurationHist.Record(ctx, durationSec, metric.WithAttributes(
				attribute.String("mph.tool.name", tc.Function.Name),
				attribute.String("mph.tool.status", status),
			))
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
