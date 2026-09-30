package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ardanlabs/kronk/sdk/kronk/model"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/trollLemon/MPHarness/internal/config"
	"github.com/trollLemon/MPHarness/internal/multipass"
	mphotel "github.com/trollLemon/MPHarness/internal/otel"
	"github.com/trollLemon/MPHarness/internal/output"
)

type Kronk interface {
	Chat(ctx context.Context, req model.D) (model.ChatResponse, error)
	Tokenize(ctx context.Context, d model.D) (model.TokenizeResponse, error)
	ModelConfig() model.Config
}

type LLMConfig struct {
	Temperature     float64 `json:"temperature" yaml:"temperature"`
	TopP            float64 `json:"top_p" yaml:"top_p"`
	TopK            int     `json:"top_k" yaml:"top_k"`
	ToolChoice      string  `json:"tool_choice" yaml:"tool_choice"`
	MaxOutputTokens int     `json:"max_output_tokens" yaml:"max_output_tokens"`
}

const (
	// DefaultMaxOutputTokens caps one model turn. Without it a runaway completion
	// can decode until it exhausts the context window, which trips the length
	// nudge and costs a whole extra iteration.
	DefaultMaxOutputTokens = 2048

	// toolResultWindowDivisor sets each tool result to roughly 1/divisor of the
	// live context window, leaving room for the remaining iterations, the
	// assistant turns, and the tool call documents.
	toolResultWindowDivisor = 8

	// bytesPerToken is a deliberately conservative text estimate. Command output
	// is mostly ASCII, so real payloads land near 4 bytes per token.
	bytesPerToken = 4

	minToolResultBytes = 2 * 1024
	maxToolResultBytes = 64 * 1024

	// assumedContextWindow is used when the model reports no window, so the
	// budget is still bounded rather than falling back to an unbounded payload.
	assumedContextWindow = 8192

	// finalOutputBytes bounds the stdout dump of every command result. The log
	// line is truncated separately, but stdout is consumed by whatever runs the
	// harness, so an unbounded join becomes that consumer's token cost.
	finalOutputBytes = 64 * 1024
)

func finalCommandOutput(commandOutputs []string) string {
	if len(commandOutputs) == 0 {
		return ""
	}
	return truncate(strings.Join(commandOutputs, "\n---\n"), finalOutputBytes)
}

func toolResultBudgetBytes(cfg config.Config, contextWindow int) int {
	if cfg.Agent.MaxOutputBytes > 0 {
		return cfg.Agent.MaxOutputBytes
	}
	if contextWindow <= 0 {
		contextWindow = assumedContextWindow
	}
	budget := (contextWindow / toolResultWindowDivisor) * bytesPerToken
	return max(min(budget, maxToolResultBytes), minToolResultBytes)
}

// Agent owns the state of a single run. Iteration state lives here rather than
// being threaded through every signature, so the per-iteration helpers read as
// what they do instead of what they carry.
type Agent struct {
	log           zerolog.Logger // Run-scoped logger, already tagged component=agent.
	krn           Kronk          // Model backend: chat, tokenize, and model metadata.
	maxIterations int            // Hard cap on inference rounds before the run stops.
	chatTimeout   time.Duration  // Budget for one chat call; guards a stalled model.
	totalTimeout  time.Duration  // Budget for the whole run; bounds every iteration.
	llmConfig     LLMConfig      // Sampling and output limits sent with each request.
	runID         string         // Identifies this run in logs, spans, and output paths.

	conversation       []model.D     // Full message history, including tool results.
	toolDocs           []model.D     // Tool schemas sent alongside every request.
	commandOutputs     []string      // Captured command results, in the order they ran.
	store              *output.Store // Captured output handles, or nil when disabled.
	contextTokens      int64         // Live context size, driving compaction and metrics.
	iteration          int           // Zero-based index of the round now running.
	compactionAttempts int           // Failed compactions; budget for the current one.
	compactionsApplied int           // Compactions that succeeded this run.
	toolResultBudget   int           // Byte cutoff between inline output and a handle.
	done               bool          // Set when the agent should stop looping.
}

