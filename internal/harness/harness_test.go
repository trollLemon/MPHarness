package harness

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestOutputRunDirCommand(t *testing.T) {
	cmd := outputRunDirCommand("run-123")
	if strings.Contains(cmd, "rm ") {
		t.Fatalf("setup must not delete other runs' captures: %q", cmd)
	}
	if !strings.Contains(cmd, "run-123") {
		t.Fatalf("setup must target this run, got %q", cmd)
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

var errHistoryWrite = errors.New("history write failed")

type fakeCommandHistory struct {
	calls       int
	gotErr      error
	gotDeadline bool
	err         error
}

func (f *fakeCommandHistory) WriteCommandHistory(ctx context.Context, _ io.Writer) error {
	f.calls++
	f.gotErr = ctx.Err()
	_, f.gotDeadline = ctx.Deadline()
	return f.err
}

func TestWriteCommandHistory(t *testing.T) {
	tests := []struct {
		name         string
		writer       io.Writer
		cancelRun    bool
		histErr      error
		wantCalls    int
		wantErr      error
		wantDeadline bool
	}{
		{
			name:         "writes with a valid writer",
			writer:       io.Discard,
			wantCalls:    1,
			wantDeadline: true,
		},
		{
			name:      "nil writer is a no-op",
			wantCalls: 0,
		},
		{
			name:         "client error is returned",
			writer:       io.Discard,
			histErr:      errHistoryWrite,
			wantCalls:    1,
			wantErr:      errHistoryWrite,
			wantDeadline: true,
		},
		{
			name:         "cancelled run context still flushes",
			writer:       io.Discard,
			cancelRun:    true,
			wantCalls:    1,
			wantDeadline: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.cancelRun {
				cancel()
			}

			hist := &fakeCommandHistory{err: tt.histErr}
			err := writeCommandHistory(ctx, hist, tt.writer)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if hist.calls != tt.wantCalls {
				t.Fatalf("calls = %d, want %d", hist.calls, tt.wantCalls)
			}
			if tt.wantCalls == 0 {
				return
			}
			if hist.gotErr != nil {
				t.Fatalf("flush context err = %v, want detached from the run context", hist.gotErr)
			}
			if hist.gotDeadline != tt.wantDeadline {
				t.Fatalf("flush context deadline present = %v, want %v", hist.gotDeadline, tt.wantDeadline)
			}
		})
	}
}
