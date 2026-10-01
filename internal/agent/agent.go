package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ardanlabs/kronk/sdk/kronk/model"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/trollLemon/MPHarness/internal/config"
	"github.com/trollLemon/MPHarness/internal/multipass"
	mphotel "github.com/trollLemon/MPHarness/internal/otel"
	"github.com/trollLemon/MPHarness/internal/output"
	"github.com/trollLemon/MPHarness/internal/textutil"
)

type Kronk interface {
	Chat(ctx context.Context, req model.D) (model.ChatResponse, error)
	Tokenize(ctx context.Context, d model.D) (model.TokenizeResponse, error)
	ModelConfig() model.Config
}

const (
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
	return textutil.Truncate(strings.Join(commandOutputs, "\n---\n"), finalOutputBytes)
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
	log           zerolog.Logger   // Run-scoped logger, already tagged component=agent.
	krn           Kronk            // Model backend: chat, tokenize, and model metadata.
	maxIterations int              // Hard cap on inference rounds before the run stops.
	chatTimeout   time.Duration    // Budget for one chat call; guards a stalled model.
	totalTimeout  time.Duration    // Budget for the whole run; bounds every iteration.
	llmConfig     config.LLMConfig // Sampling and output limits sent with each request.
	runID         string           // Bare uuid: the output directory name.
	runLabel      string           // Composite <name>:<uuid>: spans and metrics.
	runName       string           // Human-readable grouping label.

	conversation       []model.D     // Full message history, including tool results.
	toolDocs           []model.D     // Tool schemas sent alongside every request.
	commandOutputs     []string      // Captured command results, in the order they ran.
	store              *output.Store // Captured output handles, or nil when disabled.
	contextTokens      int64         // Live context size, driving compaction and metrics.
	fixedOverhead      int64         // Per-request cost outside the messages: tool schemas and template framing.
	iteration          int           // Zero-based index of the round now running.
	compactionAttempts int           // Failed compactions; budget for the current one.
	compactionsApplied int           // Compactions that succeeded this run.
	toolResultBudget   int           // Byte cutoff between inline output and a handle.
	done               bool          // Set when the agent should stop looping.
	totals             runTotals     // Per-run aggregate for the run-end summary gauges.
}

// Options carries the per-run bounds NewAgent needs.
type Options struct {
	MaxIterations int
	ChatTimeout   time.Duration
	TotalTimeout  time.Duration
	LLM           config.LLMConfig
	RunID         string
	RunLabel      string
	RunName       string
}

// NewAgent builds an Agent for a single run.
func NewAgent(log zerolog.Logger, krn Kronk, opts Options) *Agent {
	if opts.LLM.ToolChoice == "" {
		opts.LLM.ToolChoice = "auto"
	}
	log = log.With().Str("component", "agent").Logger()
	return &Agent{
		log:           log,
		krn:           krn,
		maxIterations: opts.MaxIterations,
		chatTimeout:   opts.ChatTimeout,
		totalTimeout:  opts.TotalTimeout,
		llmConfig:     opts.LLM,
		runID:         opts.RunID,
		runLabel:      opts.RunLabel,
		runName:       opts.RunName,
		totals:        runTotals{ToolCalls: map[string]int{}, ToolFailures: map[string]int{}},
	}
}

