package output

import (
	"context"
	"fmt"
	"path"
	"regexp/syntax"
	"sort"
	"strconv"
	"strings"
	"uuid"

	"github.com/trollLemon/MPHarness/internal/config"
	"github.com/trollLemon/MPHarness/internal/validation"
)

type Handle struct {
	ID         string
	Path       string
	ChunkDir   string
	ChunkLines int
	Chunks     int
	TotalLines int
	TotalBytes int64
	ExitCode   int
	Command    string
}

type Result struct {
	Inline   string
	Handle   *Handle
	ExitCode int
}

type SearchArgs struct {
	OutputID    string
	Pattern     string
	FixedString bool
	IgnoreCase  bool
}

type ChunkMatches struct {
	Chunk int
	Lines []int
}

type SearchResult struct {
	OutputID      string
	TotalMatches  int
	ChunksMatched int
	Matches       []ChunkMatches
	Truncated     bool
}

type ReadResult struct {
	OutputID   string
	Offset     int
	Limit      int
	TotalLines int
	Lines      []string
	Truncated  bool
}

const (
	defaultReadLimit = 50
	maxReadLines     = 200
	maxLineWidth     = 2000
)

type ExecClient interface {
	Exec(ctx context.Context, name, command string, maxAttrBytes int) (string, error)
}

// Store keeps every command output capture for one run.
type Store struct {
	client       ExecClient
	vm           string
	runDir       string
	cfg          config.OutputConfig
	inlineCutoff int64
	allowed      map[string]bool
	handles      map[string]Handle
	totalBytes   int64
}

// NewStore returns a new Store instance.
func NewStore(client ExecClient, vm, runDir string, cfg config.OutputConfig, inlineCutoff int64, allowed map[string]bool) *Store {
	return &Store{
		client: client, vm: vm, runDir: runDir, cfg: cfg,
		inlineCutoff: inlineCutoff, allowed: allowed,
		handles: map[string]Handle{},
	}
}

// Handles returns a list of Handle objects.
func (s *Store) Handles() []Handle {
	out := make([]Handle, 0, len(s.handles))
	for _, h := range s.handles {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// DecideInline returns if we should inline the output (return the command output directly to the agent), or
// not (return a handle to use later).
func DecideInline(mode string, force bool, size, cutoff int64) bool {
	if mode == "always" {
		return false
	}
	if !force {
		return true
	}
	if cutoff <= 0 {
		cutoff = 1 << 30
	}
	return size < cutoff
}

// AbsoluteLine returns the absolute line number of a file given the chunkIndex, chunkLines, and local line number.
func AbsoluteLine(chunkIndex, chunkLines, localLine int) int {
	return chunkIndex*chunkLines + localLine
}

// EffectiveCutoff returns the cutoff size for output, given the derived budget, and the given configuration.
// This function prefers the cfg.InlineMaxSize value, if that is 0 the function returns derivedBudget.
func EffectiveCutoff(cfg config.OutputConfig, derivedBudget int) int64 {
	if int64(cfg.InlineMaxSize) > 0 {
		return int64(cfg.InlineMaxSize)
	}
	return int64(derivedBudget)
}

// ValidatePattern validates a regex search pattern.
func ValidatePattern(pattern string) error {
	if strings.TrimSpace(pattern) == "" {
		return fmt.Errorf("pattern is required")
	}
	for _, bad := range []string{`\d`, `\w`, `\s`, `\b`, `(?`, `\A`, `\z`} {
		if strings.Contains(pattern, bad) {
			return fmt.Errorf("invalid pattern %q: %q is PCRE, not POSIX ERE; use [[:digit:]] etc. or fixed_string for literal text", pattern, bad)
		}
	}
	if _, err := syntax.Parse(pattern, syntax.POSIX); err != nil {
		return fmt.Errorf("invalid pattern %q: %v", pattern, err)
	}
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func cleanCommand(command string) (string, error) {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return "", fmt.Errorf("command is required")
	}
	return cmd, nil
}

func (s *Store) mode() string {
	if s.cfg.Mode == "" {
		return "auto"
	}
	return s.cfg.Mode
}

func (s *Store) directExec(ctx context.Context, cmd string) (Result, error) {
	out, err := s.client.Exec(ctx, s.vm, cmd, 0)
	if err != nil {
		return Result{}, err
	}
	if out == "" {
		return Result{Inline: "(no output)"}, nil
	}
	return Result{Inline: out}, nil
}

func capturePaths(runDir, oid string) (subdir, raw string) {
	subdir = path.Join(runDir, "raw-"+oid)
	return subdir, path.Join(subdir, "output.txt")
}

func (s *Store) preflight(ctx context.Context) error {
	maxCmd := int64(s.cfg.MaxCommandSize)
	if maxCmd <= 0 {
		return nil
	}
	freeOut, err := s.client.Exec(ctx, s.vm, fmt.Sprintf("df -P %s | awk 'NR==2 {print $4}'", shellQuote(s.runDir)), 0)
	if err != nil {
		return nil
	}
	kb, perr := strconv.ParseInt(strings.TrimSpace(freeOut), 10, 64)
	if perr != nil {
		return nil
	}
	if kb*1024 < maxCmd {
		return fmt.Errorf("not enough free space for capture: need %d bytes", maxCmd)
	}
	return nil
}

func (s *Store) ensureSubdir(ctx context.Context, subdir string) error {
	_, err := s.client.Exec(ctx, s.vm, fmt.Sprintf("mkdir -p %s", shellQuote(subdir)), 0)
	return err
}

func (s *Store) removeSubdir(ctx context.Context, subdir string) {
	_, _ = s.client.Exec(ctx, s.vm, fmt.Sprintf("rm -rf %s", shellQuote(subdir)), 0)
}

func parseExitMarker(out, marker string) (int, bool) {
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), marker); ok {
			if n, perr := strconv.Atoi(strings.TrimSpace(v)); perr == nil {
				return n, true
			}
		}
	}
	return 0, false
}

