package output

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"uuid"

	"github.com/trollLemon/MPHarness/v2/internal/config"
)

type Handle struct {
	ID         string
	TotalBytes int64
	Content    string
}

type Result struct {
	Inline string
	Handle *Handle
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
	Matches      []string
}

const (
	NoOutput = "(no output)"

	defaultReadLimit = 50
)

// Store keeps every captured command output in memory for one run.
type Store struct {
	cfg          config.OutputConfig
	inlineCutoff int64
	handles      map[string]*Handle
}

// NewStore returns an in-memory Store configured with cfg and inlineCutoff.
func NewStore(cfg config.OutputConfig, inlineCutoff int64) *Store {
	return &Store{
		cfg:          cfg,
		inlineCutoff: inlineCutoff,
		handles:      make(map[string]*Handle),
	}
}

// Capture decides whether out is returned inline or stored in memory via a handle.
func (s *Store) Capture(out string) (Result, error) {
	size := int64(len(out))

	if out == "" {
		return Result{Inline: NoOutput}, nil
	}

	if s.cfg.Mode == "always" || (s.inlineCutoff > 0 && size > s.inlineCutoff) {
		id := uuid.NewV7().String()

		handle := &Handle{
			ID:         id,
			TotalBytes: size,
			Content:    out,
		}

		s.handles[id] = handle
		return Result{Handle: handle}, nil
	}

	return Result{Inline: out}, nil
}

// unknownOutputID reports a lookup failure with enough context for the model
// to recover: the handles it could have used, or that none exist yet.
func (s *Store) unknownOutputID(id string) error {
	if len(s.handles) == 0 {
		return fmt.Errorf("unknown output_id %q; no output is captured yet, never invent an output_id", id)
	}
	ids := make([]string, 0, len(s.handles))
	for id := range s.handles {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return fmt.Errorf("unknown output_id %q; active output_ids: %s", id, strings.Join(ids, ", "))
}

// Search returns every line of the captured output that matches args.Pattern.
func (s *Store) Search(args SearchArgs) (SearchResult, error) {
	if strings.TrimSpace(args.Pattern) == "" {
		return SearchResult{}, errors.New("pattern is required")
	}

	h, ok := s.handles[strings.TrimSpace(args.OutputID)]
	if !ok {
		return SearchResult{}, s.unknownOutputID(args.OutputID)
	}

	expr := args.Pattern
	if args.FixedString {
		expr = regexp.QuoteMeta(expr)
	}
	flags := "(?m)"
	if args.IgnoreCase {
		flags = "(?mi)"
	}
	re, err := regexp.Compile(flags + "^.*(?:" + expr + ").*$")
	if err != nil {
		return SearchResult{}, fmt.Errorf("invalid pattern %q: %w", args.Pattern, err)
	}

	matches := re.FindAllString(h.Content, -1)
	return SearchResult{
		OutputID:     h.ID,
		TotalMatches: len(matches),
		Matches:      matches,
	}, nil
}

// Read returns the bytes of the captured output between offset (inclusive) and
// limit (exclusive).
func (s *Store) Read(outputID string, offset, limit int) (string, error) {
	h, ok := s.handles[strings.TrimSpace(outputID)]
	if !ok {
		return "", s.unknownOutputID(outputID)
	}

	if offset < 0 {
		return "", errors.New("offset must be >=0")
	}
	if offset > len(h.Content) {
		return "", fmt.Errorf("offset %d is beyond the captured output of %d bytes", offset, len(h.Content))
	}

	if limit <= 0 {
		limit = defaultReadLimit
	}
	if limit > len(h.Content) {
		limit = len(h.Content)
	}
	if offset > limit {
		return "", nil
	}

	return h.Content[offset:limit], nil
}
