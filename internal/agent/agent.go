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

	"github.com/trollLemon/MPHarness/internal/config"
	"github.com/trollLemon/MPHarness/internal/lxd"
	"github.com/trollLemon/MPHarness/internal/otlp"
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

	heartbeatInterval = 5 * time.Second
)

// Every structured log line meant for dashboards carries one of these as its
// "event" field, so LogQL can select a line by kind rather than by message text.
const (
	eventTokenUsage               = "token_usage"
	eventIteration                = "iteration"
	eventAgentToolCall            = "agent_tool_call"
	eventToolSucceeded            = "tool_succeeded"
	eventToolFailed               = "tool_failed"
	eventToolOutputTooLarge       = "tool_output_too_large"
	eventContextOverflow          = "context_overflow"
	eventContextCompaction        = "context_compaction"
	eventContextCompactionApplied = "context_compaction_applied"
	eventRunSummary               = "run_summary"
)

type iterationKey struct{}

// withIteration stores iter+1 because iteration labels are 1-based while the
// agent's loop counter is 0-based; converting here keeps the two from drifting.
func withIteration(ctx context.Context, iter int) context.Context {
	return context.WithValue(ctx, iterationKey{}, iter+1)
}

func iterationFromContext(ctx context.Context) (int, bool) {
	v, ok := ctx.Value(iterationKey{}).(int)
	return v, ok
}

func heartbeatWithCancel(log zerolog.Logger, msg string) func() {
	ticker := time.NewTicker(heartbeatInterval)
	done := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ticker.C:
				log.Info().Msg(msg)
			case <-done:
				return
			}
		}
	}()

	return func() {
		ticker.Stop()
		close(done)
		wg.Wait()
	}
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

// argInt reads an integer tool argument. kronk decodes tool arguments through
// json.Decoder.UseNumber, so a JSON integer arrives as json.Number; float64
// covers callers that build arguments in Go.
func argInt(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0
		}
		return int(n)
	case float64:
		return int(v)
	case int:
		return v
	case json.RawMessage:
		var n int
		if err := json.Unmarshal(v, &n); err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// Agent comprises of the state of a single run, including the logger, model, conversation, and metrics.
type Agent struct {
	log           zerolog.Logger   // Run-scoped logger, already tagged component=agent.
	krn           Kronk            // Model backend: chat, tokenize, and model metadata.
	maxIterations int              // Hard cap on inference rounds before the run stops.
	chatTimeout   time.Duration    // Budget for one chat call; guards a stalled model.
	totalTimeout  time.Duration    // Budget for the whole run; bounds every iteration.
	llmConfig     config.LLMConfig // Sampling and output limits sent with each request.
	runID         string           // Bare uuid: the output directory name.

	conversation       []model.D     // Full message history, including tool results.
	toolDocs           []model.D     // Tool schemas sent alongside every request.
	store              *output.Store // Captured output handles, or nil when disabled.
	contextTokens      int64         // Live context size, driving compaction.
	iteration          int           // Zero-based index of the round now running.
	compactionAttempts int           // Failed compactions; budget for the current one.
	compactionsApplied int           // Compactions that succeeded this run.
	toolResultBudget   int           // Byte cutoff between inline output and a handle.
	done               bool          // Set when the agent should stop looping.
	taskStatusNudged   bool          // Set after prompting a model that stopped without task_status.
	totals             runTotals     // Per-run aggregate for the run_summary event.

	finalRunStatus TaskStatusOutcome // final status of the run.
}

// Options carries the per-run bounds NewAgent needs.
type Options struct {
	MaxIterations int
	ChatTimeout   time.Duration
	TotalTimeout  time.Duration
	LLM           config.LLMConfig
	RunID         string
}

// NewAgent builds an Agent for a single run. The run's telemetry identity
// arrives on the context Execute is called with, not through Options.
func NewAgent(log zerolog.Logger, krn Kronk, opts Options) *Agent {
	opts.LLM = opts.LLM.WithDefaults()
	log = log.With().Str("component", "agent").Logger()
	return &Agent{
		log:           log,
		krn:           krn,
		maxIterations: opts.MaxIterations,
		chatTimeout:   opts.ChatTimeout,
		totalTimeout:  opts.TotalTimeout,
		llmConfig:     opts.LLM,
		runID:         opts.RunID,
	}
}

