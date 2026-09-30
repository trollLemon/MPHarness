package output

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/trollLemon/MPHarness/internal/config"
)

type fakeExec struct {
	fn func(name, command string) (string, error)
}

func (f *fakeExec) Exec(_ context.Context, name, command string, _ int) (string, error) {
	return f.fn(name, command)
}

func testCfg() config.OutputConfig {
	return config.OutputConfig{Enabled: true, Mode: "auto", MaxCommandSize: 64 * 1024 * 1024, MaxTotalSize: 512 * 1024 * 1024, SearchMaxMatches: 200}
}

func TestDecideInline(t *testing.T) {
	tests := []struct {
		name   string
		mode   string
		size   int64
		cutoff int64
		want   bool
	}{
		{"auto small inline", "auto", 50, 100, true},
		{"auto big chunk", "auto", 200, 100, false},
		{"auto at cutoff chunks", "auto", 100, 100, false},
		{"always small chunk", "always", 10, 100, false},
		{"always big chunk", "always", 1 << 20, 100, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decideInline(tt.mode, tt.size, tt.cutoff); got != tt.want {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}

func TestValidatePattern(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		wantErr bool
	}{
		{"valid ERE", "foo.*bar", false},
		{"PCRE digit rejected", `\d+`, true},
		{"invalid regex", "([", true},
		{"empty pattern", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidatePattern(tt.pattern); (err != nil) != tt.wantErr {
				t.Fatalf("ValidatePattern(%q) err = %v, wantErr %v", tt.pattern, err, tt.wantErr)
			}
		})
	}
}

func TestSearchFixedStringSkipsValidation(t *testing.T) {
	f := &fakeExec{fn: func(name, cmd string) (string, error) {
		if strings.Contains(cmd, "grep") {
			return "1:(['ll\n__MPH_GREP_EXIT_abc:0", nil
		}
		return "", nil
	}}
	s := NewStore(f, "vm", "/tmp/mph-output/run1", testCfg(), 4096, nil)
	s.handles["abc"] = Handle{ID: "abc", Path: "/tmp/x/output.txt", TotalLines: 10}

	tests := []struct {
		name        string
		pattern     string
		fixedString bool
		wantMatches int
		wantErr     bool
	}{
		{"regex mode rejects invalid pattern", "([", false, 0, true},
		{"fixed_string skips ERE validation", "([", true, 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := s.Search(context.Background(), SearchArgs{OutputID: "abc", Pattern: tt.pattern, FixedString: tt.fixedString})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want validation error for %q", tt.pattern)
				}
				return
			}
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if res.TotalMatches != tt.wantMatches {
				t.Fatalf("want %d matches, got %+v", tt.wantMatches, res)
			}
		})
	}
}