// NewAgent builds an Agent for a single run. A zero ToolChoice defaults to
// "auto" so callers can leave sampling config unset.
func NewAgent(log zerolog.Logger, krn Kronk, maxIterations int, chatTimeout time.Duration, totalTimeout time.Duration, llmConfig LLMConfig, runID string) *Agent {
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
		runID:         runID,
	}
}

// Execute runs the agent to completion, bounded by totalTimeout and
// maxIterations, and returns the first error that aborts the run. It owns the
// iteration loop, compacting the context before each round, and prints the
// final command output or assistant answer when the loop ends.
func (a *Agent) Execute(ctx context.Context, conf config.Config, cli *multipass.Client) error {
	ctx, cancel := context.WithTimeout(ctx, a.totalTimeout)
	defer cancel()

	ctx = withRunID(ctx, a.runID)

	a.toolDocs = buildToolDocuments(conf)
	a.conversation = buildInitialConversation(conf)
	a.toolResultBudget = toolResultBudgetBytes(conf, a.krn.ModelConfig().ContextWindow())

	if conf.Output.Enabled {
		a.store = output.NewStore(cli, conf.VM.Name, "/tmp/mph-output/"+a.runID, conf.Output, int64(a.toolResultBudget), conf.AllowedCommands)
	}

	a.log.Info().Int("tools", len(a.toolDocs)).Msg("tool documents built")
	a.log.Info().Str("run", a.runID).Msg("Starting inference")

	initAgentMetrics()
	recordContextWindow(ctx, a.krn)

	for ; a.iteration < a.maxIterations && !a.done; a.iteration++ {
		a.maybeCompact(ctx, conf)
		if err := a.runIteration(ctx, cli, conf); err != nil {
			return err
		}
	}

	if len(a.commandOutputs) > 0 {
		final := finalCommandOutput(a.commandOutputs)
		a.log.Info().Str("out", truncate(final, conf.Truncation.CommandOutput)).Msg("final command output")
		fmt.Println(final)
	} else {
		a.log.Info().Msg("agent completed with no command output or final answer")
	}

	contextUsedPercent := 0.0
	if window := a.krn.ModelConfig().ContextWindow(); window > 0 {
		contextUsedPercent = float64(a.contextTokens) / float64(window) * 100
	}

	a.log.Info().
		Float64("ctx_pct", contextUsedPercent).
		Int("compacted", a.compactionsApplied).
		Int("compact_tries", a.compactionAttempts).
		Msg("run complete")

	return nil
}

