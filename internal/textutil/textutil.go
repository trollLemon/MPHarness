// Package textutil holds text helpers shared across the harness, the agent, and
// the VM client.
package textutil

import (
	"fmt"
	"unicode/utf8"
)

// Truncate caps s at n bytes and appends a marker stating how much was dropped.
// A non-positive n means no cap. The cut backs off to the nearest rune boundary
// so the result is always valid UTF-8.
func Truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return fmt.Sprintf("%s…(truncated, %d of %d bytes)", trimPartialRuneTail(s[:n]), n, len(s))
}

func trimPartialRuneTail(s string) string {
	for len(s) > 0 {
		if r, size := utf8.DecodeLastRuneInString(s); r == utf8.RuneError && size <= 1 {
			s = s[:len(s)-1]
			continue
		}
		break
	}
	return s
}
