package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ardanlabs/kronk/sdk/kronk/model"
	"github.com/rs/zerolog"

	"github.com/trollLemon/MPHarness/internal/config"
	"github.com/trollLemon/MPHarness/internal/output"
)

func bigToolStep(content string, total int) model.ChatResponse {
	return chatResp(content, "", model.FinishReasonTool, []model.ResponseToolCall{
		{ID: "c1", Type: "function", Function: model.ResponseToolCallFunction{Name: "vm_info", Arguments: model.ToolCallArguments{}}},
	}, &model.Usage{TotalTokens: total})
}

func countHandovers(mock *mockKronk) int {
	handovers := 0
	for _, req := range mock.captured {
		if isCompactionRequest(req) {
			handovers++
		}
	}
	return handovers
}

type windowEnforcingKronk struct {
	*mockKronk
	overWindow int
}

func promptTokens(req model.D) int {
	n := 0
	if msgs, ok := req["messages"].([]model.D); ok {
		for _, m := range msgs {
			if c, _ := m["content"].(string); c != "" {
				n += len(c) / bytesPerToken
			}
		}
	}
	if tools, ok := req["tools"].([]model.D); ok {
		for _, d := range tools {
			b, _ := json.Marshal(d)
			n += len(b) / bytesPerToken
		}
	}
	return n
}

func (w *windowEnforcingKronk) Chat(ctx context.Context, req model.D) (model.ChatResponse, error) {
	n := promptTokens(req)
	if n > w.contextWidth {
		if !isCompactionRequest(req) {
			w.overWindow++
		}
		return model.ChatResponse{}, fmt.Errorf("%w: input tokens [%d] exceed context window [%d]", model.ErrInvalidRequest, n, w.contextWidth)
	}
	resp, err := w.mockKronk.Chat(ctx, req)
	if err == nil && len(resp.Choices) > 0 {
		completion := 0
		if resp.Usage != nil {
			completion = resp.Usage.CompletionTokens
		}
		resp.Usage = &model.Usage{PromptTokens: n, CompletionTokens: completion, TotalTokens: n + completion}
	}
	return resp, err
}

func enabledCompaction() config.CompactionConfig {
	return config.CompactionConfig{
		Enabled: true, Threshold: 0.8, MaxSummaryTokens: 512,
		MaxAttempts: 3,
	}
}