func (s *Store) runRedirect(ctx context.Context, cmd, raw, marker string) (int, error) {
	wrapped := fmt.Sprintf("%s > %s 2>&1; echo %s$?", cmd, shellQuote(raw), marker)
	execOut, execErr := s.client.Exec(ctx, s.vm, wrapped, 0)
	if strings.Contains(execOut, "No space left on device") {
		return 0, fmt.Errorf("capture failed: no space left on device")
	}
	if exit, ok := parseExitMarker(execOut, marker); ok {
		return exit, nil
	}
	return 0, execErr
}

func (s *Store) rawSize(ctx context.Context, raw string) (int64, error) {
	sizeOut, err := s.client.Exec(ctx, s.vm, fmt.Sprintf("wc -c < %s", shellQuote(raw)), 0)
	if err != nil {
		if strings.Contains(err.Error(), "No space left on device") || strings.Contains(sizeOut, "No space left on device") {
			return 0, fmt.Errorf("capture failed: no space left on device")
		}
		return 0, fmt.Errorf("capture stat failed: %w", err)
	}
	size, err := strconv.ParseInt(strings.TrimSpace(sizeOut), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("capture stat failed: unexpected size %q", strings.TrimSpace(sizeOut))
	}
	return size, nil
}

func (s *Store) checkCaps(size int64) error {
	if maxCmd := int64(s.cfg.MaxCommandSize); maxCmd > 0 && size > maxCmd {
		return fmt.Errorf("capture of %d bytes exceeds output.max_command_size %d", size, maxCmd)
	}
	if maxTotal := int64(s.cfg.MaxTotalSize); maxTotal > 0 && s.totalBytes+size > maxTotal {
		return fmt.Errorf("capture would exceed output.max_total_size %d", maxTotal)
	}
	return nil
}

func (s *Store) shouldInline(mode string, size int64) bool {
	return DecideInline(mode, true, size, EffectiveCutoff(s.cfg, int(s.inlineCutoff))) && mode != "always"
}

func (s *Store) materializeInline(ctx context.Context, subdir, raw string, size int64, exit int) (Result, error) {
	inline := ""
	if size > 0 {
		catOut, err := s.client.Exec(ctx, s.vm, fmt.Sprintf("cat %s", shellQuote(raw)), 0)
		if err != nil {
			return Result{}, err
		}
		inline = catOut
	}
	s.removeSubdir(ctx, subdir)
	if inline == "" {
		inline = "(no output)"
	}
	return Result{Inline: inline, ExitCode: exit}, nil
}