func TestCapture(t *testing.T) {
	tests := []struct {
		name       string
		command    string
		force      bool
		maxCmd     config.Size
		cancelCtx  bool
		respond    func(cmd string) (string, error)
		wantInline string
		wantHandle bool
		wantLines  int
		wantExit   int
		wantErrSub string
	}{
		{
			name:    "auto no-flag runs direct without touching files",
			command: "echo hi",
			respond: func(cmd string) (string, error) {
				if strings.Contains(cmd, "df -P") || strings.Contains(cmd, "mkdir") {
					t.Fatalf("auto no-flag must not touch files: %q", cmd)
				}
				return "hello", nil
			},
			wantInline: "hello",
		},
		{
			name:    "large output becomes a handle with no split",
			command: "echo hi",
			force:   true,
			respond: func(cmd string) (string, error) {
				switch {
				case strings.Contains(cmd, "df -P"):
					return "99999999", nil
				case strings.Contains(cmd, "mkdir"):
					return "", nil
				case strings.Contains(cmd, "__MPH_EXIT_"):
					return echoMarker(cmd, "0"), nil
				case strings.Contains(cmd, "wc -c"):
					return "7340032", nil
				case strings.Contains(cmd, "wc -l"):
					return "64000", nil
				default:
					if strings.Contains(cmd, "split ") {
						t.Fatalf("chunking must be gone, got %q", cmd)
					}
					return "", fmt.Errorf("unexpected cmd %q", cmd)
				}
			},
			wantHandle: true,
			wantLines:  64000,
		},
		{
			name:    "preflight refusal on low disk",
			command: "echo hi",
			force:   true,
			respond: func(cmd string) (string, error) {
				if strings.Contains(cmd, "df -P") {
					return "1", nil
				}
				return "", nil
			},
			wantErrSub: "free space",
		},
		{
			name:    "command size cap enforced",
			command: "echo hi",
			force:   true,
			maxCmd:  10,
			respond: func(cmd string) (string, error) {
				switch {
				case strings.Contains(cmd, "df -P"):
					return "99999999", nil
				case strings.Contains(cmd, "mkdir"):
					return "", nil
				case strings.Contains(cmd, "__MPH_EXIT_"):
					return echoMarker(cmd, "0"), nil
				case strings.Contains(cmd, "wc -c"):
					return "100", nil
				default:
					return "", nil
				}
			},
			wantErrSub: "exceeds",
		},
		{
			name:    "non-numeric size is a stat error",
			command: "echo hi",
			force:   true,
			respond: func(cmd string) (string, error) {
				switch {
				case strings.Contains(cmd, "df -P"):
					return "99999999", nil
				case strings.Contains(cmd, "mkdir"):
					return "", nil
				case strings.Contains(cmd, "__MPH_EXIT_"):
					return echoMarker(cmd, "0"), nil
				case strings.Contains(cmd, "wc -c"):
					return "not-a-number", nil
				default:
					return "", nil
				}
			},
			wantErrSub: "unexpected size",
		},
		{
			name:      "timed-out command keeps its partial output",
			command:   "sleep 99",
			force:     true,
			cancelCtx: true,
			respond: func(cmd string) (string, error) {
				switch {
				case strings.Contains(cmd, "df -P"):
					return "99999999", nil
				case strings.Contains(cmd, "mkdir"):
					return "", nil
				case strings.Contains(cmd, "__MPH_EXIT_"):
					return "", context.DeadlineExceeded
				case strings.Contains(cmd, "wc -c"):
					return "5000", nil
				case strings.Contains(cmd, "wc -l"):
					return "100", nil
				default:
					if strings.Contains(cmd, "split ") {
						t.Fatalf("chunking must be gone, got %q", cmd)
					}
					return "", nil
				}
			},
			wantHandle: true,
			wantLines:  100,
			wantExit:   124,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.cancelCtx {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			cfg := testCfg()
			if tt.maxCmd > 0 {
				cfg.MaxCommandSize = tt.maxCmd
			}
			s := NewStore(&fakeExec{fn: func(name, cmd string) (string, error) {
				return tt.respond(cmd)
			}}, "vm", "/tmp/mph-output/run1", cfg, 4096, nil)

			res, err := s.Capture(ctx, tt.command, tt.force)
			if tt.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("want error containing %q, got %+v %v", tt.wantErrSub, res, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("capture: %v", err)
			}
			if !tt.wantHandle {
				if res.Handle != nil || res.Inline != tt.wantInline {
					t.Fatalf("got %+v want inline %q", res, tt.wantInline)
				}
				if len(s.Handles()) != 0 {
					t.Fatalf("no handle should be registered")
				}
				return
			}
			if res.Handle == nil {
				t.Fatalf("want handle, got inline %q", res.Inline)
			}
			if res.Handle.TotalLines != tt.wantLines || res.Handle.ExitCode != tt.wantExit {
				t.Fatalf("handle %+v", res.Handle)
			}
			if len(s.Handles()) != 1 {
				t.Fatalf("registry len %d", len(s.Handles()))
			}
		})
	}
}