func TestExecuteCompaction(t *testing.T) {
	big := strings.Repeat("s", 4000)
	final := chatResp("final", "", model.FinishReasonStop, nil, &model.Usage{TotalTokens: 1050})
	tests := []struct {
		name         string
		runID        string
		responses    []model.ChatResponse
		failHandover bool
		compaction   config.CompactionConfig
		maxIter      int
		check        func(t *testing.T, mock *mockKronk, logs string)
	}{
		{
			name:       "fires at threshold",
			runID:      "run-compact-1",
			responses:  []model.ChatResponse{bigToolStep(big, 4000), bigToolStep(big, 7500), final},
			compaction: enabledCompaction(),
			maxIter:    5,
			check: func(t *testing.T, mock *mockKronk, _ string) {
				if mock.calls < 3 {
					t.Fatalf("want compaction summary call + 2 iterations, got %d chat calls", mock.calls)
				}
			},
		},
		{
			name:      "disabled skips",
			runID:     "run-no-compact",
			responses: []model.ChatResponse{chatResp("final", "", model.FinishReasonStop, nil, &model.Usage{TotalTokens: 7500})},
			compaction: config.CompactionConfig{
				Enabled: false, Threshold: 0.8, MaxSummaryTokens: 512,
				MaxAttempts: 3,
			},
			maxIter: 3,
			check: func(t *testing.T, mock *mockKronk, _ string) {
				if mock.calls != 1 {
					t.Fatalf("disabled compaction must not add chat calls, got %d", mock.calls)
				}
			},
		},
		{
			name:  "failures stop after max attempts",
			runID: "run-fail",
			responses: []model.ChatResponse{
				bigToolStep(big, 7500), bigToolStep(big, 7500), bigToolStep(big, 7500),
				bigToolStep(big, 7500), bigToolStep(big, 7500), bigToolStep(big, 7500),
				chatResp("final", "", model.FinishReasonStop, nil, nil),
			},
			failHandover: true,
			compaction:   enabledCompaction(),
			maxIter:      7,
			check: func(t *testing.T, mock *mockKronk, logs string) {
				if got := countHandovers(mock); got != 3 {
					t.Fatalf("want exactly maxAttempts handover calls, got %d", got)
				}
				if !strings.Contains(logs, "context deadline exceeded") {
					t.Fatal("failed compaction must log its reason")
				}
			},
		},
		{
			name:  "run stats logged",
			runID: "run-stats",
			responses: []model.ChatResponse{
				bigToolStep(strings.Repeat("z", 3000), 7500),
				chatResp("final", "", model.FinishReasonStop, nil, &model.Usage{TotalTokens: 1050}),
			},
			compaction: func() config.CompactionConfig {
				c := enabledCompaction()
				return c
			}(),
			maxIter: 5,
			check: func(t *testing.T, _ *mockKronk, logs string) {
				for _, want := range []string{`"run complete"`, `"compacted":1`, `"ctx_pct"`} {
					if !strings.Contains(logs, want) {
						t.Errorf("missing %q in run stats log", want)
					}
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			mock := &mockKronk{contextWidth: 8192, responses: tt.responses, failHandover: tt.failHandover}
			agt := NewAgent(zerolog.New(&buf), mock, Options{
				MaxIterations: tt.maxIter,
				ChatTimeout:   time.Second,
				TotalTimeout:  30 * time.Second,
				LLM:           defaultLLMConfig(),
				RunID:         tt.runID,
			})
			conf := config.Config{
				VM:         config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
				Model:      "m",
				Prompt:     "task",
				Compaction: tt.compaction,
			}
			if err := agt.Execute(context.Background(), conf, newTestClient(t)); err != nil {
				t.Fatalf("compaction failure must not end the run: %v", err)
			}
			tt.check(t, mock, buf.String())
		})
	}
}

func TestCompactionEventReportsContextDrop(t *testing.T) {
	var buf bytes.Buffer
	big := strings.Repeat("s", 4000)
	mock := &mockKronk{
		contextWidth: 8192,
		responses: []model.ChatResponse{
			bigToolStep(big, 4000),
			bigToolStep(big, 7500),
			chatResp("final", "", model.FinishReasonStop, nil, &model.Usage{TotalTokens: 1050}),
		},
	}
	agt := NewAgent(zerolog.New(&buf), mock, Options{
		MaxIterations: 5,
		ChatTimeout:   time.Second,
		TotalTimeout:  30 * time.Second,
		LLM:           defaultLLMConfig(),
		RunID:         "run-compaction-drop",
	})
	conf := config.Config{
		VM:         config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:      "m",
		Prompt:     "task",
		Compaction: enabledCompaction(),
	}
	if err := agt.Execute(context.Background(), conf, newTestClient(t)); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if countHandovers(mock) == 0 {
		t.Fatalf("no compaction summary call ran; the event would not prove compaction")
	}
	events := decodeEvents(t, &buf)
	var applied map[string]any
	for _, e := range eventsOf(events, eventContextCompaction) {
		if e["outcome"] == "applied" {
			applied = e
		}
	}
	if applied == nil {
		t.Fatalf("no applied context_compaction event in %v", eventsOf(events, eventContextCompaction))
	}
	before, after := applied["tok_before"].(float64), applied["tok_after"].(float64)
	if after >= 7500 || after < 0 {
		t.Fatalf("tok_after = %v, want 0 <= tok_after < 7500", after)
	}
	if got := applied["reclaimed"]; got != before-after {
		t.Errorf("reclaimed = %v, want tok_before-tok_after = %v", got, before-after)
	}
	if len(eventsOf(events, eventContextCompactionApplied)) != 1 {
		t.Errorf("want exactly one context_compaction_applied event")
	}
}

func TestAppliedCompactionResetsAttempts(t *testing.T) {
	mock := &mockKronk{contextWidth: 8192}
	agt := NewAgent(zerolog.Nop(), mock, Options{
		MaxIterations: 5,
		ChatTimeout:   time.Second,
		TotalTimeout:  30 * time.Second,
		LLM:           defaultLLMConfig(),
		RunID:         "run-reset",
	})
	conf := config.Config{
		VM:     config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:  "m",
		Prompt: "task",
		Compaction: config.CompactionConfig{
			Enabled: true, Threshold: 0.8, MaxSummaryTokens: 512,
			MaxAttempts: 3,
		},
	}
	big := strings.Repeat("s", 4000)
	conv := []model.D{
		{"role": "system", "content": "sys"},
		{"role": "user", "content": "task"},
		{"role": "assistant", "content": big},
		{"role": "user", "content": big},
		{"role": "assistant", "content": big},
	}
	mock.responses = []model.ChatResponse{chatResp(validHandoverSummary, "", model.FinishReasonStop, nil, nil)}

	agt.conversation, agt.contextTokens, agt.toolResultBudget = conv, 7500, 4096
	agt.compactionAttempts = 2
	agt.maybeCompact(context.Background(), conf)
	if agt.compactionsApplied != 1 {
		t.Fatalf("want applied, got %d compactions", agt.compactionsApplied)
	}
	if agt.compactionAttempts != 0 {
		t.Fatalf("a successful compaction must restore the failure budget, attempts=%d", agt.compactionAttempts)
	}
}

func TestPostCompactionContextKeepsToolSchemaOverhead(t *testing.T) {
	big := strings.Repeat("s", 4000)
	base := []model.D{
		{"role": "system", "content": "sys"},
		{"role": "user", "content": "task"},
		{"role": "assistant", "content": big},
		{"role": "user", "content": big},
		{"role": "assistant", "content": big},
	}

	tests := []struct {
		name         string
		schemas      int
		wantOverhead bool
	}{
		{name: "no tool schemas", schemas: 0},
		{name: "one small schema", schemas: 1},
		// Big enough that dropping the schemas from the post-compaction figure
		// would be obvious rather than a rounding difference.
		{name: "eight large schemas", schemas: 8, wantOverhead: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockKronk{contextWidth: 8192, responses: []model.ChatResponse{
				chatResp(validHandoverSummary, "", model.FinishReasonStop, nil, nil)}}
			agt := NewAgent(zerolog.Nop(), mock, Options{
				MaxIterations: 5, ChatTimeout: time.Second, TotalTimeout: 30 * time.Second,
				LLM: defaultLLMConfig(), RunID: "run-overhead",
			})
			conf := config.Config{
				VM:    config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
				Model: "m", Prompt: "task", Compaction: enabledCompaction(),
			}
			for i := 0; i < tt.schemas; i++ {
				agt.toolDocs = append(agt.toolDocs, model.D{"type": "function", "function": model.D{
					"name": "vm_tool", "description": strings.Repeat("d", 2000),
				}})
			}
			schemaJSON, err := json.Marshal(agt.toolDocs)
			if err != nil {
				t.Fatalf("marshal tool docs: %v", err)
			}
			schemaTokens := int64(agt.estimateTokens(context.Background(), string(schemaJSON)))

			agt.conversation, agt.contextTokens, agt.toolResultBudget = base, 7500, 4096
			agt.maybeCompact(context.Background(), conf)
			if agt.compactionsApplied != 1 {
				t.Fatalf("want applied, got %d compactions", agt.compactionsApplied)
			}
			if !tt.wantOverhead {
				return
			}
			if agt.contextTokens <= schemaTokens {
				t.Fatalf("post-compaction context %d omits the ~%d tokens of tool schemas every request still pays",
					agt.contextTokens, schemaTokens)
			}
		})
	}
}

