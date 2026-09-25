package validation

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

var blockedShells = map[string]bool{
	"bash": true,
	"sh":   true,
	"zsh":  true,
	"fish": true,
	"dash": true,
	"ksh":  true,
	"csh":  true,
	"tcsh": true,
}

func isBlockedShell(name string) bool {
	base := filepath.Base(strings.TrimSpace(name))
	base = strings.ToLower(base)
	return blockedShells[base]
}

// ValidateShellCommand parses a raw shell command string into an AST and checks
// that every executed command name is in the allowed set. Command substitution
// (e.g. $(...)), pipes, &&, ||, and ; are handled by the shell grammar itself.
// Shell binaries (bash, sh, zsh, fish, dash, ksh, csh, tcsh) are always blocked
// even in allow-all mode; piping (e.g. "ps aux | grep nginx") works via the
// outer "bash -c" wrapper without explicit shell invocation.
// Returns nil when the allowlist is empty (allow-all mode) and no blocked shells are found.
func ValidateShellCommand(allowed map[string]bool, cmd string) error {
	file, err := syntax.NewParser().Parse(strings.NewReader(cmd), "command.sh")
	if err != nil {
		return fmt.Errorf("failed to parse command %q: %w", cmd, err)
	}

	var notAllowed []string
	var checkErrs error
	syntax.Walk(file, func(node syntax.Node) bool {
		if call, ok := node.(*syntax.CallExpr); ok {
			var err error
			notAllowed, err = checkCall(call, allowed, notAllowed)
			checkErrs = errors.Join(checkErrs, err)
		}
		return true
	})

	if checkErrs != nil {
		return fmt.Errorf("failed to verify command %q: %w", cmd, checkErrs)
	}

	if len(notAllowed) > 0 {
		return fmt.Errorf("commands %v not in allowed list", notAllowed)
	}
	return nil
}

func isCommandAllowed(allowed map[string]bool, name string) bool {
	if len(allowed) == 0 {
		return true
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	if allowed[name] {
		return true
	}
	base := strings.Fields(name)[0]
	return allowed[base]
}

func checkCall(call *syntax.CallExpr, allowed map[string]bool, notAllowed []string) ([]string, error) {
	name, ok := parseCommandName(call)
	if !ok {
		return notAllowed, nil
	}
	if name == "true" || name == "false" {
		return notAllowed, nil
	}
	if name == "" {
		// A non-literal command name (e.g. "$EDITOR") can't be statically
		// verified; fail rather than allow it.
		return notAllowed, errors.New("non-literal command name can't be verified")
	}
	if isBlockedShell(name) {
		return notAllowed, fmt.Errorf("shell %q is blocked: use direct commands instead (piping like \"ps aux | grep nginx\" works without explicit shell invocation)", name)
	}
	if isCommandAllowed(allowed, name) {
		return notAllowed, nil
	}

	newNotAllowed := append(notAllowed, name)

	return newNotAllowed, nil
}

func parseCommandName(call *syntax.CallExpr) (string, bool) {
	if len(call.Args) == 0 {
		return "", false
	}
	return wordLiteral(call.Args[0]), true
}

func wordLiteral(w *syntax.Word) string {
	if w == nil {
		return ""
	}
	var b strings.Builder
	for _, part := range w.Parts {
		s, ok := partLiteral(part)
		if !ok {
			return ""
		}
		b.WriteString(s)
	}
	return b.String()
}

func partLiteral(part syntax.WordPart) (string, bool) {
	switch p := part.(type) {
	case *syntax.Lit:
		return p.Value, true
	case *syntax.SglQuoted:
		return p.Value, true
	case *syntax.DblQuoted:
		var b strings.Builder
		for _, qp := range p.Parts {
			s, ok := partLiteral(qp)
			if !ok {
				return "", false
			}
			b.WriteString(s)
		}
		return b.String(), true
	default:
		return "", false
	}
}