func TestSearchExitMapping(t *testing.T) {
	tests := []struct {
		name        string
		grepOut     string
		grepErr     bool
		outputID    string
		wantMatches int
		wantLines   []int
		wantErr     bool
	}{
		{
			name:        "line number is already absolute",
			grepOut:     "1513:hit\n__MPH_GREP_EXIT_abc:0",
			outputID:    "abc",
			wantMatches: 1,
			wantLines:   []int{1513},
		},
		{
			name:        "line text may itself contain colons",
			grepOut:     "1:2000001:needle-7x9 TREASURE-LINE\n__MPH_GREP_EXIT_abc:0",
			outputID:    "abc",
			wantMatches: 1,
			wantLines:   []int{1},
		},
		{
			name:        "matches come back sorted",
			grepOut:     "900:a\n4:b\n77:c\n__MPH_GREP_EXIT_abc:0",
			outputID:    "abc",
			wantMatches: 3,
			wantLines:   []int{4, 77, 900},
		},
		{
			name:        "no match yields zero matches",
			grepOut:     "__MPH_GREP_EXIT_abc:1",
			outputID:    "abc",
			wantMatches: 0,
		},
		{
			name:     "grep exit 2 is an error",
			grepOut:  "__MPH_GREP_EXIT_abc:2",
			outputID: "abc",
			wantErr:  true,
		},
		{
			name:     "exec error without marker surfaces",
			grepErr:  true,
			outputID: "abc",
			wantErr:  true,
		},
		{
			name:     "unknown output_id is an error",
			grepOut:  "__MPH_GREP_EXIT_abc:0",
			outputID: "nope",
			wantErr:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeExec{fn: func(name, cmd string) (string, error) {
				if strings.Contains(cmd, "grep") {
					if tt.grepErr {
						return "", fmt.Errorf("multipass exploded")
					}
					return tt.grepOut, nil
				}
				return "", nil
			}}
			s := NewStore(f, "vm", "/tmp/mph-output/run1", testCfg(), 4096, nil)
			s.handles["abc"] = Handle{ID: "abc", Path: "/tmp/x/output.txt", TotalLines: 2000000}

			res, err := s.Search(context.Background(), SearchArgs{OutputID: tt.outputID, Pattern: "hit"})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", res)
				}
				return
			}
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if res.TotalMatches != tt.wantMatches {
				t.Fatalf("want %d matches, got %+v", tt.wantMatches, res)
			}
			if !slices.Equal(res.Matches, tt.wantLines) {
				t.Fatalf("want lines %v, got %v", tt.wantLines, res.Matches)
			}
		})
	}
}

func TestCaptureTrimsAndRejectsBlankCommand(t *testing.T) {
	t.Run("trims", func(t *testing.T) {
		var gotCmd string
		f := &fakeExec{fn: func(name, cmd string) (string, error) {
			gotCmd = cmd
			return "hi", nil
		}}
		s := NewStore(f, "vm", "/tmp/mph-output/run1", testCfg(), 4096, nil)

		res, err := s.Capture(context.Background(), "  echo hi  ", false)
		if err != nil {
			t.Fatalf("Capture: %v", err)
		}
		if res.Inline != "hi" {
			t.Fatalf("inline %q want %q", res.Inline, "hi")
		}
		if gotCmd != "echo hi" {
			t.Fatalf("command must be trimmed before exec, got %q", gotCmd)
		}
	})

	t.Run("blank is an error", func(t *testing.T) {
		f := &fakeExec{fn: func(name, cmd string) (string, error) {
			t.Fatalf("a blank command must not reach the VM: %q", cmd)
			return "", nil
		}}
		s := NewStore(f, "vm", "/tmp/mph-output/run1", testCfg(), 4096, nil)

		if _, err := s.Capture(context.Background(), "   ", true); err == nil {
			t.Fatal("blank command must be rejected")
		}
	})
}

func TestCapturePaths(t *testing.T) {
	subdir, raw := capturePaths("/tmp/mph-output/run1", "oid")
	if subdir != "/tmp/mph-output/run1/raw-oid" || raw != "/tmp/mph-output/run1/raw-oid/output.txt" {
		t.Fatalf("got %q %q", subdir, raw)
	}
}

func TestRunRedirectMissingMarkerIsAnError(t *testing.T) {
	tests := []struct {
		name     string
		execOut  string
		execErr  error
		wantSubs string
	}{
		{"marker absent and exec succeeded", "partial output", nil, "exit marker"},
		{"marker absent and exec failed", "", errors.New("boom"), "boom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeExec{fn: func(name, cmd string) (string, error) { return tt.execOut, tt.execErr }}
			s := NewStore(f, "vm", "/tmp/mph-output/run1", testCfg(), 4096, nil)
			_, err := s.runRedirect(context.Background(), "echo hi", "/tmp/raw/output.txt", "__MPH_EXIT_abc:")
			if err == nil {
				t.Fatal("a missing marker must not report success")
			}
			if !strings.Contains(err.Error(), tt.wantSubs) {
				t.Fatalf("error %q must mention %q", err, tt.wantSubs)
			}
		})
	}
}