func TestCompactionSeesToolResultBeforeNextModelCall(t *testing.T) {
	const window = 32768
	big := strings.Repeat("z", 12000)

	cfg := config.Config{
		VM:         config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model:      "m",
		Prompt:     "task",
		Compaction: enabledCompaction(),
		Output:     config.OutputConfig{Enabled: true, Mode: "auto", MaxCommandSize: 1 << 20, MaxTotalSize: 1 << 26, SearchMaxMatches: 200},
	}
	// The mock has to report what the conversation it was handed actually costs,
	// or the guard is deciding on numbers no real run produces.
	initial := buildInitialConversation(cfg)
	agt := NewAgent(zerolog.Nop(), &mockKronk{contextWidth: window}, Options{
		MaxIterations: 3,
		ChatTimeout:   time.Second,
		TotalTimeout:  30 * time.Second,
		LLM:           defaultLLMConfig(),
		RunID:         "run-grow",
	})
	agt.toolDocs = buildToolDocuments(cfg)
	agt.toolResultBudget = toolResultBudgetBytes(cfg, window)
	// ~24000 tokens of history, so the context is already near the 0.80
	// threshold and the one tool result is what tips it over.
	const fillerBytes = 4800
	const fillerTurns = 10
	filler := strings.Repeat("h", fillerBytes)
	for i := 0; i < fillerTurns; i++ {
		agt.conversation = append(agt.conversation,
			model.D{"role": "assistant", "content": filler},
			model.D{"role": "user", "content": filler},
		)
	}
	// Reported usage is the message content the request carried.
	reported := int64((2*fillerTurns*fillerBytes + len(initial[0]["content"].(string))) / bytesPerToken)
	threshold := int64(cfg.Compaction.Threshold * float64(window))

	mock := &mockKronk{contextWidth: window, responses: []model.ChatResponse{
		chatResp("", "", model.FinishReasonTool, []model.ResponseToolCall{{
			ID: "c1", Type: "function",
			Function: model.ResponseToolCallFunction{
				Name: "vm_exec", Arguments: model.ToolCallArguments{"command": "cat big", "capture": true},
			},
		}}, &model.Usage{TotalTokens: int(reported), PromptTokens: int(reported) - 10, CompletionTokens: 10}),
	}}
	agt.krn = mock

	f := &fakeOutputExec{fn: func(name, cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "grep -a -c"):
			return "0", nil
		case strings.Contains(cmd, "__MPH_EXIT_"):
			return exitMarker(cmd) + "0", nil
		case strings.Contains(cmd, "cat "):
			return big, nil
		case strings.Contains(cmd, "wc -c"):
			return strconv.Itoa(len(big)), nil
		case strings.Contains(cmd, "wc -l"):
			return "1", nil
		}
		return "", nil
	}}
	// Cutoff must match what NewAgent passes the store in production.
	store := output.NewStore(f, "vm", "/tmp/mph-output/run1", cfg.Output, int64(agt.toolResultBudget))
	agt.store = store

	if reported >= threshold {
		t.Fatalf("setup: reported %d must sit below the %d threshold, or there is nothing to react to", reported, threshold)
	}
	if err := agt.runIteration(context.Background(), newTestClient(t), cfg); err != nil {
		t.Fatalf("runIteration: %v", err)
	}

	// The reply was fine to send, but the tool result it triggered is what
	// pushes the real context over. If the measurement stops at the reported
	// usage, the next iteration calls the model before compacting.
	if agt.contextTokens <= reported {
		t.Fatalf("context %d must include the tool result appended after the reply reported %d, or the next model call goes out over budget", agt.contextTokens, reported)
	}
	if agt.contextTokens < threshold {
		t.Fatalf("context %d must reach the %d threshold for the next call to be oversized", agt.contextTokens, threshold)
	}
	agt.maybeCompact(context.Background(), cfg)
	if agt.compactionsApplied != 1 {
		t.Fatalf("compaction must fire before the next model call, got %d compactions", agt.compactionsApplied)
	}
	if agt.contextTokens >= threshold {
		t.Fatalf("compaction did not reclaim context: %d still over budget", agt.contextTokens)
	}
}

