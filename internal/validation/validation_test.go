package validation_test

import (
	"strings"
	"testing"

	"pgregory.net/rapid"

	. "github.com/trollLemon/MPHarness/internal/validation"
)

func TestIsCommandAllowed(t *testing.T) {
	tests := []struct {
		name    string
		allowed map[string]bool
		cmd     string
		want    bool
	}{
		{"allow all when empty", nil, "rm -rf /", true},
		{"allow all when empty map", map[string]bool{}, "any", true},
		{"exact match", map[string]bool{"ls": true}, "ls", true},
		{"base match with args", map[string]bool{"ls": true}, "ls -la /tmp", true},
		{"base match with tab", map[string]bool{"ls": true}, "ls\t-la", true},
		{"not allowed", map[string]bool{"ls": true}, "rm -rf", false},
		{"trimmed input", map[string]bool{"ls": true}, "  ls  ", true},
		{"empty cmd", map[string]bool{"ls": true}, "  ", false},
		{"subcommand allowed via base", map[string]bool{"git": true}, "git status", true},
		{"different binary not allowed", map[string]bool{"apt": true}, "apt-get update", false},
		{"case sensitive", map[string]bool{"LS": true}, "ls", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsCommandAllowed(tt.allowed, tt.cmd); got != tt.want {
				t.Fatalf("IsCommandAllowed(%q)=%v want %v (set %v)", tt.cmd, got, tt.want, tt.allowed)
			}
		})
	}
}