func TestRunRedirectMarkerWinsOverExecError(t *testing.T) {
	f := &fakeExec{fn: func(name, cmd string) (string, error) {
		return echoMarker(cmd, "7"), errors.New("wrapper complained")
	}}
	s := NewStore(f, "vm", "/tmp/mph-output/run1", testCfg(), 4096, nil)
	exit, err := s.runRedirect(context.Background(), "echo hi", "/tmp/raw/output.txt", "__MPH_EXIT_abc:")
	if err != nil {
		t.Fatalf("the marker is authoritative: %v", err)
	}
	if exit != 7 {
		t.Fatalf("exit %d want 7", exit)
	}
}

func TestCaptureFailsWhenLineCountUnparseable(t *testing.T) {
	f := &fakeExec{fn: func(name, cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "df -P"):
			return "99999999", nil
		case strings.Contains(cmd, "mkdir"):
			return "", nil
		case strings.Contains(cmd, "__MPH_EXIT_"):
			return echoMarker(cmd, "0"), nil
		case strings.Contains(cmd, "wc -c"):
			return "10", nil
		case strings.Contains(cmd, "wc -l"):
			return "not a number", nil
		default:
			return "", nil
		}
	}}
	cfg := testCfg()
	cfg.Mode = "always"
	s := NewStore(f, "vm", "/tmp/mph-output/run1", cfg, 4096, nil)

	res, err := s.Capture(context.Background(), "echo hi", false)
	if err == nil {
		t.Fatalf("want error on unparseable wc -l, got %+v", res)
	}
	if res.Handle != nil {
		t.Fatalf("no handle should be registered, got %+v", res.Handle)
	}
	if !strings.Contains(err.Error(), "line count") {
		t.Fatalf("error must name the line count failure, got %v", err)
	}
}

type ctxAwareExec struct {
	fn func(ctx context.Context, name, cmd string) (string, error)
}

func (c ctxAwareExec) Exec(ctx context.Context, name, command string, _ int) (string, error) {
	return c.fn(ctx, name, command)
}

func TestCaptureTimeoutKeepsPartialOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var sawCancelledFollowUp bool
	f := ctxAwareExec{fn: func(c context.Context, name, cmd string) (string, error) {
		if strings.Contains(cmd, "__MPH_EXIT_") {
			cancel() // the command's own context is done
			return "", context.DeadlineExceeded
		}
		if c.Err() != nil {
			// A real ExecClient fails here; recording it makes that visible.
			sawCancelledFollowUp = true
			return "", c.Err()
		}
		switch {
		case strings.Contains(cmd, "df -P"):
			return "99999999", nil
		case strings.Contains(cmd, "mkdir"):
			return "", nil
		case strings.Contains(cmd, "wc -c"):
			return "5000", nil
		case strings.Contains(cmd, "wc -l"):
			return "100", nil
		}
		return "", nil
	}}
	s := NewStore(f, "vm", "/tmp/mph-output/run1", testCfg(), 4096, nil)
	res, err := s.Capture(ctx, "sleep 99", true)
	if err != nil {
		t.Fatalf("a timed-out command must still return its partial capture: %v", err)
	}
	if sawCancelledFollowUp {
		t.Fatal("follow-up work reused the cancelled context")
	}
	if res.Handle == nil {
		t.Fatal("want a handle for the partial output")
	}
	if res.Handle.ExitCode != 124 {
		t.Fatalf("exit %d want 124", res.Handle.ExitCode)
	}
	if res.Handle.TotalLines != 100 {
		t.Fatalf("TotalLines %d want 100", res.Handle.TotalLines)
	}
}

func TestCaptureBoundsTheWrite(t *testing.T) {
	var gotCmd string
	f := &fakeExec{fn: func(name, cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "df -P"):
			return "99999999", nil
		case strings.Contains(cmd, "__MPH_EXIT_"):
			gotCmd = cmd
			return echoMarker(cmd, "0"), nil
		case strings.Contains(cmd, "wc -c"):
			return "100", nil
		case strings.Contains(cmd, "wc -l"):
			return "1", nil
		}
		return "", nil
	}}
	cfg := testCfg()
	cfg.MaxCommandSize = 4096
	s := NewStore(f, "vm", "/tmp/mph-output/run1", cfg, 4096, nil)
	if _, err := s.Capture(context.Background(), "yes RUNAWAY", true); err != nil {
		t.Fatalf("capture: %v", err)
	}
	if !strings.Contains(gotCmd, "head -c") {
		t.Fatalf("capture write is unbounded, no byte cap in %q", gotCmd)
	}
	if !strings.Contains(gotCmd, "4097") {
		t.Fatalf("byte cap must be derived from max_command_size plus one, got %q", gotCmd)
	}
}