func (a *Agent) runIteration(ctx context.Context, cli *multipass.Client, conf config.Config) error {
	iter := a.iteration

	iterCtx, iterSpan := getAgentTracer().Start(ctx, "mph.agent.iteration",
		trace.WithAttributes(runSpanAttrs(ctx, attribute.Int("mph.iteration", iter+1))...))
	if iterationsCounter != nil {
		iterationsCounter.Add(iterCtx, 1, metric.WithAttributes(attribute.Int("mph.iteration", iter+1)))
	}

	iterLog := a.log.With().Ctx(iterCtx).Logger()

	iterLog.Debug().Int("iter", iter+1).Msg("running inference")

	resp, err := a.callChat(iterCtx, buildChatRequest(a.conversation, a.toolDocs, a.llmConfig), iter)
	if err != nil {
		iterSpan.RecordError(err)
		iterSpan.SetStatus(codes.Error, err.Error())
		iterSpan.End()
		return err
	}

	// The reported usage is the measurement; anything this turn appends on top
	// of it is tallied by addToolTokens below.
	recordTokenUsage(iterCtx, iterSpan, iterLog, resp.Usage)
	a.contextTokens = 0
	if resp.Usage != nil {
		a.contextTokens = int64(resp.Usage.TotalTokens)
	}

	msg, finishReason, shouldBreak := a.extractAssistantMessage(resp)
	iterSpan.SetAttributes(attribute.String("mph.finish_reason", finishReason))
	if shouldBreak {
		iterSpan.End()
		a.done = true
		return nil
	}

	content := logModelOutput(iterCtx, iterLog, iter, msg, conf.Truncation.LogContent)
	// Bare tool calls carry no text, so without this a turn leaves no model
	// output trace at all. The shape event proves the model was heard and
	// shows what it returned, instead of looking like dropped logging.
	mphotel.LogEvent(iterCtx, iterLog, zerolog.DebugLevel, "assistant message", map[string]any{
		"iter":   iter + 1,
		"finish": finishReason,
		"clen":   len(msg.Content),
		"rlen":   len(msg.Reasoning),
		"calls":  len(msg.ToolCalls),
	})
	if content == "" && finishReason != model.FinishReasonTool && len(msg.ToolCalls) == 0 {
		mphotel.LogEvent(iterCtx, iterLog, zerolog.DebugLevel, "empty content", map[string]any{
			"finish": finishReason,
		})
	}

	toolCalls := msg.ToolCalls
	addToolCallEvents(iterSpan, toolCalls, conf.Truncation.ToolResult)

	if len(toolCalls) == 0 && finishReason == model.FinishReasonLength {
		mphotel.LogEvent(iterCtx, iterLog, zerolog.WarnLevel, "hit length limit without tool calls, injecting nudge and continuing", map[string]any{
			"iter":   iter + 1,
			"finish": finishReason,
		})
		iterSpan.AddEvent("mph.agent.length_nudge", trace.WithAttributes(attribute.Int("mph.iteration", iter+1)))
		a.conversation = appendToConversation(a.conversation, buildLengthNudgeMessages(msg, conf.Truncation.Nudge)...)
		iterSpan.End()
		return nil
	}

	if a.shouldTerminateWithoutToolCalls(toolCalls, finishReason, iter) {
		iterSpan.End()
		a.done = true
		return nil
	}

	toolCallDocs := a.buildToolCallDocsWithContext(iterCtx, toolCalls, iter)
	a.conversation = appendToConversation(a.conversation, buildAssistantMessage(msg, toolCallDocs, conf.Agent.Reasoning()))

	toolResponses, newOutputs := a.executeToolCalls(iterCtx, cli, conf, a.store, toolCalls)
	a.commandOutputs = append(a.commandOutputs, newOutputs...)
	a.conversation = appendToConversation(a.conversation, toolResponses...)
	a.addToolTokens(iterCtx, toolResponses)

	if len(a.commandOutputs) > 0 {
		iterLog.Debug().Int("outs", len(a.commandOutputs)).Msg("collected outputs so far")
	}
	iterSpan.End()
	return nil
}

func (a *Agent) addToolTokens(ctx context.Context, msgs []model.D) {
	var batch strings.Builder
	for _, m := range msgs {
		c, _ := m["content"].(string)
		if c == "" {
			continue
		}
		batch.WriteString(c)
		batch.WriteByte('\n')
	}
	added := int64(a.estimateTokens(ctx, batch.String()))
	if added == 0 {
		return
	}
	a.contextTokens += added
	if contextTokensGauge != nil {
		contextTokensGauge.Record(ctx, a.contextTokens)
	}
}

func logModelOutput(ctx context.Context, log zerolog.Logger, iter int, msg *model.ResponseMessage, maxContent int) string {
	if msg.Reasoning != "" {
		logModelPart(ctx, log, iter, "reasoning", msg.Reasoning, maxContent)
	}
	if msg.Content == "" {
		return ""
	}
	logModelPart(ctx, log, iter, "content", msg.Content, maxContent)
	return msg.Content
}

func logModelPart(ctx context.Context, log zerolog.Logger, iter int, part, text string, maxContent int) {
	mphotel.LogEvent(ctx, log, zerolog.InfoLevel, "model output", map[string]any{
		"iter": iter + 1,
		"part": part,
		"text": truncate(text, maxContent),
	})
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
		a.log.Warn().Str("finish", fr).Msg("empty message, ending loop")
		return nil, fr, true
	}

	return msg, fr, false
}