func TestCompactionAppliesBeforeWindowIsExceeded(t *testing.T) {
	base := config.Config{
		VM:    config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Model: "m",
		Prompt: "Inside the VM:\n" +
			"1. Run uname -a and note the kernel release field.\n" +
			"2. Write that release string into /tmp/steps.txt.\n" +
			"3. Append the output of hostname as a second line.\n",
		LLM: config.LLMConfig{Temperature: 0.0, ToolChoice: "auto"},
		Output: config.OutputConfig{
			Enabled: true, Mode: "auto", InlineMaxSize: 0,
			MaxCommandSize: 64 << 20, MaxTotalSize: 512 << 20, SearchMaxMatches: 200,
		},
	}
	tests := []struct {
		name      string
		window    int
		threshold float64
		stepBytes int
		steps     int
	}{
		{name: "small window", window: 4096, threshold: 0.71, stepBytes: 1400, steps: 8},
		// An auto-tuned window, which is what testing/compaction.yaml asks for.
		{name: "auto tuned window", window: 32768, threshold: 0.71, stepBytes: 6000, steps: 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			cfg.LLM.ContextWindow = tt.window
			cfg.Compaction = config.CompactionConfig{
				Enabled: true, Threshold: tt.threshold, MaxSummaryTokens: 512,
				MaxAttempts: 3,
			}
			step := strings.Repeat("k", tt.stepBytes)
			responses := make([]model.ChatResponse, 0, tt.steps+1)
			for i := 0; i < tt.steps; i++ {
				responses = append(responses, bigToolStep(step, 0))
			}
			responses = append(responses, chatResp("done", "", model.FinishReasonStop, nil, &model.Usage{TotalTokens: 400}))

			mock := &windowEnforcingKronk{mockKronk: &mockKronk{contextWidth: tt.window, responses: responses}}
			agt := NewAgent(zerolog.Nop(), mock, Options{
				MaxIterations: tt.steps + 4, ChatTimeout: time.Second, TotalTimeout: 30 * time.Second,
				LLM: cfg.LLM, RunID: "run-window",
			})
			if err := agt.Execute(context.Background(), cfg, newTestClient(t)); err != nil {
				t.Fatalf("run grew past the window without compacting: %v (handovers=%d)", err, countHandovers(mock.mockKronk))
			}
			if agt.compactionsApplied == 0 {
				t.Fatalf("no compaction applied; handovers=%d", countHandovers(mock.mockKronk))
			}
			if mock.overWindow != 0 {
				t.Fatalf("%d chat requests exceeded the %d-token window before compaction; recovery masked it", mock.overWindow, tt.window)
			}
		})
	}
}

// slowSummarizerKronk makes the summarizer's chat call take measurable time, so
// a compaction duration is large enough to tell seconds from
// milliseconds.
type slowSummarizerKronk struct {
	*mockKronk
}

func (s *slowSummarizerKronk) Chat(ctx context.Context, req model.D) (model.ChatResponse, error) {
	if isCompactionRequest(req) {
		time.Sleep(50 * time.Millisecond)
		return chatResp(validHandoverSummary, "", model.FinishReasonStop, nil, nil), nil
	}
	return s.mockKronk.Chat(ctx, req)
}

