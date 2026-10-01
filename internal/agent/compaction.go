package agent

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/ardanlabs/kronk/sdk/kronk/model"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/trollLemon/MPHarness/internal/compact"
	"github.com/trollLemon/MPHarness/internal/config"
	mphotel "github.com/trollLemon/MPHarness/internal/otel"
	"github.com/trollLemon/MPHarness/internal/output"
)

type agentSummarizer struct {
	krn         Kronk
	chatTimeout time.Duration
}

func (s *agentSummarizer) Summarize(ctx context.Context, systemPrompt, renderedHistory string, maxTokens int) (string, error) {
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
		"max_tokens":  maxTokens,
		// Thinking would spend the summary's own token budget on <think> and
		// return no content, which compaction reports as outcome=failed.
		"enable_thinking": false,
	}
	resp, err := s.krn.Chat(callCtx, req)
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", nil
	}
	msg := resp.Choices[0].Message
	if msg == nil {
		msg = resp.Choices[0].Delta
	}
	if msg == nil {
		return "", nil
	}
	if msg.Content != "" {
		return msg.Content, nil
	}

	return "", errors.New("summary response contained no `content` tokens")
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

// measureFixedOverhead splits a request's reported prompt cost into the messages
// and the per-request overhead around them (tool schemas and template framing).
// Only the overhead survives compaction, so it has to be carried into the
// post-compaction figure instead of being assumed away.
func (a *Agent) measureFixedOverhead(ctx context.Context, usage *model.Usage) int64 {
	if usage != nil && usage.PromptTokens > 0 {
		estimate := func(s string) int { return a.estimateTokens(ctx, s) }
		if n := int64(usage.PromptTokens) - int64(compact.EstimateConversation(a.conversation, estimate)); n > 0 {
			return n
		}
	}
	// Usage without a prompt count, or an estimate that already overshoots it.
	return a.schemaOverheadTokens(ctx)
}

// schemaOverheadTokens is the floor for the overhead: every request carries the
// tool schemas, whatever the reported usage says.
func (a *Agent) schemaOverheadTokens(ctx context.Context) int64 {
	schemas, err := json.Marshal(a.toolDocs)
	if err != nil {
		return 0
	}
	return int64(a.estimateTokens(ctx, string(schemas)))
}

func outputHandleInfos(handles []output.Handle) []compact.HandleInfo {
	infos := make([]compact.HandleInfo, 0, len(handles))
	for _, h := range handles {
		infos = append(infos, compact.HandleInfo{
			ID: h.ID, TotalLines: h.TotalLines,
			TotalBytes: h.TotalBytes, Command: h.Command,
		})
	}
	return infos
}

// maybeCompact rewrites the conversation in place when the measured context has
// reached the threshold, and returns the outcome. All state it reads and
// updates lives on the Agent.
func (a *Agent) maybeCompact(ctx context.Context, cfg config.Config) string {
	cc := cfg.Compaction
	if !cc.Enabled || a.compactionAttempts >= cc.MaxAttempts {
		return ""
	}
	window := a.krn.ModelConfig().ContextWindow()
	if !compact.ShouldCompact(a.contextTokens, int64(window), cc.Threshold, len(a.conversation), cc.MinMessages) {
		return ""
	}
	var infos []compact.HandleInfo
	if a.store != nil {
		infos = outputHandleInfos(a.store.Handles())
	}

	compCtx, compSpan := getAgentTracer().Start(ctx, "mph.context.compaction",
		trace.WithAttributes(runSpanAttrs(ctx, attribute.Int("mph.iteration", a.iteration+1))...))
	defer compSpan.End()

	// eventAttrs reads the iteration from ctx, and compCtx is about to become
	// the summarizer's chat context, so the labelled tracer has to be built
	// from the caller's context rather than the derived one.
	compCtx = mphotel.InjectTracing(compCtx, getAgentTracer(), eventAttrs(ctx)...)

	start := time.Now()
	summarizer := &agentSummarizer{krn: a.krn, chatTimeout: a.chatTimeout}
	estimate := func(s string) int { return a.estimateTokens(compCtx, s) }
	overhead := a.schemaOverheadTokens(compCtx)
	before := a.contextTokens
	win := compact.Window{Size: window, FixedOverhead: int(overhead)}
	newConv, outcome, summaryLen := compact.Compact(compCtx, a.conversation, infos, cc.MaxSummaryTokens, a.toolResultBudget, win, summarizer, estimate, a.compactionsApplied)
	duration := time.Since(start)

	after := before
	summaryBytes := summaryLen
	if outcome == "applied" && len(newConv) == 2 {
		after = overhead
		for _, m := range newConv {
			if c, _ := m["content"].(string); c != "" {
				after += int64(estimate(c))
			}
		}
		a.conversation = newConv
		a.contextTokens = after
	}
	compSpan.SetAttributes(
		attribute.String("mph.context.compaction.outcome", outcome),
		attribute.Int64("mph.context.tokens.before", before),
		attribute.Int64("mph.context.tokens.after", after),
		attribute.Int("mph.context.compaction.summary_bytes", summaryBytes),
		attribute.Int("mph.context.compaction.handles", len(infos)),
		attribute.Int("mph.context.compactions_applied", a.compactionsApplied),
	)
	compLog := a.log.With().Ctx(compCtx).Logger()
	mphotel.LogEvent(compCtx, compLog, zerolog.WarnLevel, "context compaction", map[string]any{
		"iter": a.iteration + 1, "outcome": outcome, "tok_before": before, "tok_after": after,
		"thr": cc.Threshold, "summary": summaryBytes, "handles": len(infos),
		"compacted": a.compactionsApplied,
		"try":       a.compactionAttempts + 1, "dur_ms": duration.Milliseconds(),
	})
	if compactionsCounter != nil {
		compactionsCounter.Add(compCtx, 1, metric.WithAttributes(eventAttrs(compCtx,
			attribute.String("mph.context.compaction.outcome", outcome))...))
	}
	if compactionDurationHist != nil {
		compactionDurationHist.Record(compCtx, duration.Seconds(), metric.WithAttributes(eventAttrs(compCtx,
			attribute.String("mph.context.compaction.outcome", outcome))...))
	}

	switch outcome {
	case "applied":
		// Rewriting history invalidates the inference prefix cache. Accepted:
		// one re-prefill of a small prompt buys back thousands of tokens.
		if tokensReclaimedHist != nil {
			tokensReclaimedHist.Record(compCtx, before-after, metric.WithAttributes(eventAttrs(compCtx)...))
		}
		if contextTokensGauge != nil {
			contextTokensGauge.Record(compCtx, after, metric.WithAttributes(runAttrs(compCtx)...))
		}
		a.compactionsApplied++
		// A compaction that worked proves the summarizer and handover are
		// healthy, so the failure budget is restored. Otherwise one failure
		// anywhere in a long run disables compaction for good.
		a.compactionAttempts = 0

		mphotel.LogEvent(compCtx, compLog, zerolog.InfoLevel, "context compaction", map[string]any{
			"compacted": newConv,
		})

		return outcome
	case "reverted", "skipped":
		// Not worth it yet (or nothing to do): the next attempt with a longer
		// history may win, so this must not consume the failure budget.
		return outcome
	default:
		a.compactionAttempts++
		return outcome
	}
}
