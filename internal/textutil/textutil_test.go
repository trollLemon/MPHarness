package textutil

import (
	"testing"
	"unicode/utf8"
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