// The field is named dur_ms, so a value recorded in seconds would read 1000x
// too small on the dashboard instead of failing.
func TestCompactionDurationIsLoggedInMilliseconds(t *testing.T) {
	var buf bytes.Buffer
	big := strings.Repeat("s", 4000)
	mock := &slowSummarizerKronk{mockKronk: &mockKronk{contextWidth: 8192, responses: []model.ChatResponse{
		bigToolStep(big, 7500), chatResp("final", "", model.FinishReasonStop, nil, &model.Usage{TotalTokens: 800}),
	}}}
	agent := NewAgent(zerolog.New(&buf), mock, Options{
		MaxIterations: 3, ChatTimeout: time.Second, TotalTimeout: 30 * time.Second,
		LLM: defaultLLMConfig(), RunID: "run-compact-ms",
	})
	conf := config.Config{
		VM:         config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"},
		Prompt:     "task",
		Compaction: enabledCompaction(),
	}
	if err := agent.Execute(context.Background(), conf, newTestClient(t)); err != nil {
		t.Fatalf("execute: %v", err)
	}

	compactions := eventsOf(decodeEvents(t, &buf), eventContextCompaction)
	if len(compactions) == 0 {
		t.Fatal("no context_compaction event was logged")
	}
	for _, e := range compactions {
		// The summarizer sleeps 50ms, so milliseconds land near 50 and seconds near 0.
		if d := e["dur_ms"].(float64); d < 50 || d > 5000 {
			t.Errorf("dur_ms = %v, want ~50 for a 50ms summarizer call", d)
		}
	}
}

type fakeSummarizer struct {
	summary string
	err     error
}

func (f *fakeSummarizer) Summarize(_ context.Context, _, _ string, _ int) (string, error) {
	return f.summary, f.err
}

func conv(n int) []model.D {
	out := []model.D{{"role": "system", "content": "sys"}, {"role": "user", "content": "task: do things"}}
	for i := 0; i < n-2; i++ {
		out = append(out, model.D{"role": "assistant", "content": "step"})
	}
	return out
}

