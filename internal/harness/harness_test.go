package harness

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestOutputRunDirCommands(t *testing.T) {
	cmds := outputRunDirCommands("run-123")
	if len(cmds) != 1 {
		t.Fatalf("want 1 cmd, got %v", cmds)
	}
	for _, c := range cmds {
		if strings.Contains(c, "rm ") {
			t.Fatalf("setup must not delete other runs' captures: %q", c)
		}
		if !strings.Contains(c, "run-123") {
			t.Fatalf("setup must target this run, got %q", c)
		}
	}
}

// The retry loop must share one buffered reader: a fresh bufio.Reader per prompt
// discards whatever the previous one read past the first newline, so a piped
// second answer is lost.
func TestPromptExistingVMReadsEveryAnswer(t *testing.T) {
	in := bufio.NewReader(strings.NewReader("bogus\ny\n"))

	if err := promptExistingVM(in, io.Discard, "vm"); err != errInvalidInput {
		t.Fatalf("first pass want errInvalidInput, got %v", err)
	}
	if err := promptExistingVM(in, io.Discard, "vm"); err != nil {
		t.Fatalf("second pass want consent, got %v", err)
	}
}

// A failed read must never read as consent.
func TestPromptExistingVMReadFailureIsNotConsent(t *testing.T) {
	in := bufio.NewReader(strings.NewReader(""))

	err := promptExistingVM(in, io.Discard, "vm")
	if err == nil {
		t.Fatal("EOF must not be treated as consent")
	}
	if errors.Is(err, errVMShouldNotBeUsed) || errors.Is(err, errInvalidInput) {
		t.Fatalf("read failure should be its own error, got %v", err)
	}
}

func TestPromptExistingVMRejection(t *testing.T) {
	in := bufio.NewReader(strings.NewReader("n\n"))

	if err := promptExistingVM(in, io.Discard, "vm"); !errors.Is(err, errVMShouldNotBeUsed) {
		t.Fatalf("want errVMShouldNotBeUsed, got %v", err)
	}
}

// A bare Enter is a non-answer, not a read failure: it must re-prompt rather
// than abort the run.
func TestPromptExistingVMEmptyLineRetries(t *testing.T) {
	in := bufio.NewReader(strings.NewReader("\n \n"))

	if err := promptExistingVM(in, io.Discard, "vm"); err != errInvalidInput {
		t.Fatalf("bare Enter want errInvalidInput, got %v", err)
	}
	if err := promptExistingVM(in, io.Discard, "vm"); err != errInvalidInput {
		t.Fatalf("whitespace-only want errInvalidInput, got %v", err)
	}
	if err := promptExistingVM(in, io.Discard, "vm"); !errors.Is(err, errPromptRead) {
		t.Fatalf("exhausted input want errPromptRead, got %v", err)
	}
}