func TestSearchTotalMatchesIsARealCount(t *testing.T) {
	tests := []struct {
		name      string
		execOut   string
		countOut  string
		maxM      int
		wantTotal int
		wantTrunc bool
	}{
		{"under the cap reports the true count", "1:a\n2:b\n__MPH_GREP_EXIT_abc:0", "2", 200, 2, false},
		{"exactly at the cap is not truncation", "1:a\n2:b\n__MPH_GREP_EXIT_abc:0", "2", 2, 2, false},
		{"over the cap keeps the true total", "1:a\n2:b\n3:c\n__MPH_GREP_EXIT_abc:0", "3", 2, 3, true},
		{"unreadable count falls back to the listed count", "1:a\n2:b\n__MPH_GREP_EXIT_abc:0", "junk", 2, 2, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testCfg()
			cfg.SearchMaxMatches = tt.maxM
			f := &fakeExec{fn: func(name, cmd string) (string, error) {
				if strings.Contains(cmd, "grep -a -c") {
					return tt.countOut, nil
				}
				return tt.execOut, nil
			}}
			s := NewStore(f, "vm", "/tmp/mph-output/run1", cfg, 4096, nil)
			s.handles["abc"] = Handle{ID: "abc", Path: "/tmp/raw/output.txt", TotalLines: 10}
			res, err := s.Search(context.Background(), SearchArgs{OutputID: "abc", Pattern: "a"})
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if res.TotalMatches != tt.wantTotal {
				t.Fatalf("total %d want %d", res.TotalMatches, tt.wantTotal)
			}
			if res.Truncated != tt.wantTrunc {
				t.Fatalf("truncated %v want %v", res.Truncated, tt.wantTrunc)
			}
		})
	}
}

func TestParseGrepOutputIgnoresLookalikeContent(t *testing.T) {
	lines, exit, hasMarker := parseGrepOutput("__MPH_GREP_EXIT_abc:0\n7:hit\n__MPH_GREP_EXIT_abc:1", "__MPH_GREP_EXIT_abc:")
	if !hasMarker {
		t.Fatal("must find the trailing marker")
	}
	if exit != 1 {
		t.Fatalf("exit %d want 1 (the real trailing marker, not the 0 written by the model)", exit)
	}
	if n := parseGrepLines(lines); len(n) != 1 || n[0] != 7 {
		t.Fatalf("matches %v want [7]", n)
	}
}

func TestParseExitMarker(t *testing.T) {
	tests := []struct {
		name   string
		out    string
		marker string
		wantN  int
		wantOK bool
	}{
		{"exit code parsed", "foo\n__MPH_EXIT:3\n", "__MPH_EXIT:", 3, true},
		{"missing marker", "no marker", "__MPH_EXIT:", 0, false},
		{"generic marker does not match per-capture marker", "output\n__MPH_EXIT:0\n", "__MPH_EXIT_deadbeef:", 0, false},
		{"per-capture marker matches", "output\n__MPH_EXIT_deadbeef:3\n", "__MPH_EXIT_deadbeef:", 3, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, ok := parseExitMarker(tt.out, tt.marker)
			if ok != tt.wantOK || n != tt.wantN {
				t.Fatalf("got %d %v want %d %v", n, ok, tt.wantN, tt.wantOK)
			}
		})
	}
}