func TestValidateShellCommand(t *testing.T) {
	tests := []struct {
		name    string
		allowed map[string]bool
		cmd     string
		wantErr bool
	}{
		{"allow all when empty", nil, "rm -rf /", false},
		{"allow all when empty map", map[string]bool{}, "any command", false},
		{"simple allowed", map[string]bool{"ls": true}, "ls -la /tmp", false},
		{"simple disallowed", map[string]bool{"ls": true}, "rm -rf /", true},
		{"double ampersand both allowed", map[string]bool{"apt": true}, "apt update && apt install -y curl", false},
		{"double ampersand second disallowed", map[string]bool{"apt": true}, "apt update && rm -rf /", true},
		{"pipe both allowed", map[string]bool{"ps": true, "grep": true}, "ps aux | grep nginx", false},
		{"pipe first disallowed", map[string]bool{"grep": true}, "ps aux | grep nginx", true},
		{"pipe second disallowed", map[string]bool{"ps": true}, "ps aux | grep nginx", true},
		{"semicolon", map[string]bool{"ls": true}, "ls; cat /etc/passwd", true},
		{"semicolon both allowed", map[string]bool{"ls": true, "cat": true}, "ls; cat /etc/passwd", false},
		{"standalone true ignored", map[string]bool{"ls": true}, "ls || true", false},
		{"standalone false ignored", map[string]bool{"ls": true}, "ls && false", false},
		{"true alone filtered out", map[string]bool{"ls": true}, "true", false},
		{"false alone filtered out", map[string]bool{"ls": true}, "false", false},
		{"complex pipeline all allowed", map[string]bool{"cat": true, "sort": true, "head": true}, "cat file.txt | sort | head -5", false},
		{"complex pipeline one disallowed", map[string]bool{"cat": true, "head": true}, "cat file.txt | sort | head -5", true},
		{"flags after command ignored", map[string]bool{"ls": true}, "ls --verbose -la /tmp", false},
		{"flag after command", map[string]bool{"cat": true}, "cat -n /etc/passwd", false},
		{"leading dash treated as command name", map[string]bool{"ls": true}, "-h --help", true},
		{"empty command", map[string]bool{"ls": true}, "", false},
		{"whitespace only", map[string]bool{"ls": true}, "   ", false},
		{"newline separated", map[string]bool{"ls": true, "cat": true}, "ls\ncat /etc/passwd", false},
		{"newline separated one disallowed", map[string]bool{"ls": true}, "ls\ncat /etc/passwd", true},
		{"mixed operators complex", map[string]bool{"apt": true}, "apt update && apt install -y curl || echo failed", true},
		{"subcommand not matching base", map[string]bool{"apt": true}, "apt-get update", true},
		{"exact base match", map[string]bool{"apt-get": true}, "apt-get update -y", false},
		{"sudo requires sudo allowed", map[string]bool{"apt": true}, "sudo apt update", true},
		{"sudo allowed runs inner command", map[string]bool{"sudo": true}, "sudo rm -rf /", false},
		{"sudo allowed with apt", map[string]bool{"sudo": true, "apt": true}, "sudo apt update", false},
		{"sudo alone requires sudo allowed", map[string]bool{"apt": true}, "sudo", true},
		{"sudo flag skipped still requires sudo allowed", map[string]bool{"apt": true}, "sudo -u root apt update", true},
		{"env requires env allowed", map[string]bool{"apt-get": true}, "env FOO=bar apt-get update -y", true},
		{"env allowed with inner command", map[string]bool{"env": true}, "env FOO=bar apt-get update -y", false},
		{"nohup requires nohup allowed", map[string]bool{"curl": true}, "nohup curl -O http://example.com/x", true},
		{"nohup allowed with inner command", map[string]bool{"nohup": true}, "nohup curl -O http://example.com/x", false},
		{"multiple pipes deep", map[string]bool{"cat": true, "awk": true, "sed": true}, "cat file | awk '{print $1}' | sed 's/foo/bar/'", false},
		{"pipe chain one bad", map[string]bool{"cat": true, "sed": true}, "cat file | awk '{print $1}' | sed 's/foo/bar/'", true},
		{"command with dash name", map[string]bool{"my-tool": true}, "my-tool --flag value", false},
		{"subshell hidden command caught", map[string]bool{"ls": true}, "ls $(rm -rf /)", true},
		{"backtick hidden command caught", map[string]bool{"echo": true}, "echo `rm -rf /`", true},
		{"subshell command allowed", map[string]bool{"echo": true, "ls": true, "pwd": true}, "echo $(pwd) && ls", false},
		{"quoted command name literal", map[string]bool{"ls": true}, "'ls' -la /tmp", false},
		{"backslash prefix fails closed", map[string]bool{"ls": true}, "\\ls -la", true},
		{"variable command expansion fails closed", map[string]bool{"ls": true}, "$EDITOR /etc/hosts", true},
		{"env assignment only no command", map[string]bool{"ls": true}, "FOO=bar", false},
		{"bash -c blocked even when bash allowed", map[string]bool{"bash": true}, "bash -c 'rm -rf /'", true},
		{"bash -c denied when bash not allowed", map[string]bool{"echo": true}, "bash -c 'echo hi'", true},
		{"sh -c blocked even when sh allowed", map[string]bool{"sh": true}, "sh -c 'echo hi'", true},
		{"zsh -c blocked even when zsh allowed", map[string]bool{"zsh": true}, "zsh -c 'echo hi'", true},
		{"fish -c blocked even when fish allowed", map[string]bool{"fish": true}, "fish -c 'echo hi'", true},
		{"bare bash blocked even when allowed", map[string]bool{"bash": true}, "bash --version", true},
		{"bare sh blocked even when allowed", map[string]bool{"sh": true}, "sh --version", true},
		{"bash blocked in allow-all mode", nil, "bash -c 'echo hi'", true},
		{"sh blocked in allow-all empty map", map[string]bool{}, "sh -c 'echo hi'", true},
		{"path qualified bash blocked", map[string]bool{"/bin/bash": true}, "/bin/bash -c 'echo hi'", true},
		{"path qualified sh blocked", map[string]bool{"bash": true}, "/usr/bin/sh -c 'id'", true},
		{"dash blocked", map[string]bool{"dash": true}, "dash -c 'echo hi'", true},
		{"piping without explicit shell still works", map[string]bool{"ps": true, "grep": true}, "ps aux | grep nginx", false},
		{"malformed syntax rejected", map[string]bool{"echo": true}, "echo \"unclosed", true},
		{"process substitution command caught", map[string]bool{"ls": true}, "diff <(rm -rf /) <(ls)", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateShellCommand(tt.allowed, tt.cmd)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateShellCommand(%q) error = %v, wantErr %v", tt.cmd, err, tt.wantErr)
			}
		})
	}
}

// Property-based tests for validation using rapid.

// IsCommandAllowed allows everything when allowlist is empty/nil.
func TestIsCommandAllowedEmptyAllowsAll(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		cmd := rapid.StringN(1, 20, 50).Draw(t, "cmd")
		// nil allowlist
		if !IsCommandAllowed(nil, cmd) {
			t.Fatalf("nil allowlist should allow %q", cmd)
		}
		// empty map
		if !IsCommandAllowed(map[string]bool{}, cmd) {
			t.Fatalf("empty allowlist should allow %q", cmd)
		}
	})
}

