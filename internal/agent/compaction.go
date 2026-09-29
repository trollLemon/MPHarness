package agent

import (
	"context"
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
	return msg.Content, nil
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

func outputHandleInfos(handles []output.Handle) []compact.HandleInfo {
	infos := make([]compact.HandleInfo, 0, len(handles))
	for _, h := range handles {
		infos = append(infos, compact.HandleInfo{
			ID: h.ID, Path: h.Path, TotalLines: h.TotalLines,
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

	start := time.Now()
	summarizer := &agentSummarizer{krn: a.krn, chatTimeout: a.chatTimeout}
	estimate := func(s string) int { return a.estimateTokens(compCtx, s) }
	before := a.contextTokens
	newConv, outcome, summaryLen := compact.Compact(compCtx, a.conversation, infos, cc.MaxSummaryTokens, cc.RecentTurns, a.toolResultBudget, summarizer, estimate, a.compactionsApplied)
	duration := time.Since(start)

	after := before
	summaryBytes := summaryLen
	if outcome == "applied" && len(newConv) == 2 {
		after = 0
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
	compLog.Warn().
		Int("iter", a.iteration+1).
		Int64("tok_before", before).
		Int64("tok_after", after).
		Float64("thr", cc.Threshold).
		Int("summary", summaryBytes).
		Int("handles", len(infos)).
		Int("compacted", a.compactionsApplied).
		Int("try", a.compactionAttempts+1).
		Int64("dur_ms", duration.Milliseconds()).
		Str("outcome", outcome).
		Msg("context compaction")
	mphotel.LogEvent(compCtx, a.log, zerolog.WarnLevel, "context compaction", map[string]any{
		"iter": a.iteration + 1, "outcome": outcome, "tok_before": before, "tok_after": after,
		"thr": cc.Threshold, "summary": summaryBytes, "handles": len(infos),
		"compacted": a.compactionsApplied,
		"try":       a.compactionAttempts + 1, "dur_ms": duration.Milliseconds(),
	})
	if compactionsCounter != nil {
		compactionsCounter.Add(compCtx, 1, metric.WithAttributes(attribute.String("mph.context.compaction.outcome", outcome)))
	}
	if compactionDurationHist != nil {
		compactionDurationHist.Record(compCtx, float64(duration.Milliseconds()), metric.WithAttributes(attribute.String("mph.context.compaction.outcome", outcome)))
	}

	switch outcome {
	case "applied":
		// Rewriting history invalidates the inference prefix cache. Accepted:
		// one re-prefill of a small prompt buys back thousands of tokens.
		if tokensReclaimedHist != nil {
			tokensReclaimedHist.Record(compCtx, before-after)
		}
		if contextTokensGauge != nil {
			contextTokensGauge.Record(compCtx, after)
		}
		a.compactionsApplied++
		// A compaction that worked proves the summarizer and handover are
		// healthy, so the failure budget is restored. Otherwise one failure
		// anywhere in a long run disables compaction for good.
		a.compactionAttempts = 0
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