func TestSearchGrepsTheRawCaptureFile(t *testing.T) {
	var gotCmd string
	f := &fakeExec{fn: func(name, cmd string) (string, error) {
		gotCmd = cmd
		return "7:hit\n__MPH_GREP_EXIT_abc:0", nil
	}}
	s := NewStore(f, "vm", "/tmp/mph-output/run1", testCfg(), 4096, nil)
	s.handles["abc"] = Handle{ID: "abc", Path: "/tmp/mph-output/run1/raw-abc/output.txt", TotalLines: 7}

	res, err := s.Search(context.Background(), SearchArgs{OutputID: "abc", Pattern: "hit"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !slices.Equal(res.Matches, []int{7}) {
		t.Fatalf("want [7], got %v", res.Matches)
	}
	if strings.Contains(gotCmd, "chunk_") {
		t.Fatalf("grep must not target chunk files: %q", gotCmd)
	}
	if !strings.Contains(gotCmd, "'/tmp/mph-output/run1/raw-abc/output.txt'") {
		t.Fatalf("grep must target the quoted raw path: %q", gotCmd)
	}
}

func TestSearchEmptyCapture(t *testing.T) {
	tests := []struct {
		name      string
		handle    Handle
		grepOut   string
		wantTotal int
		wantLines []int
	}{
		{
			name:      "empty file, grep finds nothing",
			handle:    Handle{ID: "abc", Path: "/tmp/x/output.txt"},
			grepOut:   "__MPH_GREP_EXIT_abc:1",
			wantTotal: 0,
		},
		{
			name:      "zero total_lines no longer means no output",
			handle:    Handle{ID: "abc", Path: "/tmp/x/output.txt"},
			grepOut:   "1:hit\n__MPH_GREP_EXIT_abc:0",
			wantTotal: 1,
			wantLines: []int{1},
		},
		{
			name:      "whitespace-only grep output counts as no match",
			handle:    Handle{ID: "abc", Path: "/tmp/x/output.txt"},
			grepOut:   "\n",
			wantTotal: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeExec{fn: func(name, cmd string) (string, error) {
				if strings.Contains(cmd, "grep") {
					return tt.grepOut, nil
				}
				return "", nil
			}}
			s := NewStore(f, "vm", "/tmp/mph-output/run1", testCfg(), 4096, nil)
			s.handles[tt.handle.ID] = tt.handle

			res, err := s.Search(context.Background(), SearchArgs{OutputID: "abc", Pattern: "hit"})
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if res.TotalMatches != tt.wantTotal || res.Truncated {
				t.Fatalf("got %+v want total %d truncated false", res, tt.wantTotal)
			}
			if !slices.Equal(res.Matches, tt.wantLines) {
				t.Fatalf("want lines %v, got %v", tt.wantLines, res.Matches)
			}
		})
	}
}

func TestSearchTruncatesAtTheHeadBound(t *testing.T) {
	var b strings.Builder
	b.WriteString("\n")
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(&b, "%d:hit\n", i)
	}
	cfg := testCfg()
	cfg.SearchMaxMatches = 3
	f := &fakeExec{fn: func(name, cmd string) (string, error) {
		if strings.Contains(cmd, "grep -a -c") {
			return "5\n", nil
		}
		return b.String(), nil
	}}
	s := NewStore(f, "vm", "/tmp/mph-output/run1", cfg, 4096, nil)
	s.handles["abc"] = Handle{ID: "abc", Path: "/tmp/x/output.txt", TotalLines: 5}

	res, err := s.Search(context.Background(), SearchArgs{OutputID: "abc", Pattern: "hit"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !slices.Equal(res.Matches, []int{1, 2, 3}) {
		t.Fatalf("want [1 2 3], got %v", res.Matches)
	}
	if !res.Truncated || res.TotalMatches != 5 {
		t.Fatalf("got %+v want truncated total 5", res)
	}
}

func TestBuildGrepCmd(t *testing.T) {
	tests := []struct {
		name       string
		pattern    string
		fixed      bool
		ignoreCase bool
		wantSubs   []string
		wantAbsent []string
	}{
		{
			name: "fixed_string quotes literally with -F -i", pattern: "a'b", fixed: true, ignoreCase: true,
			wantSubs:   []string{"grep -a -n", "-F", "-i", "-e 'a'\\''b'", "'/tmp/x/output.txt'", "head -n 201", "__MPH_GREP_EXIT_abc:$?"},
			wantAbsent: []string{"-E"},
		},
		{
			name: "regex search uses extended regexp", pattern: "[[:digit:]]+", fixed: false, ignoreCase: false,
			wantSubs: []string{"-E"},
		},
		{
			name: "leading dash pattern stays behind -e", pattern: "-v", fixed: false, ignoreCase: false,
			wantSubs:   []string{"-e '-v'"},
			wantAbsent: []string{"' -v'", "grep -a -n -v"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildGrepCmd("/tmp/x/output.txt", tt.pattern, tt.fixed, tt.ignoreCase, 200, "__MPH_GREP_EXIT_abc:")
			for _, want := range tt.wantSubs {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in %q", want, got)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("must not contain %q: %q", absent, got)
				}
			}
		})
	}
}

func TestParseGrepOutput(t *testing.T) {
	tests := []struct {
		name      string
		out       string
		wantLines []string
		wantExit  int
		wantOK    bool
	}{
		{"match plus marker", "1:x\n__MPH_GREP_EXIT_abc:0\n", []string{"1:x"}, 0, true},
		{"grep exit 2", "err\n__MPH_GREP_EXIT_abc:2\n", nil, 2, true},
		{"missing marker", "f:1:x\n", nil, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines, exit, ok := parseGrepOutput(tt.out, "__MPH_GREP_EXIT_abc:")
			if ok != tt.wantOK || exit != tt.wantExit {
				t.Fatalf("got exit %d ok %v want %d %v (lines %v)", exit, ok, tt.wantExit, tt.wantOK, lines)
			}
			if tt.wantOK && tt.wantExit != 2 {
				if len(lines) != len(tt.wantLines) {
					t.Fatalf("got %v want %v", lines, tt.wantLines)
				}
				for i := range lines {
					if lines[i] != tt.wantLines[i] {
						t.Fatalf("got %v want %v", lines, tt.wantLines)
					}
				}
			}
		})
	}
}