// Execute runs the agent to completion, bounded by totalTimeout and
// maxIterations, and returns the first error that aborts the run. It owns the
// iteration loop, compacting the context before each round.
func (a *Agent) Execute(ctx context.Context, conf config.Config, cli *lxd.Client) (_ *TaskStatusOutcome, err error) {
	start := time.Now()
	defer func() {
		exhausted := a.iteration >= a.maxIterations && !a.done
		a.logRunSummary(ctx, classifyOutcome(ctx, err, exhausted, a.finalRunStatus), time.Since(start))
	}()

	ctx, cancel := context.WithTimeout(ctx, a.totalTimeout)
	defer cancel()

	a.toolDocs = buildToolDocuments(conf)
	a.conversation = buildInitialConversation(conf)
	a.toolResultBudget = toolResultBudgetBytes(conf, a.krn.ModelConfig().ContextWindow())

	if conf.Output.Enabled {
		a.store = output.NewStore(cli, conf.VM.Name, config.OutputRunDir(a.runID), conf.Output, int64(a.toolResultBudget))
	}

	a.log.Info().Int("tools", len(a.toolDocs)).Msg("tool documents built")
	a.log.Info().Str("run", a.runID).Msg("Starting inference")

	for ; a.iteration < a.maxIterations && !a.done; a.iteration++ {
		// Counted before the round runs, so an iteration that aborts the run is
		// still tallied and cannot disagree with outcome="error".
		a.totals.Iterations++
		a.maybeCompact(ctx, conf)
		if err := a.runIteration(ctx, cli, conf); err != nil {
			return nil, err
		}
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

	return &a.finalRunStatus, nil
}

func (a *Agent) runIteration(ctx context.Context, cli *lxd.Client, conf config.Config) error {
	iter := a.iteration

	iterCtx := withIteration(ctx, iter)
	iterLog := a.log.With().Ctx(iterCtx).Logger()

	var chatDur, toolDur time.Duration
	iterStart := time.Now()
	defer func() {
		otlp.LogEvent(iterCtx, iterLog, zerolog.DebugLevel, "iteration timing", map[string]any{
			"event":   eventIteration,
			"iter":    iter + 1,
			"dur_ms":  time.Since(iterStart).Milliseconds(),
			"chat_ms": chatDur.Milliseconds(),
			"tool_ms": toolDur.Milliseconds(),
		})
	}()

	iterLog.Debug().Int("iter", iter+1).Msg("running inference")

	heartbeatLog := iterLog.With().Str("run", a.runID).Int("iter", iter+1).Logger()
	resp, chatDur, err := func() (model.ChatResponse, time.Duration, error) {
		cancel := heartbeatWithCancel(heartbeatLog, "still generating")
		defer cancel()
		return a.chatWithOverflowRecovery(iterCtx, iter, iterLog)
	}()

	if err != nil {
		return err
	}

	a.recordTokenUsage(iterCtx, iterLog, resp.Usage)
	a.contextTokens = 0
	if resp.Usage != nil {
		a.contextTokens = int64(resp.Usage.TotalTokens)
	}

	msg, finishReason, shouldBreak := a.extractAssistantMessage(resp)
	if shouldBreak {
		a.done = true
		return nil
	}

	content := logModelOutput(iterCtx, iterLog, iter, msg, conf.Truncation.LogContent)
	otlp.LogEvent(iterCtx, iterLog, zerolog.DebugLevel, "assistant message", map[string]any{
		"iter":   iter + 1,
		"finish": finishReason,
		"clen":   len(msg.Content),
		"rlen":   len(msg.Reasoning),
		"calls":  len(msg.ToolCalls),
	})
	if content == "" && finishReason != model.FinishReasonTool && len(msg.ToolCalls) == 0 {
		otlp.LogEvent(iterCtx, iterLog, zerolog.DebugLevel, "empty content", map[string]any{
			"finish": finishReason,
		})
	}

	toolCalls := msg.ToolCalls
	argsJSON := marshalToolCallArgs(toolCalls)

	if len(toolCalls) == 0 && finishReason == model.FinishReasonLength {
		otlp.LogEvent(iterCtx, iterLog, zerolog.WarnLevel, "hit length limit without tool calls, injecting nudge and continuing", map[string]any{
			"iter":   iter + 1,
			"finish": finishReason,
		})
		a.conversation = append(a.conversation, buildNudgeMessages(msg, conf.Truncation.Nudge, lengthNudge)...)
		return nil
	}

	if len(toolCalls) == 0 && a.iteration+1 < a.maxIterations && a.finalRunStatus == (TaskStatusOutcome{}) {
		a.taskStatusNudged = true
		otlp.LogEvent(iterCtx, iterLog, zerolog.WarnLevel, "finished without task_status, injecting nudge and continuing", map[string]any{
			"iter":   iter + 1,
			"finish": finishReason,
		})
		a.conversation = append(a.conversation, buildNudgeMessages(msg, conf.Truncation.Nudge, taskStatusNudge)...)
		return nil
	}

	if a.shouldTerminateWithoutToolCalls(toolCalls, finishReason, iter) {
		a.done = true
		return nil
	}

	toolCallDocs := a.buildToolCallDocsWithContext(iterCtx, toolCalls, argsJSON, iter)
	a.conversation = append(a.conversation, buildAssistantMessage(msg, toolCallDocs, conf.Agent.Reasoning()))

	toolStart := time.Now()
	toolResponses := a.executeToolCalls(iterCtx, cli, conf, toolCalls)
	toolDur = time.Since(toolStart)
	a.conversation = append(a.conversation, toolResponses...)
	a.addToolTokens(iterCtx, toolResponses)

	return nil
}

func (a *Agent) addToolTokens(ctx context.Context, msgs []model.D) {
	var batch strings.Builder
	for _, m := range msgs {
		c, _ := m["content"].(string)
		batch.WriteString(c)
		batch.WriteByte('\n')
	}
	a.contextTokens += int64(a.estimateTokens(ctx, batch.String()))
}

func (a *Agent) recordTokenUsage(ctx context.Context, log zerolog.Logger, usage *model.Usage) {
	if usage == nil {
		return
	}
	a.totals.PromptTokens += int64(usage.PromptTokens)
	a.totals.CompletionTokens += int64(usage.CompletionTokens)
	fields := map[string]any{
		"event":             eventTokenUsage,
		"prompt_tokens":     usage.PromptTokens,
		"completion_tokens": usage.CompletionTokens,
		"total_tokens":      usage.TotalTokens,
		"tps":               usage.TokensPerSecond,
		// Logged even when 0: a measured 0 means the inference cache reused
		// nothing this turn, which is a real signal rather than a gap.
		"cached_tokens":    usage.PromptTokensDetails.CachedTokens,
		"reasoning_tokens": usage.CompletionTokensDetails.ReasoningTokens,
		"ttft_ms":          usage.TimeToFirstTokenMS,
	}
	if iter, ok := iterationFromContext(ctx); ok {
		fields["iter"] = iter
	}
	if window := a.krn.ModelConfig().ContextWindow(); window > 0 {
		fields["ctx_window"] = window
	}
	otlp.LogEvent(ctx, log, zerolog.InfoLevel, "token usage", fields)
}

func marshalToolCallArgs(toolCalls []model.ResponseToolCall) []json.RawMessage {
	out := make([]json.RawMessage, len(toolCalls))
	for i, tc := range toolCalls {
		out[i], _ = json.Marshal(tc.Function.Arguments)
	}
	return out
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
	otlp.LogEvent(ctx, log, zerolog.InfoLevel, "model output", map[string]any{
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

	resp, err := a.krn.Chat(callCtx, req)
	if err != nil {
		return model.ChatResponse{}, fmt.Errorf("chat iteration %d: %w", iter+1, err)
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

func (a *Agent) buildToolCallDocsWithContext(ctx context.Context, toolCalls []model.ResponseToolCall, args []json.RawMessage, iter int) []model.D {
	docs := make([]model.D, 0, len(toolCalls))
	for i, tc := range toolCalls {
		otlp.LogEvent(ctx, a.log, zerolog.InfoLevel, "agent tool call", map[string]any{
			"event": eventAgentToolCall,
			"iter":  iter + 1,
			"tool":  tc.Function.Name,
			"id":    tc.ID,
			"args":  args[i],
		})

		docs = append(docs, model.D{
			"id":   tc.ID,
			"type": "function",
			"function": model.D{
				"name":      tc.Function.Name,
				"arguments": string(args[i]),
			},
		})
	}
	return docs
}

func (a *Agent) executeToolCalls(ctx context.Context, cli *lxd.Client, cfg config.Config, toolCalls []model.ResponseToolCall) []model.D {
	var toolResponses []model.D

	iter, _ := iterationFromContext(ctx)
	for _, tc := range toolCalls {

		var duration time.Duration
		var err error
		var result string

		// This function call has no go code to execute and return output to the agent, and is a special
		// case where we need to modify state, not return output to the model (unless its an error)
		if tc.Function.Name == "task_status" {

			var status TaskStatusOutcome
			status, err = parseTaskStatus(tc.Function.Arguments)
			if err == nil {
				a.finalRunStatus = status
				a.done = true
			}

		} else {
			start := time.Now()
			result, err = Call(ctx, cli, cfg, a.store, tc.Function.Name, tc.Function.Arguments)
			duration = time.Since(start)
		}

		resultSize := len(result)

		var content string

		a.totals.ToolCalls++
		env, isEnvelope := parseToolEnvelope(result)
		switch {
		case err != nil:
			content = buildToolErrorContent(err)
			resultSize = len(content)
			a.totals.ToolFailures++
		case isEnvelope:
			if env.Data.Result.Captured {
				id := env.Data.Result.OutputID
				otlp.LogEvent(ctx, a.log, zerolog.InfoLevel, "command output too large for inline context, captured to file", map[string]any{
					"tool":   tc.Function.Name,
					"id":     tc.ID,
					"out_id": id,
				})
			}
			content = result
		default:
			content = buildToolSuccessContent(tc.Function.Name, result)
		}

		if a.toolResultBudget > 0 && resultSize > a.toolResultBudget {
			content = buildContextOverflowContent(tc.Function.Name, resultSize, a.toolResultBudget, a.store != nil)
			if err == nil {
				err = fmt.Errorf("tool result too large: %d bytes over the %d-byte context budget", resultSize, a.toolResultBudget)
				a.totals.ToolFailures++
			}
			otlp.LogEvent(ctx, a.log, zerolog.WarnLevel, "tool output too large", map[string]any{
				"event":  eventToolOutputTooLarge,
				"iter":   iter,
				"tool":   tc.Function.Name,
				"id":     tc.ID,
				"bytes":  resultSize,
				"budget": a.toolResultBudget,
			})
		}

		if err != nil {
			otlp.LogEvent(ctx, a.log, zerolog.ErrorLevel, "tool execution failed", map[string]any{
				"event":  eventToolFailed,
				"iter":   iter,
				"tool":   tc.Function.Name,
				"id":     tc.ID,
				"dur_ms": duration.Milliseconds(),
				"error":  err,
			})
		} else {
			otlp.LogEvent(ctx, a.log, zerolog.InfoLevel, "tool succeeded", map[string]any{
				"event":  eventToolSucceeded,
				"iter":   iter,
				"tool":   tc.Function.Name,
				"id":     tc.ID,
				"dur_ms": duration.Milliseconds(),
			})
		}

		toolResponses = append(toolResponses, buildToolResponseMessage(tc, content))

		if a.done {
			break
		}
	}

	return toolResponses
}

type agentSummarizer struct {
	krn         Kronk
	chatTimeout time.Duration
	maxTokens   int
}

func (s *agentSummarizer) Summarize(ctx context.Context, systemPrompt, renderedHistory string) (string, error) {
	// Dedicated Chat, not an agent turn: no tools, temperature 0, bounded
	// input. It never touches the run's conversation slice.
	callCtx, cancel := context.WithTimeout(ctx, s.chatTimeout)
	defer cancel()
	req := model.D{
		"messages": []model.D{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": renderedHistory},
		},
		"temperature": 0,
		"max_tokens":  s.maxTokens,
		// Thinking would spend the summary's own token budget on <think> and
		// return no content, which compaction reports as outcome=failed.
		"enable_thinking": false,
	}
	resp, err := s.krn.Chat(callCtx, req)
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("%w (no choices)", errEmptySummary)
	}
	choice := resp.Choices[0]
	msg := choice.Message
	if msg == nil {
		msg = choice.Delta
	}
	if msg != nil && msg.Content != "" {
		return msg.Content, nil
	}
	return "", fmt.Errorf("%w (finish_reason=%s)", errEmptySummary, choice.FinishReason())
}

func (a *Agent) estimateTokens(ctx context.Context, text string) int {
	if text == "" {
		return 0
	}
	if resp, err := a.krn.Tokenize(ctx, model.D{"input": text}); err == nil && resp.Tokens > 0 {
		return resp.Tokens
	}
	return len(text) / bytesPerToken
}

// schemaOverheadTokens is the floor for the overhead: every request carries the
// tool schemas, whatever the reported usage says.
func (a *Agent) schemaOverheadTokens(ctx context.Context) int64 {
	schemas, _ := json.Marshal(a.toolDocs)
	return int64(a.estimateTokens(ctx, string(schemas)))
}

func outputHandleInfos(handles []output.Handle) []handleInfo {
	infos := make([]handleInfo, 0, len(handles))
	for _, h := range handles {
		infos = append(infos, handleInfo{
			ID: h.ID, TotalLines: h.TotalLines,
			TotalBytes: h.TotalBytes, Command: h.Command,
		})
	}
	return infos
}

// maybeCompact rewrites the conversation in place when the measured context has
// reached the threshold. All state it reads and updates lives on the Agent.
func (a *Agent) maybeCompact(ctx context.Context, cfg config.Config) {
	cc := cfg.Compaction
	if !cc.Enabled || a.compactionAttempts >= cc.MaxAttempts {
		return
	}
	window := a.krn.ModelConfig().ContextWindow()
	if !shouldCompact(a.contextTokens, int64(window), cc.Threshold) {
		return
	}
	var infos []handleInfo
	if a.store != nil {
		infos = outputHandleInfos(a.store.Handles())
	}

	compCtx := ctx

	start := time.Now()
	summarizer := &agentSummarizer{krn: a.krn, chatTimeout: a.chatTimeout, maxTokens: cc.MaxSummaryTokens}
	overhead := a.schemaOverheadTokens(compCtx)
	before := a.contextTokens
	win := compactionWindow{Size: window, FixedOverhead: int(overhead)}

	heartbeatLog := a.log.With().Ctx(compCtx).Str("run", a.runID).Int("iter", a.iteration+1).Str("phase", "compaction").Logger()
	newConv, outcome, summaryLen, compactErr := func() ([]model.D, string, int, error) {
		cancel := heartbeatWithCancel(heartbeatLog, "still compacting")
		defer cancel()
		return compactConversation(compCtx, a.conversation, infos, cc.MaxSummaryTokens, a.toolResultBudget, win, summarizer, a.compactionsApplied)
	}()
	duration := time.Since(start)

	after := before
	summaryBytes := summaryLen
	if outcome == "applied" && len(newConv) == 2 {
		after = overhead
		for _, m := range newConv {
			if c, _ := m["content"].(string); c != "" {
				after += int64(a.estimateTokens(compCtx, c))
			}
		}
		a.conversation = newConv
		a.contextTokens = after
	}
	compLog := a.log.With().Ctx(compCtx).Logger()
	a.logCompaction(compCtx, compLog, cc, before, after, summaryBytes, len(infos), duration, outcome, compactErr)

	switch outcome {
	case "applied":
		// Rewriting history invalidates the inference prefix cache. Accepted:
		// one re-prefill of a small prompt buys back thousands of tokens.
		a.totals.ReclaimedTokens += before - after
		a.compactionsApplied++
		// A compaction that worked proves the summarizer and handover are
		// healthy, so the failure budget is restored. Otherwise one failure
		// anywhere in a long run disables compaction for good.
		a.compactionAttempts = 0

		otlp.LogEvent(compCtx, compLog, zerolog.InfoLevel, "context compaction", map[string]any{
			"event":     eventContextCompactionApplied,
			"iter":      a.iteration + 1,
			"compacted": newConv,
		})

		return
	default:
		// "failed": the summarizer could not produce an acceptable handover.
		a.compactionAttempts++
		return
	}
}

func (a *Agent) logCompaction(ctx context.Context, log zerolog.Logger, cc config.CompactionConfig, before, after int64, summaryBytes, handles int, duration time.Duration, outcome string, compactErr error) {
	fields := map[string]any{
		"event": eventContextCompaction,
		"iter":  a.iteration + 1, "outcome": outcome, "tok_before": before, "tok_after": after,
		"reclaimed": before - after, "thr": cc.Threshold, "summary": summaryBytes, "handles": handles,
		"compacted": a.compactionsApplied,
		"try":       a.compactionAttempts + 1, "dur_ms": duration.Milliseconds(),
	}
	if compactErr != nil {
		fields["error"] = compactErr
	}
	otlp.LogEvent(ctx, log, zerolog.WarnLevel, "context compaction", fields)
}