func (s *Store) materializeChunked(ctx context.Context, cmd, oid, subdir, raw string, size int64, exit int) (Result, error) {
	linesOut, err := s.client.Exec(ctx, s.vm, fmt.Sprintf("wc -l < %s", shellQuote(raw)), 0)
	if err != nil {
		return Result{}, fmt.Errorf("capture line count failed: %w", err)
	}
	totalLines, _ := strconv.Atoi(strings.TrimSpace(linesOut))
	chunkLines := s.cfg.ChunkLines
	if chunkLines <= 0 {
		chunkLines = 500
	}
	chunks := 0
	if totalLines > 0 {
		splitCmd := fmt.Sprintf("split -l %d -d -a 5 %s %s", chunkLines, shellQuote(raw), shellQuote(path.Join(subdir, "chunk_")))
		if _, err := s.client.Exec(ctx, s.vm, splitCmd, 0); err != nil {
			return Result{}, fmt.Errorf("capture chunk failed: %w", err)
		}
		chunks = (totalLines + chunkLines - 1) / chunkLines
	}
	h := Handle{
		ID: oid, Path: raw, ChunkDir: subdir + "/",
		ChunkLines: chunkLines, Chunks: chunks,
		TotalLines: totalLines, TotalBytes: size,
		ExitCode: exit, Command: cmd,
	}
	s.handles[oid] = h
	s.totalBytes += size
	return Result{Handle: &h}, nil
}

func (s *Store) Capture(ctx context.Context, command string, force bool) (Result, error) {
	cmd, err := cleanCommand(command)
	if err != nil {
		return Result{}, err
	}
	if err := validation.ValidateShellCommand(s.allowed, cmd); err != nil {
		return Result{}, err
	}
	mode := s.mode()
	if mode == "auto" && !force {
		return s.directExec(ctx, cmd)
	}
	oid := uuid.NewV7().String()
	subdir, raw := capturePaths(s.runDir, oid)
	if err := s.preflight(ctx); err != nil {
		return Result{}, err
	}
	if err := s.ensureSubdir(ctx, subdir); err != nil {
		return Result{}, err
	}
	cleanup := func() { s.removeSubdir(ctx, subdir) }
	marker := "__MPH_EXIT_" + strings.ReplaceAll(oid, "-", "") + ":"
	exit, err := s.runRedirect(ctx, cmd, raw, marker)
	if err != nil {
		if ctx.Err() == nil {
			cleanup()
			return Result{}, err
		}
		// Timed out mid-write: chunk the partial file as-is and report
		// 124, the `timeout(1)` convention for a timed-out command.
		exit = 124
	}
	size, err := s.rawSize(ctx, raw)
	if err != nil {
		cleanup()
		return Result{}, err
	}
	if err := s.checkCaps(size); err != nil {
		cleanup()
		return Result{}, err
	}
	if s.shouldInline(mode, size) {
		res, err := s.materializeInline(ctx, subdir, raw, size, exit)
		if err != nil {
			cleanup()
			return Result{}, err
		}
		return res, nil
	}
	res, err := s.materializeChunked(ctx, cmd, oid, subdir, raw, size, exit)
	if err != nil {
		cleanup()
		return Result{}, err
	}
	return res, nil
}

func (s *Store) lookup(outputID string) (Handle, error) {
	id := strings.TrimSpace(outputID)
	if h, ok := s.handles[id]; ok {
		return h, nil
	}
	if len(s.handles) == 0 {
		return Handle{}, fmt.Errorf("unknown output_id %q (no active handles; use multipass_exec for new commands, not output_search/output_read)", outputID)
	}
	ids := make([]string, 0, len(s.handles))
	for k := range s.handles {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	return Handle{}, fmt.Errorf("unknown output_id %q (active handles: %s; never invent an output_id)", outputID, strings.Join(ids, ", "))
}

func (s *Store) resolveMaxMatches() int {
	if s.cfg.SearchMaxMatches > 0 {
		return s.cfg.SearchMaxMatches
	}
	return 200
}

func buildGrepCmd(chunkDir, pattern string, fixed, ignoreCase bool, maxM int, marker string) string {
	var b strings.Builder
	b.WriteString("{ grep -a -n -H -m ")
	b.WriteString(strconv.Itoa(maxM))
	b.WriteString(" ")
	if fixed {
		b.WriteString("-F ")
	} else {
		b.WriteString("-E ")
	}
	if ignoreCase {
		b.WriteString("-i ")
	}
	b.WriteString("-e ")
	b.WriteString(shellQuote(pattern))
	b.WriteString(" ")
	b.WriteString(shellQuote(chunkDir) + "chunk_*")
	b.WriteString("; echo " + marker + "$?; } | head -n ")
	b.WriteString(strconv.Itoa(maxM + 1))
	return b.String()
}

func parseGrepOutput(out, marker string) (lines []string, exit int, hasMarker bool) {
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), marker); ok {
			hasMarker = true
			if n, perr := strconv.Atoi(strings.TrimSpace(v)); perr == nil {
				exit = n
			}
			continue
		}
		lines = append(lines, line)
	}
	return lines, exit, hasMarker
}