func TestParseGrepLines(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  []int
	}{
		{
			name:  "line number is the absolute line",
			lines: []string{"1513:hit"},
			want:  []int{1513},
		},
		{
			name:  "colons inside the matched line are kept in the text",
			lines: []string{"12:2000001:needle-7x9"},
			want:  []int{12},
		},
		{
			name:  "blank and non-numeric lines are skipped",
			lines: []string{"", "   ", "badline", "7:ok", ":leading colon in text", "x:1"},
			want:  []int{7},
		},
		{
			name:  "empty match text still counts",
			lines: []string{"3:"},
			want:  []int{3},
		},
		{
			name:  "out of order input is sorted",
			lines: []string{"900:a", "4:b", "77:c", "1:d"},
			want:  []int{1, 4, 77, 900},
		},
		{
			name:  "no usable lines",
			lines: []string{"", "badline"},
			want:  []int{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseGrepLines(tt.lines); !slices.Equal(got, tt.want) {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}

func TestCapMatches(t *testing.T) {
	tests := []struct {
		name        string
		matches     []int
		maxM        int
		wantKept    []int
		wantTruncat bool
	}{
		{"under the cap keeps everything", []int{1, 2, 500}, 10, []int{1, 2, 500}, false},
		{"at the cap keeps everything", []int{1, 2}, 2, []int{1, 2}, false},
		{"over the cap keeps the first maxM", []int{1, 2, 500, 900}, 2, []int{1, 2}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kept, truncated := capMatches(tt.matches, tt.maxM)
			if truncated != tt.wantTruncat {
				t.Fatalf("truncated %v want %v", truncated, tt.wantTruncat)
			}
			if !slices.Equal(kept, tt.wantKept) {
				t.Fatalf("got %v want %v", kept, tt.wantKept)
			}
		})
	}
}

func echoMarker(cmd, code string) string {
	start := strings.Index(cmd, "__MPH_EXIT_")
	if start < 0 {
		return "__MPH_EXIT:" + code
	}
	rest := cmd[start:]
	end := strings.Index(rest, ":")
	if end < 0 {
		return rest + code
	}
	return rest[:end+1] + code
}

func TestBuildReadCmd(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		offset int
		limit  int
		want   string
	}{
		{"mid-file window", "/tmp/x/output.txt", 812, 20, "sed -n '812,831p' '/tmp/x/output.txt' | cut -c1-2000"},
		{"from the start", "/tmp/x/output.txt", 1, 50, "sed -n '1,50p' '/tmp/x/output.txt' | cut -c1-2000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildReadCmd(tt.path, tt.offset, tt.limit); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestClampReadLimit(t *testing.T) {
	tests := []struct {
		name  string
		limit int
		want  int
	}{
		{"zero defaults", 0, 50},
		{"negative defaults", -5, 50},
		{"caps at 200", 10000, 200},
		{"in range passes through", 20, 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clampReadLimit(tt.limit); got != tt.want {
				t.Fatalf("got %d want %d", got, tt.want)
			}
		})
	}
}

func TestStoreRead(t *testing.T) {
	tests := []struct {
		name          string
		seed          map[string]Handle
		outputID      string
		offset        int
		limit         int
		viaSearch     bool
		execOut       string
		forbidExec    bool
		wantLines     []string
		wantTotal     int
		wantTruncated bool
		wantErrSubs   []string
	}{
		{
			name:          "reads a slice with truncation flag",
			seed:          map[string]Handle{"abc": {ID: "abc", Path: "/tmp/x/output.txt", TotalLines: 1000}},
			outputID:      "abc",
			offset:        812,
			limit:         20,
			execOut:       "line812\nline813\n",
			wantLines:     []string{"line812", "line813"},
			wantTotal:     1000,
			wantTruncated: true,
		},
		{
			name:       "past EOF returns empty without exec",
			seed:       map[string]Handle{"abc": {ID: "abc", Path: "/tmp/x/output.txt", TotalLines: 1000}},
			outputID:   "abc",
			offset:     5000,
			limit:      20,
			forbidExec: true,
			wantLines:  []string{},
			wantTotal:  1000,
		},
		{
			name:        "offset below 1 is an error",
			seed:        map[string]Handle{"abc": {ID: "abc", TotalLines: 10}},
			outputID:    "abc",
			offset:      0,
			limit:       10,
			wantErrSubs: []string{"offset"},
		},
		{
			name:        "unknown id with no handles says so",
			outputID:    "nope",
			offset:      1,
			limit:       10,
			wantErrSubs: []string{"unknown output_id", "no active handles"},
		},
		{
			name:        "unknown id lists valid handles",
			seed:        map[string]Handle{"oid-1": {ID: "oid-1", TotalLines: 4}, "oid-2": {ID: "oid-2", TotalLines: 4}},
			outputID:    "output",
			offset:      1,
			limit:       10,
			wantErrSubs: []string{"oid-1", "oid-2", "never invent"},
		},
		{
			name:        "search with no handles says so",
			outputID:    "0",
			viaSearch:   true,
			wantErrSubs: []string{"unknown output_id", "no active handles"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			f := &fakeExec{fn: func(name, cmd string) (string, error) {
				called = true
				if tt.forbidExec {
					return "", fmt.Errorf("must not exec: %q", cmd)
				}
				return tt.execOut, nil
			}}
			s := NewStore(f, "vm", "/tmp/mph-output/run1", testCfg(), 4096, nil)
			for id, h := range tt.seed {
				s.handles[id] = h
			}
			if tt.viaSearch {
				_, err := s.Search(context.Background(), SearchArgs{OutputID: tt.outputID, Pattern: "x"})
				if err == nil {
					t.Fatalf("want error, got nil")
				}
				for _, sub := range tt.wantErrSubs {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("error %q must contain %q", err, sub)
					}
				}
				return
			}
			res, err := s.Read(context.Background(), tt.outputID, tt.offset, tt.limit)
			if len(tt.wantErrSubs) > 0 {
				if err == nil {
					t.Fatalf("want error, got %+v", res)
				}
				for _, sub := range tt.wantErrSubs {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("error %q must contain %q", err, sub)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if tt.forbidExec && called {
				t.Fatalf("must not exec when offset is past EOF")
			}
			if len(res.Lines) != len(tt.wantLines) || res.TotalLines != tt.wantTotal || res.Truncated != tt.wantTruncated {
				t.Fatalf("got %+v want lines %v total %d truncated %v", res, tt.wantLines, tt.wantTotal, tt.wantTruncated)
			}
			for i := range tt.wantLines {
				if res.Lines[i] != tt.wantLines[i] {
					t.Fatalf("got %+v want lines %v", res, tt.wantLines)
				}
			}
		})
	}
}

func TestWrapCaptureWithNoCapKeepsFullOutput(t *testing.T) {
	got := wrapCapture("echo hi", "/tmp/raw.txt", "__MPH_EXIT_abc:", 0)
	if strings.Contains(got, "head -c") {
		t.Fatalf("a disabled cap must not truncate the capture: %q", got)
	}
	if !strings.Contains(got, "echo hi") || !strings.Contains(got, "__MPH_EXIT_abc:") {
		t.Fatalf("capture body or marker lost: %q", got)
	}
}
