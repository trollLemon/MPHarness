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

func (a *Agent) maybeCompact(ctx context.Context, iter int, conversation []model.D, store *output.Store, cfg config.Config, lastContextTokens int64, toolResultBudget, attempts int) ([]model.D, int64, int, string) {
	cc := cfg.Compaction
	if !cc.Enabled || attempts >= cc.MaxAttempts {
		return conversation, lastContextTokens, attempts, ""
	}
	window := a.krn.ModelConfig().ContextWindow()
	if !compact.ShouldCompact(lastContextTokens, int64(window), cc.Threshold, len(conversation), cc.MinMessages) {
		return conversation, lastContextTokens, attempts, ""
	}
	var infos []compact.HandleInfo
	if store != nil {
		infos = outputHandleInfos(store.Handles())
	}

	compCtx, compSpan := getAgentTracer().Start(ctx, "mph.context.compaction",
		trace.WithAttributes(runSpanAttrs(ctx, attribute.Int("mph.iteration", iter+1))...))
	defer compSpan.End()

	start := time.Now()
	summarizer := &agentSummarizer{krn: a.krn, chatTimeout: a.chatTimeout}
	estimate := func(s string) int { return a.estimateTokens(compCtx, s) }
	newConv, outcome := compact.Compact(compCtx, conversation, infos, cc.MaxSummaryTokens, cc.RecentTurns, toolResultBudget, summarizer, estimate)
	duration := time.Since(start)

	after := lastContextTokens
	summaryBytes := 0
	if outcome == "applied" && len(newConv) == 2 {
		after = 0
		for _, m := range newConv {
			if c, _ := m["content"].(string); c != "" {
				after += int64(estimate(c))
			}
		}
		if c, _ := newConv[1]["content"].(string); c != "" {
			summaryBytes = len(c)
		}
	}
	compSpan.SetAttributes(
		attribute.String("mph.context.compaction.outcome", outcome),
		attribute.Int64("mph.context.tokens.before", lastContextTokens),
		attribute.Int64("mph.context.tokens.after", after),
		attribute.Int("mph.context.compaction.summary_bytes", summaryBytes),
		attribute.Int("mph.context.compaction.handles", len(infos)),
	)
	compLog := a.log.With().Ctx(compCtx).Logger()
	compLog.Warn().
		Int64("tokens_before", lastContextTokens).
		Int64("tokens_after", after).
		Float64("threshold", cc.Threshold).
		Int("summary_bytes", summaryBytes).
		Int("handles", len(infos)).
		Int("attempt", attempts+1).
		Int("iteration", iter+1).
		Int64("duration_ms", duration.Milliseconds()).
		Str("outcome", outcome).
		Msg("context compaction")
	mphotel.LogEvent(compCtx, a.log, zerolog.WarnLevel, "context compaction", map[string]any{
		"outcome": outcome, "tokens_before": lastContextTokens, "tokens_after": after,
		"threshold": cc.Threshold, "summary_bytes": summaryBytes, "handles": len(infos),
		"attempt": attempts + 1, "iteration": iter + 1, "duration_ms": duration.Milliseconds(),
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
			tokensReclaimedHist.Record(compCtx, lastContextTokens-after)
		}
		if contextTokensGauge != nil {
			contextTokensGauge.Record(compCtx, after)
		}
		return newConv, after, attempts, outcome
	default:
		return conversation, lastContextTokens, attempts + 1, outcome
	}
}
