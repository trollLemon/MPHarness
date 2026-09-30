package output

import (
	"context"
	"errors"
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

type SearchResult struct {
	OutputID     string
	TotalMatches int
	Matches      []int
	Truncated    bool
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

// DecideInline reports whether a captured command's output should be returned
// inline to the agent rather than as a handle. Callers reach this only once the
// output has actually been captured, so the non-forced "auto" case (run it
// directly instead) is handled by Capture before any bytes are written.
func DecideInline(mode string, size, cutoff int64) bool {
	if mode == "always" {
		return false
	}
	if cutoff <= 0 {
		cutoff = 1 << 30
	}
	return size < cutoff
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

// errNoExitMarker means the harness's own marker never reached the output, so
// the command's real exit status is unknown. Reporting 0 would tell the model a
// failed command succeeded.
var errNoExitMarker = errors.New("command produced no exit marker")

// wrapCapture isolates the model-supplied command in its own shell group and
// pipes it through `head -c` so a runaway command is cut off near the cap
// rather than filling the VM disk. PIPESTATUS carries the command's own status
// rather than head's.
func wrapCapture(cmd, raw, marker string, maxBytes int64) string {
	var b strings.Builder
	b.WriteString("{\n")
	b.WriteString(cmd)
	b.WriteString("\n} 2>&1")
	if maxBytes > 0 {
		b.WriteString(" | head -c ")

		// The cap is read one byte over: a file of exactly maxBytes is indistinguishable
		// from a silently truncated one, whereas maxBytes+1 proves the command ran past
		// the limit and lets checkCaps reject it with a real error.
		b.WriteString(strconv.FormatInt(maxBytes+1, 10))
	}
	b.WriteString(" > ")
	b.WriteString(shellQuote(raw))
	b.WriteString("\necho ")
	b.WriteString(marker)
	b.WriteString("${PIPESTATUS[0]}\n")
	return b.String()
}

func (s *Store) runRedirect(ctx context.Context, cmd, raw, marker string) (int, error) {
	execOut, execErr := s.client.Exec(ctx, s.vm, wrapCapture(cmd, raw, marker, int64(s.cfg.MaxCommandSize)), 0)
	if strings.Contains(execOut, "No space left on device") {
		return 0, fmt.Errorf("capture failed: no space left on device")
	}
	if exit, ok := parseExitMarker(execOut, marker); ok {
		return exit, nil
	}
	if execErr != nil {
		return 0, execErr
	}
	return 0, errNoExitMarker
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
	total := s.totalBytes
	if maxTotal := int64(s.cfg.MaxTotalSize); maxTotal > 0 && total+size > maxTotal {
		return fmt.Errorf("capture would exceed output.max_total_size %d", maxTotal)
	}
	return nil
}

func (s *Store) shouldInline(mode string, size int64) bool {
	return DecideInline(mode, size, EffectiveCutoff(s.cfg, int(s.inlineCutoff)))
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

func (s *Store) materializeHandle(ctx context.Context, cmd, oid, raw string, size int64, exit int) (Result, error) {
	linesOut, err := s.client.Exec(ctx, s.vm, fmt.Sprintf("wc -l < %s", shellQuote(raw)), 0)
	if err != nil {
		return Result{}, fmt.Errorf("capture line count failed: %w", err)
	}
	totalLines, _ := strconv.Atoi(strings.TrimSpace(linesOut))
	h := Handle{
		ID: oid, Path: raw,
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
	// Once the command's context is done, follow-up work must not reuse it or
	// the stat below fails on a cancelled context and the partial output that
	// was already written gets thrown away.
	workCtx := ctx
	if err != nil {
		if ctx.Err() == nil {
			cleanup()
			return Result{}, err
		}
		// Timed out mid-write: keep the partial file as-is and report 124,
		// the `timeout(1)` convention for a timed-out command.
		exit = 124
		workCtx = context.WithoutCancel(ctx)
	}
	size, err := s.rawSize(workCtx, raw)
	if err != nil {
		cleanup()
		return Result{}, err
	}
	if err := s.checkCaps(size); err != nil {
		cleanup()
		return Result{}, err
	}
	if s.shouldInline(mode, size) {
		res, err := s.materializeInline(workCtx, subdir, raw, size, exit)
		if err != nil {
			cleanup()
			return Result{}, err
		}
		return res, nil
	}
	res, err := s.materializeHandle(workCtx, cmd, oid, raw, size, exit)
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

// resolveMaxMatches returns the per-search cap on reported line numbers, or 0
// for unlimited.
func (s *Store) resolveMaxMatches() int {
	return s.cfg.SearchMaxMatches
}

func buildGrepCmd(rawPath, pattern string, fixed, ignoreCase bool, maxM int, marker string) string {
	var b strings.Builder
	b.WriteString("{ grep -a -n ")
	if fixed {
		b.WriteString("-F ")
	} else {
		b.WriteString("-E ")
	}
	if ignoreCase {
		b.WriteString("-i ")
	}
	// -e keeps a pattern that starts with '-' from being read as an option.
	b.WriteString("-e ")
	b.WriteString(shellQuote(pattern))
	b.WriteString(" ")
	b.WriteString(shellQuote(rawPath))
	b.WriteString("; echo " + marker + "$?; }")
	// head is the only bound. It trims the stream to maxM+1 lines, so the exit
	// marker surviving the trim is how Search tells "exactly maxM" from "more".
	// maxM 0 means unlimited, so no trim is applied.
	if maxM > 0 {
		b.WriteString(" | head -n ")
		b.WriteString(strconv.Itoa(maxM + 1))
	}
	return b.String()
}

// buildGrepCountCmd counts every match in the capture. The list of line numbers
// is bounded, but the total must be the real number: a sentinel like maxM+1 is
// indistinguishable from a genuine count and tells the model nothing.
func buildGrepCountCmd(rawPath, pattern string, fixed, ignoreCase bool) string {
	var b strings.Builder
	b.WriteString("grep -a -c ")
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
	b.WriteString(shellQuote(rawPath))
	return b.String()
}

func parseGrepOutput(out, marker string) (lines []string, exit int, hasMarker bool) {
	all := strings.Split(out, "\n")
	// The harness always emits the marker last, so only a trailing line is
	// trusted. Model-written content that merely looks like the marker must
	// not be able to dictate the exit status.
	for i := len(all) - 1; i >= 0; i-- {
		line := strings.TrimSpace(all[i])
		if line == "" {
			continue
		}
		if v, ok := strings.CutPrefix(line, marker); ok {
			hasMarker = true
			if n, perr := strconv.Atoi(strings.TrimSpace(v)); perr == nil {
				exit = n
			}
			all = all[:i]
			break
		}
		break
	}
	lines = append(lines, all...)
	return lines, exit, hasMarker
}

// parseGrepLines turns the `line:text` records grep emits for the raw capture
// into absolute line numbers.
func parseGrepLines(lines []string) []int {
	out := make([]int, 0, len(lines))
	for _, line := range lines {
		num, _, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(num)
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// capMatches keeps the first maxM matches. maxM 0 means unlimited, matching a
// disabled cap rather than "keep nothing".
func capMatches(matches []int, maxM int) (kept []int, truncated bool) {
	if maxM <= 0 || len(matches) <= maxM {
		return matches, false
	}
	return matches[:maxM], true
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
	maxM := s.resolveMaxMatches()
	marker := "__MPH_GREP_EXIT_" + strings.ReplaceAll(h.ID, "-", "") + ":"

	out, execErr := s.client.Exec(ctx, s.vm, buildGrepCmd(h.Path, args.Pattern, args.FixedString, args.IgnoreCase, maxM, marker), 0)
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

	matches, truncated := capMatches(parseGrepLines(lines), maxM)
	total := s.countMatches(ctx, h, args, grepExit, len(matches))

	return SearchResult{
		OutputID: h.ID, TotalMatches: total,
		Matches: matches, Truncated: truncated,
	}, nil
}

// countMatches returns the true number of matches in the capture, falling back
// to the number actually listed when the count cannot be obtained.
func (s *Store) countMatches(ctx context.Context, h Handle, args SearchArgs, grepExit, listed int) int {
	if grepExit == 1 {
		return 0
	}
	out, err := s.client.Exec(ctx, s.vm, buildGrepCountCmd(h.Path, args.Pattern, args.FixedString, args.IgnoreCase), 0)
	if err != nil {
		return listed
	}
	// grep -c prints a single number and nothing else.
	n, perr := strconv.Atoi(strings.TrimSpace(out))
	if perr != nil || n < listed {
		return listed
	}
	return n
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