func mapGrepLines(lines []string, chunkLines int) map[int][]int {
	byChunk := map[int][]int{}
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 2 {
			continue
		}
		if !strings.HasPrefix(path.Base(parts[0]), "chunk_") {
			continue
		}
		idx, perr := strconv.Atoi(strings.TrimPrefix(path.Base(parts[0]), "chunk_"))
		if perr != nil {
			continue
		}
		local, perr := strconv.Atoi(parts[1])
		if perr != nil {
			continue
		}
		byChunk[idx] = append(byChunk[idx], AbsoluteLine(idx, chunkLines, local))
	}
	return byChunk
}

func capMatches(byChunk map[int][]int, maxM int) (matches []ChunkMatches, total int, truncated bool) {
	for idx, ls := range byChunk {
		sort.Ints(ls)
		total += len(ls)
		matches = append(matches, ChunkMatches{Chunk: idx, Lines: ls})
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Chunk < matches[j].Chunk })
	if total <= maxM {
		return matches, total, false
	}
	kept := []ChunkMatches{}
	n := 0
	for _, m := range matches {
		if n >= maxM {
			break
		}
		if room := maxM - n; len(m.Lines) > room {
			m.Lines = m.Lines[:room]
		}
		n += len(m.Lines)
		kept = append(kept, m)
	}
	return kept, maxM + 1, true
}

func (s *Store) Search(ctx context.Context, args SearchArgs) (SearchResult, error) {
	h, err := s.lookup(args.OutputID)
	if err != nil {
		return SearchResult{}, err
	}

	if !args.FixedString {
		if err := ValidatePattern(args.Pattern); err != nil {
			return SearchResult{}, err
		}
	}
	if h.Chunks == 0 {
		return SearchResult{OutputID: h.ID}, nil
	}
	maxM := s.resolveMaxMatches()
	marker := "__MPH_GREP_EXIT_" + strings.ReplaceAll(h.ID, "-", "") + ":"

	out, execErr := s.client.Exec(ctx, s.vm, buildGrepCmd(h.ChunkDir, args.Pattern, args.FixedString, args.IgnoreCase, maxM, marker), 0)
	lines, grepExit, hasMarker := parseGrepOutput(out, marker)

	if !hasMarker && execErr != nil {
		return SearchResult{}, fmt.Errorf("search exec failed for output %q: %w", h.ID, execErr)
	}

	if grepExit == 2 {
		return SearchResult{}, fmt.Errorf("search failed for output %q", h.ID)
	}

	if grepExit == 1 || len(lines) == 0 || (len(lines) == 1 && strings.TrimSpace(lines[0]) == "") {
		return SearchResult{OutputID: h.ID}, nil
	}

	matches, total, truncated := capMatches(mapGrepLines(lines, h.ChunkLines), maxM)

	return SearchResult{
		OutputID: h.ID, TotalMatches: total,
		ChunksMatched: len(matches), Matches: matches, Truncated: truncated,
	}, nil
}

func clampReadLimit(limit int) int {
	if limit <= 0 {
		return defaultReadLimit
	}
	if limit > maxReadLines {
		return maxReadLines
	}
	return limit
}

func buildReadCmd(rawPath string, offset, limit int) string {
	end := offset + limit - 1
	return fmt.Sprintf("sed -n '%d,%dp' %s | cut -c1-%d", offset, end, shellQuote(rawPath), maxLineWidth)
}

func (s *Store) Read(ctx context.Context, outputID string, offset, limit int) (ReadResult, error) {
	h, err := s.lookup(outputID)
	if err != nil {
		return ReadResult{}, err
	}
	if offset < 1 {
		return ReadResult{}, fmt.Errorf("offset must be >= 1, got %d", offset)
	}
	limit = clampReadLimit(limit)
	if offset > h.TotalLines {
		return ReadResult{OutputID: h.ID, Offset: offset, Limit: limit, TotalLines: h.TotalLines}, nil
	}
	out, err := s.client.Exec(ctx, s.vm, buildReadCmd(h.Path, offset, limit), 0)
	if err != nil {
		return ReadResult{}, fmt.Errorf("read failed for output %q: %w", h.ID, err)
	}
	lines := strings.Split(out, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	if len(lines) > limit {
		lines = lines[:limit]
	}
	return ReadResult{
		OutputID: h.ID, Offset: offset, Limit: limit,
		TotalLines: h.TotalLines, Lines: lines,
		Truncated: offset+len(lines)-1 < h.TotalLines,
	}, nil
}
