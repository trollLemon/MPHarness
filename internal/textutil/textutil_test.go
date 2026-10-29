package textutil_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"pgregory.net/rapid"

	. "github.com/trollLemon/MPHarness/v2/internal/textutil"
)

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		s    string
		n    int
		want string
	}{
		{"short unchanged", "hello", 10, "hello"},
		{"long truncated", "hello world", 5, "hello…(truncated, 5 of 11 bytes)"},
		{"empty", "", 5, ""},
		{"exact", "hello", 5, "hello"},
		{"zero means no limit", "hello", 0, "hello"},
		{"negative means no limit", "hello", -1, "hello"},
		{"rune boundary never splits", "aaé", 3, "aa…(truncated, 3 of 4 bytes)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Truncate(tt.s, tt.n)
			if !utf8.ValidString(got) {
				t.Fatalf("Truncate produced invalid UTF-8: %q", got)
			}
			if got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

// Property-based tests for Truncate using rapid.

// Truncate always produces valid UTF-8.
func TestTruncateProducesValidUTF8(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.String().Draw(t, "s")
		n := rapid.Int().Draw(t, "n")
		got := Truncate(s, n)
		if !utf8.ValidString(got) {
			t.Fatalf("Truncate produced invalid UTF-8: %q", got)
		}
	})
}

// When n <= 0, Truncate returns the original string unchanged.
func TestTruncateNonPositiveNReturnsOriginal(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.String().Draw(t, "s")
		n := rapid.IntRange(-1000, 0).Draw(t, "n")
		got := Truncate(s, n)
		if got != s {
			t.Fatalf("Truncate(s, %d) = %q, want %q", n, got, s)
		}
	})
}

// When len(s) <= n (and n > 0), Truncate returns s unchanged.
func TestTruncateShortStringReturnsOriginal(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.StringN(0, 50, 200).Draw(t, "s")
		if len(s) == 0 {
			return
		}
		n := rapid.IntRange(len(s), len(s)+100).Draw(t, "n")
		got := Truncate(s, n)
		if got != s {
			t.Fatalf("Truncate(%q, %d) = %q, want %q", s, n, got, s)
		}
	})
}

// When truncated, result contains the marker.
func TestTruncateLongStringContainsMarker(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.StringN(10, 100, 400).Draw(t, "s")
		n := rapid.IntRange(1, len(s)-1).Draw(t, "n")
		got := Truncate(s, n)
		if !strings.Contains(got, "…(truncated,") {
			t.Fatalf("truncated result missing marker: %q", got)
		}
	})
}

// Truncated result length respects the byte limit (plus marker overhead).
func TestTruncateRespectsByteLimit(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.StringN(10, 100, 400).Draw(t, "s")
		n := rapid.IntRange(1, len(s)-1).Draw(t, "n")
		got := Truncate(s, n)
		// The actual bytes of content (before marker) should be <= n
		markerIdx := strings.Index(got, "…")
		if markerIdx < 0 {
			t.Fatalf("missing truncation marker in %q", got)
		}
		if markerIdx > n {
			t.Fatalf("content exceeds limit: %d > %d", markerIdx, n)
		}
	})
}

// CutPrefix always produces valid UTF-8.
func TestCutPrefixProducesValidUTF8(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.String().Draw(t, "s")
		n := rapid.Int().Draw(t, "n")
		got := CutPrefix(s, n)
		if !utf8.ValidString(got) {
			t.Fatalf("CutPrefix produced invalid UTF-8: %q", got)
		}
	})
}

// When n >= len(s), CutPrefix returns the original string.
func TestCutPrefixLargeNReturnsOriginal(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.StringN(0, 50, 200).Draw(t, "s")
		n := rapid.IntRange(len(s), len(s)+1000).Draw(t, "n")
		got := CutPrefix(s, n)
		if got != s {
			t.Fatalf("CutPrefix(%q, %d) = %q, want %q", s, n, got, s)
		}
	})
}