func TestShouldCompact(t *testing.T) {
	tests := []struct {
		name      string
		tokens    int64
		window    int64
		threshold float64
		want      bool
	}{
		{"at threshold", 8000, 10000, 0.8, true},
		{"below threshold", 7999, 10000, 0.8, false},
		{"unknown window", 9000, 0, 0.8, false},
		{"zero usage", 0, 10000, 0.8, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldCompact(tt.tokens, tt.window, tt.threshold); got != tt.want {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}

func summaryWith(text string) string {
	return "## Done\n- " + text + "\n\n## In progress\n- None.\n\n## Not started\n- None."
}

func TestCompactHandover(t *testing.T) {
	tests := []struct {
		name         string
		conversation []model.D
		handles      []handleInfo
		summary      string
		applied      int
		wantOutcome  string
		wantSystem   string
		wantSubs     []string
	}{
		{
			name: "rewrite keeps system and task",
			conversation: []model.D{
				{"role": "system", "content": "SYS-BYTES"},
				{"role": "user", "content": "Task:\nrun ls"},
				{"role": "assistant", "content": "doing"},
				{"role": "tool", "name": "vm_exec", "content": "out"},
				{"role": "assistant", "content": "done"},
				{"role": "tool", "name": "vm_exec", "content": "out2"},
			},
			handles:     []handleInfo{{ID: "oid-1", TotalLines: 10, TotalBytes: 100, Command: "cat big"}},
			summary:     summaryWith("did stuff"),
			wantOutcome: "applied",
			wantSystem:  "SYS-BYTES",
			wantSubs:    []string{"## Task", "run ls", "did stuff", "oid-1", "after 0 compactions", "never invent one"},
		},
		{
			name:         "handles survive when the model omits them",
			conversation: conv(8),
			handles:      []handleInfo{{ID: "must-survive", TotalLines: 5, TotalBytes: 50, Command: "ls"}},
			summary:      summaryWith("no handles mentioned"),
			wantOutcome:  "applied",
			wantSubs:     []string{"must-survive"},
		},
		{
			name:         "handover carries the compaction count",
			conversation: conv(8),
			summary:      summaryWith("did stuff"),
			applied:      3,
			wantOutcome:  "applied",
			wantSubs:     []string{"after 3 compactions", "do not call output_search or output_read"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, outcome, _, _ := compactConversation(context.Background(), tt.conversation, tt.handles, 2048, 4000, compactionWindow{Size: 1 << 20}, &fakeSummarizer{summary: tt.summary}, tt.applied)
			if outcome != tt.wantOutcome {
				t.Fatalf("outcome %q want %q", outcome, tt.wantOutcome)
			}
			if len(got) != 2 {
				t.Fatalf("want exactly 2 messages, got %d", len(got))
			}
			if tt.wantSystem != "" && got[0]["content"] != tt.wantSystem {
				t.Fatalf("system must be byte-for-byte, got %v", got[0]["content"])
			}
			body, _ := got[1]["content"].(string)
			for _, want := range tt.wantSubs {
				if !strings.Contains(body, want) {
					t.Fatalf("missing %q in %q", want, body)
				}
			}
		})
	}
}

func TestFailureKeepsOriginal(t *testing.T) {
	tests := []struct {
		name    string
		summary string
		err     error
	}{
		{"summarizer error", "", context.DeadlineExceeded},
		{"blank summary", "  ", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conversation := conv(8)
			got, outcome, _, _ := compactConversation(context.Background(), conversation, nil, 2048, 4000, compactionWindow{Size: 1 << 20}, &fakeSummarizer{summary: tt.summary, err: tt.err}, 0)
			if outcome != "failed" {
				t.Fatalf("want failed, got %q", outcome)
			}
			if len(got) != len(conversation) {
				t.Fatalf("want original kept, got len %d want %d", len(got), len(conversation))
			}
		})
	}
}

type sequenceSummarizer struct {
	summaries []string
	errs      []error
	calls     int
	systems   []string
	rendered  []string
}

func (s *sequenceSummarizer) Summarize(_ context.Context, systemPrompt, renderedHistory string, _ int) (string, error) {
	s.calls++
	s.systems = append(s.systems, systemPrompt)
	s.rendered = append(s.rendered, renderedHistory)
	idx := s.calls - 1
	var summary string
	var err error
	if idx < len(s.summaries) {
		summary = s.summaries[idx]
	}
	if idx < len(s.errs) {
		err = s.errs[idx]
	}
	return summary, err
}

type countingSummarizer struct {
	summary   string
	calls     int
	maxTokens []int
	rendered  []string
}

func (f *countingSummarizer) Summarize(_ context.Context, _, renderedHistory string, maxTokens int) (string, error) {
	f.calls++
	f.maxTokens = append(f.maxTokens, maxTokens)
	f.rendered = append(f.rendered, renderedHistory)
	return f.summary, nil
}

func TestCompactRepairsRejectedSummary(t *testing.T) {
	sum := &sequenceSummarizer{summaries: []string{"too short", validSummary}}
	_, outcome, _, err := compactConversation(context.Background(), conv(8), nil, 2048, 4000,
		compactionWindow{Size: 1 << 20}, sum, 0)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if outcome != "applied" {
		t.Fatalf("outcome %q, want applied", outcome)
	}
	if sum.calls != 2 {
		t.Fatalf("calls = %d, want 2 (initial + repair)", sum.calls)
	}
	if !strings.Contains(sum.systems[1], "rejected") {
		t.Errorf("repair system prompt must name the rejection: %q", sum.systems[1])
	}
	if !strings.Contains(sum.rendered[1], "Previous rejected handover") || !strings.Contains(sum.rendered[1], "too short") {
		t.Errorf("repair history must carry the rejected summary: %q", sum.rendered[1])
	}
}

func TestCompactRepairsEmptySummary(t *testing.T) {
	sum := &sequenceSummarizer{
		summaries: []string{"", validSummary},
		errs:      []error{errEmptySummary, nil},
	}
	_, outcome, _, err := compactConversation(context.Background(), conv(8), nil, 2048, 4000,
		compactionWindow{Size: 1 << 20}, sum, 0)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if outcome != "applied" {
		t.Fatalf("outcome %q, want applied", outcome)
	}
	if sum.calls != 2 {
		t.Fatalf("calls = %d, want 2", sum.calls)
	}
	if !strings.Contains(sum.systems[1], "no content") {
		t.Errorf("repair prompt must explain the empty summary: %q", sum.systems[1])
	}
}

func TestCompactReportsFailureReason(t *testing.T) {
	tests := []struct {
		name string
		sum  *sequenceSummarizer
		want string
	}{
		{
			name: "short twice",
			sum:  &sequenceSummarizer{summaries: []string{"too short", "too short"}},
			want: "summary rejected after repair",
		},
		{
			name: "summarizer error",
			sum:  &sequenceSummarizer{errs: []error{context.DeadlineExceeded}, summaries: []string{""}},
			want: "summarize",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, outcome, _, err := compactConversation(context.Background(), conv(8), nil, 2048, 4000,
				compactionWindow{Size: 1 << 20}, tt.sum, 0)
			if outcome != "failed" {
				t.Fatalf("outcome %q, want failed", outcome)
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestSummarizerInputFitsWindow(t *testing.T) {
	longResult := strings.Repeat("out", 4000)
	conversation := []model.D{
		{"role": "system", "content": "sys"},
		{"role": "user", "content": "Task:\nrun ls"},
	}
	for i := 0; i < 4; i++ {
		conversation = append(conversation,
			model.D{"role": "assistant", "content": "calling", "tool_calls": []model.D{
				{"function": model.D{"name": "vm_exec", "arguments": `{"command":"ls"}`}}}},
			model.D{"role": "tool", "name": "vm_exec", "content": longResult},
		)
	}
	est := func(s string) int { return len(s)/4 + 1 }

	tests := []struct {
		name          string
		window        int
		budget        int
		wantRendered  int
		wantOversized bool
	}{
		{name: "budget larger than the window", window: 4096, budget: 2048, wantRendered: 4000},
		{name: "window several times the budget", window: 32768, budget: 2048, wantRendered: 4000},
		{name: "window barely fits the prompt", window: 1024, budget: 2048, wantRendered: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sum := &countingSummarizer{summary: summaryWith("listed the requested output")}
			_, _, _, _ = compactConversation(context.Background(), conversation, nil, 512, tt.budget,
				compactionWindow{Size: tt.window}, sum, 0)

			if sum.calls != 1 {
				t.Fatalf("want 1 summarizer call, got %d", sum.calls)
			}
			if used := est(compactionSystemPrompt) + est(sum.rendered[0]); used > tt.window {
				t.Fatalf("summarizer prompt is %d tokens, over the %d window", used, tt.window)
			}
			if len(sum.rendered[0]) < tt.wantRendered {
				t.Fatalf("render is %d bytes, want at least %d; the bound must not gut the history",
					len(sum.rendered[0]), tt.wantRendered)
			}
		})
	}
}

func TestBuildUserMessageGuidesHandles(t *testing.T) {
	tests := []struct {
		name     string
		handles  []handleInfo
		applied  int
		wantSubs []string
	}{
		{
			name:     "no handles forbids output tools",
			applied:  1,
			wantSubs: []string{"after 1 compaction", "do not call output_search or output_read", "Completed: x"},
		},
		{
			name:     "listed handles must not be invented",
			handles:  []handleInfo{{ID: "oid-9", TotalLines: 3, TotalBytes: 30, Command: "ls"}},
			wantSubs: []string{"oid-9", "never invent one", "after 0 compactions"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildUserMessage("do things", "Completed: x. Remaining: y.", "", tt.handles, tt.applied)
			for _, want := range tt.wantSubs {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q in %q", want, got)
				}
			}
		})
	}
}

func TestTruncateHeadKeepsValidUTF8(t *testing.T) {
	tests := []struct {
		name string
		s    string
		n    int
	}{
		{"cut lands mid rune", strings.Repeat("a", 5) + "日本語テキスト", 8},
		{"cut lands on rune start", strings.Repeat("a", 5) + "日本語テキスト", 9},
		{"emoji surrogate pair", strings.Repeat("a", 3) + "🙂🙂🙂", 5},
		{"no truncation", "short", 100},
		{"zero budget", "anything", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateHead(tt.s, tt.n)
			if !utf8.ValidString(got) {
				t.Fatalf("invalid UTF-8: %q", got)
			}
			if body := strings.TrimSuffix(got, "…(truncated)"); !strings.HasPrefix(tt.s, body) {
				t.Fatalf("truncation is not a prefix of the input: %q", got)
			}
		})
	}
}

func TestRenderHistory(t *testing.T) {
	longToolResult := []model.D{
		{"role": "assistant", "content": "calling", "tool_calls": []model.D{
			{"function": model.D{"name": "vm_exec", "arguments": `{"command":"ls"}`}}}},
		{"role": "tool", "name": "vm_exec", "content": strings.Repeat("z", 5000)},
	}
	manyTurns := make([]model.D, 0, 800)
	for i := 0; i < 400; i++ {
		manyTurns = append(manyTurns, model.D{
			"role": "assistant", "content": strings.Repeat("a", 1000),
			"reasoning_content": strings.Repeat("r", 1000),
		})
	}

	tests := []struct {
		name         string
		conversation []model.D
		budget       int
		maxBytes     int
		maxRendered  int
		want         []string
	}{
		{
			// Prose is the summarizer's only view of what the agent was doing,
			// but a single rambling message still has to respect the budget.
			name:         "assistant prose respects the per-message budget",
			conversation: []model.D{{"role": "assistant", "content": strings.Repeat("a", 100000)}},
			budget:       100, maxBytes: 1 << 20,
			maxRendered: 500,
		},
		{
			name:         "user text respects the per-message budget",
			conversation: []model.D{{"role": "user", "content": strings.Repeat("u", 100000)}},
			budget:       100, maxBytes: 1 << 20,
			maxRendered: 500,
		},
		{
			// budget is per-message, so without a total bound a long
			// conversation renders to len(messages)*budget and the summarizer
			// prompt grows without limit.
			name:         "total render is bounded by maxBytes",
			conversation: manyTurns,
			budget:       1000, maxBytes: 8000,
			maxRendered: 8000,
		},
		{
			// The summarizer has to see what a command actually returned.
			name:         "full text keeps tool output in the summarizer view",
			conversation: longToolResult,
			budget:       100, maxBytes: 1 << 20,
			want: []string{"vm_exec", "zzz"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := renderHistory(tt.conversation, tt.budget, tt.maxBytes)
			if tt.maxRendered > 0 && len(got) > tt.maxRendered {
				t.Fatalf("rendered %d bytes, over the %d bound", len(got), tt.maxRendered)
			}
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q in %q", want, got)
				}
			}
		})
	}
}

func TestCompactionSystemPromptAsksForResumableState(t *testing.T) {
	for _, want := range []string{"## Done", "## In progress", "## Not started", "even when a section is empty", "None.", "Before replying"} {
		if !strings.Contains(compactionSystemPrompt, want) {
			t.Errorf("compactionSystemPrompt missing %q", want)
		}
	}
}

func TestCompactionSystemPromptProtectsHandleIDs(t *testing.T) {
	for _, want := range []string{"output_id", "verbatim"} {
		if !strings.Contains(compactionSystemPrompt, want) {
			t.Errorf("compactionSystemPrompt missing %q", want)
		}
	}
}

func TestBuildUserMessageCarriesNoVerbatimTail(t *testing.T) {
	got := buildUserMessage("do things", "Done: x", "", nil, 1)
	if strings.Contains(got, "## Recent activity") {
		t.Fatalf("handover still carries a verbatim recent block: %q", got)
	}
}

const validSummary = `## Done
- Ran curl --version; observed curl 8.5.0 (x86_64-pc-linux-gnu).
- GET https://example.com returned HTTP/2 200 with "Example Domain" in the body.

## In progress
- None.

## Not started
- None.`

func TestCompactRejectsDegenerateSummary(t *testing.T) {
	conversation := conv(8)
	got, outcome, summaryLen, _ := compactConversation(context.Background(), conversation, nil, 2048, 4000,
		compactionWindow{Size: 1 << 20}, &fakeSummarizer{summary: "too short"}, 0)
	if outcome != "failed" {
		t.Fatalf("want failed for a short summary, got %q", outcome)
	}
	if len(got) != len(conversation) {
		t.Fatalf("degenerate summary must keep the original history, got %d messages", len(got))
	}
	if summaryLen != 0 {
		t.Fatalf("rejected summary must not report bytes, got %d", summaryLen)
	}
}

func TestCompactAcceptsSummaryWithoutHeadings(t *testing.T) {
	const summary = "Ran curl --version and observed curl 8.5.0. The download finished, but verification is still pending."
	conversation := conv(8)
	got, outcome, _, _ := compactConversation(context.Background(), conversation, nil, 2048, 4000,
		compactionWindow{Size: 1 << 20}, &fakeSummarizer{summary: summary}, 0)
	if outcome != "applied" {
		t.Fatalf("outcome %q, want applied", outcome)
	}
	body, _ := got[1]["content"].(string)
	if !strings.Contains(body, summary) {
		t.Fatalf("handover lost the summary: %q", body)
	}
}

func TestCompactHandoverCarriesRecentCommandResults(t *testing.T) {
	conversation := []model.D{
		{"role": "system", "content": "sys"},
		{"role": "user", "content": "Task:\nrun curl tests"},
		{"role": "assistant", "content": "", "tool_calls": []model.D{{
			"id": "c1",
			"function": model.D{
				"name": "vm_exec", "arguments": `{"command":"curl -sS -I https://example.com"}`,
			},
		}}},
		{"role": "tool", "name": "vm_exec", "tool_call_id": "c1", "content": "HTTP/2 200"},
	}
	got, outcome, _, _ := compactConversation(context.Background(), conversation, nil, 2048, 4000,
		compactionWindow{Size: 1 << 20}, &fakeSummarizer{summary: validSummary}, 0)
	if outcome != "applied" {
		t.Fatalf("want applied, got %q", outcome)
	}
	body, _ := got[1]["content"].(string)
	for _, want := range []string{"## Recent command results", "curl -sS -I https://example.com", "HTTP/2 200"} {
		if !strings.Contains(body, want) {
			t.Fatalf("handover missing %q in %q", want, body)
		}
	}
}

func TestCompactHandoverRecentResultsAreBounded(t *testing.T) {
	big := strings.Repeat("z", 10000)
	conversation := []model.D{
		{"role": "system", "content": "sys"},
		{"role": "user", "content": "Task:\nrun curl tests"},
		{"role": "assistant", "content": "", "tool_calls": []model.D{{
			"id": "c1",
			"function": model.D{
				"name": "vm_exec", "arguments": `{"command":"cat huge"}`,
			},
		}}},
		{"role": "tool", "name": "vm_exec", "tool_call_id": "c1", "content": big},
	}
	got, outcome, _, _ := compactConversation(context.Background(), conversation, nil, 2048, 4000,
		compactionWindow{Size: 1 << 20}, &fakeSummarizer{summary: validSummary}, 0)
	if outcome != "applied" {
		t.Fatalf("want applied, got %q", outcome)
	}
	body, _ := got[1]["content"].(string)
	if strings.Contains(body, big) {
		t.Fatal("handover copied the entire tool result instead of a bounded excerpt")
	}
	if !strings.Contains(body, "…(truncated)") {
		t.Fatalf("handover must show that the result was truncated: %q", body)
	}
	if len(body) > 4096 {
		t.Fatalf("handover is %d bytes, want evidence and summary bounded", len(body))
	}
}

func TestCompactionSystemPromptRequiresConcreteSections(t *testing.T) {
	for _, want := range []string{
		"## Done", "## In progress", "## Not started",
		"observed result", "only a heading",
	} {
		if !strings.Contains(compactionSystemPrompt, want) {
			t.Errorf("compactionSystemPrompt missing %q", want)
		}
	}
}
