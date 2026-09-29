package output

import (
	"context"
	"fmt"
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
	return config.OutputConfig{Enabled: true, Mode: "auto", ChunkLines: 500, MaxCommandSize: 64 * 1024 * 1024, MaxTotalSize: 512 * 1024 * 1024, SearchMaxMatches: 200}
}

func TestDecideInline(t *testing.T) {
	tests := []struct {
		name   string
		mode   string
		force  bool
		size   int64
		cutoff int64
		want   bool
	}{
		{"auto no flag always inline", "auto", false, 1 << 30, 100, true},
		{"auto flag small inline", "auto", true, 50, 100, true},
		{"auto flag big chunk", "auto", true, 200, 100, false},
		{"auto flag at cutoff chunks", "auto", true, 100, 100, false},
		{"always ignores flag", "always", false, 10, 100, false},
		{"always big chunk", "always", true, 1 << 20, 100, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DecideInline(tt.mode, tt.force, tt.size, tt.cutoff); got != tt.want {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}

func TestAbsoluteLine(t *testing.T) {
	if got := AbsoluteLine(3, 500, 13); got != 3*500+13 {
		t.Fatalf("got %d", got)
	}
	if got := AbsoluteLine(0, 500, 1); got != 1 {
		t.Fatalf("got %d", got)
	}
}

func TestValidatePattern(t *testing.T) {
	if err := ValidatePattern("foo.*bar"); err != nil {
		t.Fatalf("valid ERE rejected: %v", err)
	}
	if err := ValidatePattern(`\d+`); err == nil {
		t.Fatalf("PCRE \\d accepted, want clean error")
	}
	if err := ValidatePattern("(["); err == nil {
		t.Fatalf("invalid regex accepted")
	}
	if err := ValidatePattern(""); err == nil {
		t.Fatalf("empty pattern accepted")
	}
}

func TestSearchFixedStringSkipsValidation(t *testing.T) {
	f := &fakeExec{fn: func(name, cmd string) (string, error) {
		if strings.Contains(cmd, "grep") {
			return "/tmp/x/chunk_00000:1:(['ll\n__MPH_GREP_EXIT_abc:0", nil
		}
		return "", nil
	}}
	s := NewStore(f, "vm", "/tmp/mph-output/run1", testCfg(), 4096, nil)
	s.handles["abc"] = Handle{ID: "abc", ChunkDir: "/tmp/x/", ChunkLines: 500, Chunks: 1}
	res, err := s.Search(context.Background(), SearchArgs{OutputID: "abc", Pattern: "(["})
	if err == nil {
		t.Fatalf("regex mode must reject invalid pattern")
	}
	res, err = s.Search(context.Background(), SearchArgs{OutputID: "abc", Pattern: "([", FixedString: true})
	if err != nil {
		t.Fatalf("fixed_string must skip ERE validation: %v", err)
	}
	if res.TotalMatches != 1 {
		t.Fatalf("want 1 match, got %+v", res)
	}
}

func TestCaptureAutoNoFlagDirect(t *testing.T) {
	f := &fakeExec{fn: func(name, cmd string) (string, error) {
		if strings.Contains(cmd, "df -P") || strings.Contains(cmd, "mkdir") {
			t.Fatalf("auto no-flag must not touch files: %q", cmd)
		}
		return "hello", nil
	}}
	s := NewStore(f, "vm", "/tmp/mph-output/run1", testCfg(), 4096, nil)
	res, err := s.Capture(context.Background(), "echo hi", false)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if res.Handle != nil || res.Inline != "hello" {
		t.Fatalf("got %+v", res)
	}
	if len(s.Handles()) != 0 {
		t.Fatalf("no handle should be registered")
	}
}

func TestCaptureChunksLargeOutput(t *testing.T) {
	f := &fakeExec{fn: func(name, cmd string) (string, error) {
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
		case strings.Contains(cmd, "split -l"):
			return "", nil
		default:
			return "", fmt.Errorf("unexpected cmd %q", cmd)
		}
	}}
	s := NewStore(f, "vm", "/tmp/mph-output/run1", testCfg(), 4096, nil)
	res, err := s.Capture(context.Background(), "echo hi", true)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if res.Handle == nil {
		t.Fatalf("want handle, got inline %q", res.Inline)
	}
	if res.Handle.Chunks != 128 || res.Handle.TotalLines != 64000 {
		t.Fatalf("handle %+v", res.Handle)
	}
	if len(s.Handles()) != 1 {
		t.Fatalf("registry len %d", len(s.Handles()))
	}
}

func TestCapturePreflightRefusal(t *testing.T) {
	cfg := testCfg()
	f := &fakeExec{fn: func(name, cmd string) (string, error) {
		if strings.Contains(cmd, "df -P") {
			return "1", nil
		}
		return "", nil
	}}
	s := NewStore(f, "vm", "/tmp/mph-output/run1", cfg, 4096, nil)
	if _, err := s.Capture(context.Background(), "echo hi", true); err == nil {
		t.Fatalf("want preflight refusal")
	}
}

func TestCaptureCapEnforced(t *testing.T) {
	cfg := testCfg()
	cfg.MaxCommandSize = 10
	f := &fakeExec{fn: func(name, cmd string) (string, error) {
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
	}}
	s := NewStore(f, "vm", "/tmp/mph-output/run1", cfg, 4096, nil)
	if _, err := s.Capture(context.Background(), "echo hi", true); err == nil {
		t.Fatalf("want cap error")
	}
}

func TestSearchExitMapping(t *testing.T) {
	newStoreWithHandle := func(grepOut string) *Store {
		f := &fakeExec{fn: func(name, cmd string) (string, error) {
			if strings.Contains(cmd, "grep") {
				return grepOut, nil
			}
			return "", nil
		}}
		s := NewStore(f, "vm", "/tmp/mph-output/run1", testCfg(), 4096, nil)
		s.handles["abc"] = Handle{ID: "abc", ChunkDir: "/tmp/x/", ChunkLines: 500, Chunks: 2}
		return s
	}
	res, err := newStoreWithHandle("/tmp/x/chunk_00003:13:hit\n__MPH_GREP_EXIT_abc:0").Search(context.Background(), SearchArgs{OutputID: "abc", Pattern: "hit"})
	if err != nil || res.TotalMatches != 1 {
		t.Fatalf("matches: %+v %v", res, err)
	}
	if res.Matches[0].Lines[0] != 3*500+13 {
		t.Fatalf("absolute line %+v", res.Matches)
	}
	res, err = newStoreWithHandle("__MPH_GREP_EXIT_abc:1").Search(context.Background(), SearchArgs{OutputID: "abc", Pattern: "hit"})
	if err != nil || res.TotalMatches != 0 {
		t.Fatalf("no-match: %+v %v", res, err)
	}
	if _, err := newStoreWithHandle("__MPH_GREP_EXIT_abc:2").Search(context.Background(), SearchArgs{OutputID: "abc", Pattern: "hit"}); err == nil {
		t.Fatalf("want grep exit 2 error")
	}
	if _, err := newStoreWithHandle("__MPH_GREP_EXIT_abc:0").Search(context.Background(), SearchArgs{OutputID: "nope", Pattern: "hit"}); err == nil {
		t.Fatalf("want unknown output_id error")
	}
}

func TestCleanCommand(t *testing.T) {
	if got, err := cleanCommand("  echo hi "); err != nil || got != "echo hi" {
		t.Fatalf("got %q %v", got, err)
	}
	if _, err := cleanCommand("   "); err == nil {
		t.Fatalf("want error for blank command")
	}
}

func TestCapturePaths(t *testing.T) {
	subdir, raw := capturePaths("/tmp/mph-output/run1", "oid")
	if subdir != "/tmp/mph-output/run1/raw-oid" || raw != "/tmp/mph-output/run1/raw-oid/output.txt" {
		t.Fatalf("got %q %q", subdir, raw)
	}
}

func TestParseExitMarker(t *testing.T) {
	if n, ok := parseExitMarker("foo\n__MPH_EXIT:3\n", "__MPH_EXIT:"); !ok || n != 3 {
		t.Fatalf("got %d %v", n, ok)
	}
	if _, ok := parseExitMarker("no marker", "__MPH_EXIT:"); ok {
		t.Fatalf("want not-ok without marker")
	}
}

func TestSearchSingleChunkNoFilename(t *testing.T) {
	f := &fakeExec{fn: func(name, cmd string) (string, error) {
		if strings.Contains(cmd, "grep") {
			if strings.Contains(cmd, "-H") {
				return "/tmp/x/chunk_00000:1:2000001:needle-7x9 TREASURE-LINE\n__MPH_GREP_EXIT_abc:0", nil
			}
			return "1:2000001:needle-7x9 TREASURE-LINE\n__MPH_GREP_EXIT_abc:0", nil
		}
		return "", nil
	}}
	s := NewStore(f, "vm", "/tmp/mph-output/run1", testCfg(), 4096, nil)
	s.handles["abc"] = Handle{ID: "abc", ChunkDir: "/tmp/x/", ChunkLines: 500, Chunks: 1}
	res, err := s.Search(context.Background(), SearchArgs{OutputID: "abc", Pattern: "2000001"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if res.TotalMatches != 1 || len(res.Matches) != 1 {
		t.Fatalf("want 1 match in 1 chunk, got %+v", res)
	}
	if res.Matches[0].Chunk != 0 || len(res.Matches[0].Lines) != 1 || res.Matches[0].Lines[0] != 1 {
		t.Fatalf("want chunk 0 lines [1], got %+v", res.Matches)
	}
}

func TestBuildGrepCmd(t *testing.T) {
	got := buildGrepCmd("/tmp/x/", "a'b", true, true, 200, "__MPH_GREP_EXIT_abc:")
	for _, want := range []string{"grep -a -n -H -m 200", "-F", "-i", "'a'\\''b'", "'/tmp/x/'chunk_*", "head -n 201", "__MPH_GREP_EXIT_abc:$?"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "-E") {
		t.Fatalf("fixed_string must not combine -E with -F: %q", got)
	}
	got = buildGrepCmd("/tmp/x/", "[[:digit:]]+", false, false, 200, "__MPH_GREP_EXIT_abc:")
	if !strings.Contains(got, "-E") {
		t.Fatalf("regex search must use extended regexp (-E): %q", got)
	}
}

func TestParseGrepOutput(t *testing.T) {
	lines, exit, ok := parseGrepOutput("f:1:x\n__MPH_GREP_EXIT_abc:0\n", "__MPH_GREP_EXIT_abc:")
	if !ok || exit != 0 || len(lines) != 2 || lines[0] != "f:1:x" || lines[1] != "" {
		t.Fatalf("got %v %d", lines, exit)
	}
	if _, exit, ok := parseGrepOutput("err\n__MPH_GREP_EXIT_abc:2\n", "__MPH_GREP_EXIT_abc:"); !ok || exit != 2 {
		t.Fatalf("want exit 2, got %d", exit)
	}
}

func TestMapGrepLines(t *testing.T) {
	byChunk := mapGrepLines([]string{"/tmp/x/chunk_00003:13:hit", "badline", "/tmp/x/chunk_00001:1:a"}, 500)
	if len(byChunk) != 2 {
		t.Fatalf("got %v", byChunk)
	}
	if byChunk[3][0] != 3*500+13 || byChunk[1][0] != 501 {
		t.Fatalf("got %v", byChunk)
	}
}

func TestCapMatches(t *testing.T) {
	byChunk := map[int][]int{0: {0, 1}, 1: {500}}
	matches, total, truncated := capMatches(byChunk, 10)
	if truncated || total != 3 || len(matches) != 2 {
		t.Fatalf("got %v %d %v", matches, total, truncated)
	}
	matches, total, truncated = capMatches(byChunk, 2)
	if !truncated || total != 3 || len(matches) != 1 || len(matches[0].Lines) != 2 {
		t.Fatalf("got %v %d %v", matches, total, truncated)
	}
}

func TestResolveMaxMatches(t *testing.T) {
	s := NewStore(nil, "vm", "/tmp/x", testCfg(), 0, nil)
	if got := s.resolveMaxMatches(); got != 200 {
		t.Fatalf("got %d", got)
	}
	s.cfg.SearchMaxMatches = 0
	if got := s.resolveMaxMatches(); got != 200 {
		t.Fatalf("zero config must fall back to 200, got %d", got)
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

func TestRawSizeErrorOnGarbage(t *testing.T) {
	f := &fakeExec{fn: func(name, cmd string) (string, error) {
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
	}}
	s := NewStore(f, "vm", "/tmp/mph-output/run1", testCfg(), 4096, nil)
	if _, err := s.Capture(context.Background(), "echo hi", true); err == nil {
		t.Fatalf("want stat error for non-numeric size")
	}
}

func TestCaptureTimeoutChunksPartial(t *testing.T) {
	f := &fakeExec{fn: func(name, cmd string) (string, error) {
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
		case strings.Contains(cmd, "split -l"):
			return "", nil
		default:
			return "", nil
		}
	}}
	s := NewStore(f, "vm", "/tmp/mph-output/run1", testCfg(), 4096, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := s.Capture(ctx, "sleep 99", true)
	if err != nil {
		t.Fatalf("timeout must chunk partial output, got err: %v", err)
	}
	if res.Handle == nil || res.Handle.ExitCode != 124 {
		t.Fatalf("want handle with exit 124, got %+v", res)
	}
}

func TestSearchExecErrorWithoutMarker(t *testing.T) {
	f := &fakeExec{fn: func(name, cmd string) (string, error) {
		if strings.Contains(cmd, "grep") {
			return "", fmt.Errorf("multipass exploded")
		}
		return "", nil
	}}
	s := NewStore(f, "vm", "/tmp/mph-output/run1", testCfg(), 4096, nil)
	s.handles["abc"] = Handle{ID: "abc", ChunkDir: "/tmp/x/", ChunkLines: 500, Chunks: 2}
	if _, err := s.Search(context.Background(), SearchArgs{OutputID: "abc", Pattern: "hit"}); err == nil {
		t.Fatalf("want exec error surfaced, not silent zero matches")
	}
}

func TestExitMarkerUnforgeable(t *testing.T) {
	marker := "__MPH_EXIT_deadbeef:"
	if n, ok := parseExitMarker("output\n__MPH_EXIT:0\n", marker); ok || n != 0 {
		t.Fatalf("generic marker must not match per-capture marker: %d %v", n, ok)
	}
	if n, ok := parseExitMarker("output\n__MPH_EXIT_deadbeef:3\n", marker); !ok || n != 3 {
		t.Fatalf("got %d %v", n, ok)
	}
}

func TestBuildReadCmd(t *testing.T) {
	got := buildReadCmd("/tmp/x/output.txt", 812, 20)
	want := "sed -n '812,831p' '/tmp/x/output.txt' | cut -c1-2000"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestClampReadLimit(t *testing.T) {
	if got := clampReadLimit(0); got != 50 {
		t.Fatalf("non-positive limit must default, got %d", got)
	}
	if got := clampReadLimit(-5); got != 50 {
		t.Fatalf("negative limit must default, got %d", got)
	}
	if got := clampReadLimit(10000); got != 200 {
		t.Fatalf("limit must cap at 200, got %d", got)
	}
	if got := clampReadLimit(20); got != 20 {
		t.Fatalf("got %d", got)
	}
}

func TestStoreRead(t *testing.T) {
	f := &fakeExec{fn: func(name, cmd string) (string, error) {
		if strings.Contains(cmd, "sed -n") {
			return "line812\nline813\n", nil
		}
		return "", fmt.Errorf("unexpected cmd %q", cmd)
	}}
	s := NewStore(f, "vm", "/tmp/mph-output/run1", testCfg(), 4096, nil)
	s.handles["abc"] = Handle{ID: "abc", Path: "/tmp/x/output.txt", ChunkDir: "/tmp/x/", ChunkLines: 500, Chunks: 2, TotalLines: 1000}
	res, err := s.Read(context.Background(), "abc", 812, 20)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(res.Lines) != 2 || res.Lines[0] != "line812" || res.TotalLines != 1000 || res.Offset != 812 {
		t.Fatalf("got %+v", res)
	}
	if !res.Truncated {
		t.Fatalf("more lines remain, want truncated")
	}
}

func TestStoreReadPastEOF(t *testing.T) {
	called := false
	f := &fakeExec{fn: func(name, cmd string) (string, error) {
		called = true
		return "", nil
	}}
	s := NewStore(f, "vm", "/tmp/mph-output/run1", testCfg(), 4096, nil)
	s.handles["abc"] = Handle{ID: "abc", Path: "/tmp/x/output.txt", ChunkDir: "/tmp/x/", ChunkLines: 500, Chunks: 2, TotalLines: 1000}
	res, err := s.Read(context.Background(), "abc", 5000, 20)
	if err != nil {
		t.Fatalf("read past EOF must not error: %v", err)
	}
	if len(res.Lines) != 0 || res.Truncated {
		t.Fatalf("got %+v", res)
	}
	if called {
		t.Fatalf("must not exec when offset is past EOF")
	}
}

func TestStoreReadErrors(t *testing.T) {
	s := NewStore(&fakeExec{fn: func(name, cmd string) (string, error) { return "", nil }},
		"vm", "/tmp/mph-output/run1", testCfg(), 4096, nil)
	if _, err := s.Read(context.Background(), "nope", 1, 10); err == nil {
		t.Fatalf("want unknown output_id error")
	}
	s.handles["abc"] = Handle{ID: "abc", TotalLines: 10}
	if _, err := s.Read(context.Background(), "abc", 0, 10); err == nil {
		t.Fatalf("want offset >= 1 error")
	}
}

func TestLookupErrorGuidesModel(t *testing.T) {
	empty := NewStore(&fakeExec{fn: func(name, cmd string) (string, error) { return "", nil }},
		"vm", "/tmp/mph-output/run1", testCfg(), 4096, nil)
	_, err := empty.Search(context.Background(), SearchArgs{OutputID: "0", Pattern: "x"})
	if err == nil || !strings.Contains(err.Error(), "no active handles") {
		t.Fatalf("empty registry must say there are no handles, got %v", err)
	}
	empty.handles["oid-1"] = Handle{ID: "oid-1", TotalLines: 4}
	empty.handles["oid-2"] = Handle{ID: "oid-2", TotalLines: 4}
	_, err = empty.Read(context.Background(), "output", 1, 10)
	if err == nil || !strings.Contains(err.Error(), "oid-1") || !strings.Contains(err.Error(), "never invent") {
		t.Fatalf("error must list valid ids, got %v", err)
	}
}