func (a *Agent) shouldTerminateWithoutToolCalls(toolCalls []model.ResponseToolCall, finishReason string, iter int) bool {
	if len(toolCalls) != 0 {
		return false
	}
	if finishReason == model.FinishReasonStop || finishReason == model.FinishReasonLength || finishReason == "" {
		a.log.Info().Int("iter", iter+1).Str("finish", finishReason).Msg("agent finished without tool calls")
		return true
	}
	a.log.Info().Str("finish", finishReason).Msg("no tool calls, ending loop")
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
			"iter": iter + 1,
			"tool": tc.Function.Name,
			"id":   tc.ID,
			"args": json.RawMessage(argsJSON),
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

func (a *Agent) executeToolCalls(ctx context.Context, cli *multipass.Client, cfg config.Config, store *output.Store, toolCalls []model.ResponseToolCall) ([]model.D, []string) {
	var toolResponses []model.D
	var commandOutputs []string

	span := trace.SpanFromContext(ctx)
	for _, tc := range toolCalls {
		start := time.Now()
		result, err := Call(ctx, cli, cfg, store, tc.Function.Name, map[string]any(tc.Function.Arguments))
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
				toolFailuresCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("mph.tool.name", tc.Function.Name)))
			}
			span.AddEvent("mph.agent.tool_result", trace.WithAttributes(
				attribute.String("mph.tool.name", tc.Function.Name),
				attribute.String("mph.tool.id", tc.ID),
				attribute.String("mph.tool.status", status),
				attribute.String("mph.tool.result", truncate(err.Error(), cfg.Truncation.ToolResult)),
				attribute.Int64("mph.tool.duration_ms", durationMs),
			))
		} else if isToolEnvelope(result) {
			status = "SUCCESS"
			mphotel.LogEvent(ctx, a.log, zerolog.InfoLevel, "tool succeeded", map[string]any{
				"tool": tc.Function.Name,
				"id":   tc.ID,
				"out":  truncate(result, cfg.Truncation.ToolResult),
			})
			if isCaptureEnvelope(result) {
				if id := capturedOutputID(result); id != "" {
					commandOutputs = append(commandOutputs, fmt.Sprintf("<captured output_id=%s>", id))
					mphotel.LogEvent(ctx, a.log, zerolog.InfoLevel, "command output too large for inline context, captured to file", map[string]any{
						"tool":   tc.Function.Name,
						"id":     tc.ID,
						"out_id": id,
					})
				}
			}
			content = result
			span.AddEvent("mph.agent.tool_result", trace.WithAttributes(
				attribute.String("mph.tool.name", tc.Function.Name),
				attribute.String("mph.tool.id", tc.ID),
				attribute.String("mph.tool.status", status),
				attribute.String("mph.tool.result", truncate(result, cfg.Truncation.ToolResult)),
				attribute.Int64("mph.tool.duration_ms", durationMs),
			))
		} else {
			status = "SUCCESS"
			mphotel.LogEvent(ctx, a.log, zerolog.InfoLevel, "tool succeeded", map[string]any{
				"tool": tc.Function.Name,
				"id":   tc.ID,
				"out":  truncate(result, cfg.Truncation.ToolResult),
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
			toolCallsCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("mph.tool.name", tc.Function.Name)))
		}
		if toolDurationHist != nil {
			toolDurationHist.Record(ctx, durationSec, metric.WithAttributes(
				attribute.String("mph.tool.name", tc.Function.Name),
				attribute.String("mph.tool.status", status),
			))
		}

		toolResponses = append(toolResponses, buildToolResponseMessage(tc, content))
	}

	return toolResponses, commandOutputs
}

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return fmt.Sprintf("%s…(truncated, %d of %d bytes)", trimPartialRuneTail(s[:n]), n, len(s))
}

func trimPartialRuneTail(s string) string {
	for len(s) > 0 {
		if r, size := utf8.DecodeLastRuneInString(s); r == utf8.RuneError && size <= 1 {
			s = s[:len(s)-1]
			continue
		}
		break
	}
	return s
}