// IsCommandAllowed rejects empty/whitespace commands when allowlist is non-empty.
func TestIsCommandAllowedEmptyCommandRejected(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		allowed := map[string]bool{"ls": true}
		emptyCmd := rapid.OneOf(
			rapid.Just(""),
			rapid.Just(" "),
			rapid.Just("\t"),
			rapid.Just("  \t  "),
		).Draw(t, "emptyCmd")
		if IsCommandAllowed(allowed, emptyCmd) {
			t.Fatalf("empty command %q should be rejected with non-empty allowlist", emptyCmd)
		}
	})
}

// IsCommandAllowed is consistent for whitespace-trimmed commands.
func TestIsCommandAllowedTrimConsistency(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate alphanumeric command names only
		cmd := rapid.StringN(1, 20, 50).Draw(t, "cmd")
		// Skip commands with whitespace
		if strings.TrimSpace(cmd) != cmd {
			return
		}
		allowed := map[string]bool{cmd: true}
		paddedCmd := "   " + cmd + "   "
		if !IsCommandAllowed(allowed, paddedCmd) {
			t.Fatalf("padded command %q should match %q", paddedCmd, cmd)
		}
	})
}

// IsCommandAllowed is case-sensitive.
func TestIsCommandAllowedIsCaseSensitive(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		cmd := "ls"
		allowed := map[string]bool{"LS": true}
		if IsCommandAllowed(allowed, cmd) {
			t.Fatalf("command %q should not match %q (case sensitive)", cmd, "LS")
		}
	})
}

// ValidateShellCommand rejects blocked shells in any context.
func TestValidateShellCommandBlocksShells(t *testing.T) {
	blocked := []string{"bash", "sh", "zsh", "fish", "dash", "ksh", "csh", "tcsh"}
	for _, shell := range blocked {
		t.Run(shell, func(t *testing.T) {
			err := ValidateShellCommand(nil, shell+" -c 'echo hi'")
			if err == nil {
				t.Fatalf("shell %q should be blocked", shell)
			}
		})
	}
}

// ValidateShellCommand accepts simple commands when allowlist is empty.
func TestValidateShellCommandEmptyAllowsSimple(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		cmd := rapid.OneOf(
			rapid.Just("ls"),
			rapid.Just("cat /etc/passwd"),
			rapid.Just("echo hello"),
			rapid.Just("pwd"),
		).Draw(t, "cmd")
		err := ValidateShellCommand(nil, cmd)
		if err != nil {
			t.Fatalf("simple command %q should be allowed with empty allowlist, got: %v", cmd, err)
		}
	})
}

// ValidateShellCommand rejects malformed syntax.
func TestValidateShellCommandRejectsMalformed(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Unclosed quote
		cmd := `echo "unclosed`
		err := ValidateShellCommand(nil, cmd)
		if err == nil {
			t.Fatalf("malformed command %q should be rejected", cmd)
		}
	})
}

// ValidateShellCommand allows commands in allowlist.
func TestValidateShellCommandAllowsListed(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		cmd := "ls -la"
		allowed := map[string]bool{"ls": true}
		err := ValidateShellCommand(allowed, cmd)
		if err != nil {
			t.Fatalf("command %q should be allowed with %v, got: %v", cmd, allowed, err)
		}
	})
}

// ValidateShellCommand rejects commands not in allowlist.
func TestValidateShellCommandRejectsNotListed(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		cmd := "rm -rf /"
		allowed := map[string]bool{"ls": true, "cat": true}
		err := ValidateShellCommand(allowed, cmd)
		if err == nil {
			t.Fatalf("command %q should be rejected with allowlist %v", cmd, allowed)
		}
	})
}

// ValidateShellCommand handles commands with leading/trailing whitespace.
func TestValidateShellCommandWhitespaceVariants(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		prefixSpaces := rapid.IntRange(0, 5).Draw(t, "prefixSpaces")
		suffixSpaces := rapid.IntRange(0, 5).Draw(t, "suffixSpaces")
		prefix := strings.Repeat(" ", prefixSpaces)
		suffix := strings.Repeat(" ", suffixSpaces)
		cmd := prefix + "ls" + suffix
		allowed := map[string]bool{"ls": true}
		err := ValidateShellCommand(allowed, cmd)
		if err != nil {
			t.Fatalf("padded command %q should be allowed with 'ls' in allowlist, got: %v", cmd, err)
		}
	})
}