// Execute runs the agent to completion, bounded by totalTimeout and
// maxIterations, and returns the first error that aborts the run. It owns the
// iteration loop, compacting the context before each round, and prints the
// final command output or assistant answer when the loop ends.
func (a *Agent) Execute(ctx context.Context, conf config.Config, cli *multipass.Client) (err error) {
	start := time.Now()
	defer func() {
		// The loop below ends either because the agent finished or because it ran
		// out of iterations. Reaching the cap is not exhausted on its own: a
		// clean finish that happens to land on the last permitted iteration also
		// leaves iteration == maxIterations.
		exhausted := a.iteration >= a.maxIterations && !a.done
		a.recordRunSummary(ctx, a.totals, classifyOutcome(ctx, err, exhausted), time.Since(start))
	}()

	ctx, cancel := context.WithTimeout(ctx, a.totalTimeout)
	defer cancel()

	ctx = withRunIdentity(ctx, a.runLabel, a.runName)

	a.toolDocs = buildToolDocuments(conf)
	a.conversation = buildInitialConversation(conf)
	a.toolResultBudget = toolResultBudgetBytes(conf, a.krn.ModelConfig().ContextWindow())

	if conf.Output.Enabled {
		a.store = output.NewStore(cli, conf.VM.Name, config.OutputRunDir(a.runID), conf.Output, int64(a.toolResultBudget), conf.AllowedCommands)
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
		a.totals.Iterations++
	}

	if len(a.commandOutputs) > 0 {
		final := finalCommandOutput(a.commandOutputs)
		a.log.Info().Str("out", textutil.Truncate(final, conf.Truncation.CommandOutput)).Msg("final command output")
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
	iterCtx = withIteration(iterCtx, iter)
	iterStart := time.Now()
	defer func() { recordIterationDuration(iterCtx, time.Since(iterStart)) }()

	iterLog := a.log.With().Ctx(iterCtx).Logger()

	iterLog.Debug().Int("iter", iter+1).Msg("running inference")

	resp, err := a.callChat(iterCtx, buildChatRequest(a.conversation, a.toolDocs, a.llmConfig), iter)
	if err != nil {
		mphotel.FailSpan(iterSpan, err)
		iterSpan.End()
		return err
	}

	// The reported usage is the measurement; anything this turn appends on top
	// of it is tallied by addToolTokens below.
	recordTokenUsage(iterCtx, a, iterSpan, iterLog, resp.Usage)
	a.contextTokens = 0
	if resp.Usage != nil {
		a.contextTokens = int64(resp.Usage.TotalTokens)
	}
	// Measured before the assistant message lands, so a.conversation is still
	// exactly what the request carried.
	a.fixedOverhead = a.measureFixedOverhead(iterCtx, resp.Usage)

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

	toolResponses, newOutputs := a.executeToolCalls(iterCtx, cli, conf, toolCalls)
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
		contextTokensGauge.Record(ctx, a.contextTokens, metric.WithAttributes(runAttrs(ctx)...))
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
		"text": textutil.Truncate(text, maxContent),
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

	callCtx = mphotel.InjectTracing(callCtx, getAgentTracer(),
		eventAttrs(ctx, attribute.Int("mph.iteration", iter+1))...)

	start := time.Now()
	resp, err := a.krn.Chat(callCtx, req)
	if chatDurationHist != nil {
		chatDurationHist.Record(callCtx, time.Since(start).Seconds(), metric.WithAttributes(eventAttrs(callCtx)...))
	}
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
	a.log.Info().Int("iter", iter+1).Str("finish", finishReason).Msg("agent finished without tool calls")
	return true
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

func (a *Agent) executeToolCalls(ctx context.Context, cli *multipass.Client, cfg config.Config, toolCalls []model.ResponseToolCall) ([]model.D, []string) {
	var toolResponses []model.D
	var commandOutputs []string

	span := trace.SpanFromContext(ctx)
	for _, tc := range toolCalls {
		start := time.Now()
		result, err := Call(ctx, cli, cfg, a.store, tc.Function.Name, map[string]any(tc.Function.Arguments))
		duration := time.Since(start)

		status := "SUCCESS"
		detail := result
		var content string

		if err == nil {
			mphotel.LogEvent(ctx, a.log, zerolog.InfoLevel, "tool succeeded", map[string]any{
				"tool": tc.Function.Name,
				"id":   tc.ID,
				"out":  textutil.Truncate(result, cfg.Truncation.ToolResult),
			})
		}

		switch {
		case err != nil:
			status = "FAILED"
			detail = err.Error()
			mphotel.LogEvent(ctx, a.log, zerolog.ErrorLevel, "tool execution failed", map[string]any{
				"tool":  tc.Function.Name,
				"id":    tc.ID,
				"error": err,
			})
			content = buildToolErrorContent(err)
			if toolFailuresCounter != nil {
				toolFailuresCounter.Add(ctx, 1, metric.WithAttributes(eventAttrs(ctx,
					attribute.String("mph.tool.name", tc.Function.Name))...))
			}
			a.totals.ToolFailures[tc.Function.Name]++
		case isToolEnvelope(result):
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
		default:
			if tc.Function.Name == "multipass_exec" {
				commandOutputs = append(commandOutputs, result)
				mphotel.LogEvent(ctx, a.log, zerolog.InfoLevel, "command output", map[string]any{
					"command_output": textutil.Truncate(result, cfg.Truncation.CommandOutput),
				})
			}
			content = buildToolSuccessContent(tc.Function.Name, result)
		}

		span.AddEvent("mph.agent.tool_result", trace.WithAttributes(
			attribute.String("mph.tool.name", tc.Function.Name),
			attribute.String("mph.tool.id", tc.ID),
			attribute.String("mph.tool.status", status),
			attribute.String("mph.tool.result", textutil.Truncate(detail, cfg.Truncation.ToolResult)),
			attribute.Int64("mph.tool.duration_ms", duration.Milliseconds()),
		))

		if toolCallsCounter != nil {
			toolCallsCounter.Add(ctx, 1, metric.WithAttributes(eventAttrs(ctx,
				attribute.String("mph.tool.name", tc.Function.Name))...))
		}
		a.totals.ToolCalls[tc.Function.Name]++
		if toolDurationHist != nil {
			toolDurationHist.Record(ctx, duration.Seconds(), metric.WithAttributes(eventAttrs(ctx,
				attribute.String("mph.tool.name", tc.Function.Name),
				attribute.String("mph.tool.status", status))...))
		}

		toolResponses = append(toolResponses, buildToolResponseMessage(tc, content))
	}

	return toolResponses, commandOutputs
}
