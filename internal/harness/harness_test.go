package harness_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	lxdclient "github.com/canonical/lxd/client"
	"github.com/canonical/lxd/shared/api"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/trollLemon/MPHarness/v2/internal/agent"
	"github.com/trollLemon/MPHarness/v2/internal/config"
	. "github.com/trollLemon/MPHarness/v2/internal/harness"
	"github.com/trollLemon/MPHarness/v2/internal/lxd"
)

type fakeServer struct {
	lxdclient.InstanceServer
}

// The retry loop must share one buffered reader: a fresh bufio.Reader per prompt
// discards whatever the previous one read past the first newline, so a piped
// second answer is lost.
func TestPromptExistingVMReadsEveryAnswer(t *testing.T) {
	in := bufio.NewReader(strings.NewReader("bogus\ny\n"))

	decision, err := PromptExistingVM(in, io.Discard, "vm")
	if err != nil || decision != PromptRetry {
		t.Fatalf("first pass = (%v, %v), want retry", decision, err)
	}
	decision, err = PromptExistingVM(in, io.Discard, "vm")
	if err != nil || decision != PromptAccept {
		t.Fatalf("second pass = (%v, %v), want accept", decision, err)
	}
}

// A failed read must never read as consent.
func TestPromptExistingVMReadFailureIsNotConsent(t *testing.T) {
	in := bufio.NewReader(strings.NewReader(""))

	decision, err := PromptExistingVM(in, io.Discard, "vm")
	if err == nil {
		t.Fatal("EOF must not be treated as consent")
	}
	if decision != PromptRetry {
		t.Fatalf("decision = %v, want retry", decision)
	}
	if errors.Is(err, ErrVMShouldNotBeUsed) {
		t.Fatalf("read failure should not be a rejection, got %v", err)
	}
}

func TestPromptExistingVMRejection(t *testing.T) {
	in := bufio.NewReader(strings.NewReader("n\n"))

	decision, err := PromptExistingVM(in, io.Discard, "vm")
	if err != nil || decision != PromptReject {
		t.Fatalf("want reject, got (%v, %v)", decision, err)
	}
}

// A bare Enter is a non-answer, not a read failure: it must re-prompt rather
// than abort the run.
func TestPromptExistingVMEmptyLineRetries(t *testing.T) {
	in := bufio.NewReader(strings.NewReader("\n \n"))

	for i := 0; i < 2; i++ {
		decision, err := PromptExistingVM(in, io.Discard, "vm")
		if err != nil || decision != PromptRetry {
			t.Fatalf("pass %d = (%v, %v), want retry", i, decision, err)
		}
	}
	decision, err := PromptExistingVM(in, io.Discard, "vm")
	if !errors.Is(err, ErrPromptRead) || decision != PromptRetry {
		t.Fatalf("exhausted input = (%v, %v), want retry with ErrPromptRead", decision, err)
	}
}

func TestConfirmExistingVMAcceptsAfterInvalid(t *testing.T) {
	in := bufio.NewReader(strings.NewReader("bogus\ny\n"))

	if err := ConfirmExistingVM(in, io.Discard, "vm", 3); err != nil {
		t.Fatalf("ConfirmExistingVM: %v", err)
	}
}

func TestConfirmExistingVMRejects(t *testing.T) {
	in := bufio.NewReader(strings.NewReader("n\n"))

	if err := ConfirmExistingVM(in, io.Discard, "vm", 3); !errors.Is(err, ErrVMShouldNotBeUsed) {
		t.Fatalf("want ErrVMShouldNotBeUsed, got %v", err)
	}
}

func TestConfirmExistingVMIsBounded(t *testing.T) {
	in := bufio.NewReader(strings.NewReader(strings.Repeat("bogus\n", 20)))

	if err := ConfirmExistingVM(in, io.Discard, "vm", 3); !errors.Is(err, ErrPromptAttemptsExceeded) {
		t.Fatalf("want ErrPromptAttemptsExceeded, got %v", err)
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
			err := WriteCommandHistory(ctx, hist, tt.writer)

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

func TestStartRootSpan(t *testing.T) {
	origTP := otel.GetTracerProvider()
	defer otel.SetTracerProvider(origTP)

	exp := tracetest.NewInMemoryExporter()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)))

	cfg := config.Config{VM: config.VMConfig{Name: ""}, Prompt: "task"}
	if _, err := Start(context.Background(), nil, lxd.NewWithServer(fakeServer{}, zerolog.Nop(), nil), cfg, Options{}); err == nil {
		t.Fatal("Start expected error with empty VM name")
	}

	spans := exp.GetSpans()
	if len(spans) != 2 || spans[0].Name != "vm.info" || spans[1].Name != "mph.run" {
		t.Fatalf("spans = %v, want [vm.info, mph.run]", spans)
	}
	if spans[0].Parent.SpanID() != spans[1].SpanContext.SpanID() {
		t.Fatalf("vm.info parent = %v, want mph.run %v", spans[0].Parent.SpanID(), spans[1].SpanContext.SpanID())
	}
	if spans[1].Status.Code != codes.Error {
		t.Fatalf("mph.run status = %v, want Error", spans[1].Status.Code)
	}
}

type cleanupOperation struct {
	lxdclient.Operation
}

func (cleanupOperation) WaitContext(context.Context) error { return nil }
func (cleanupOperation) Cancel() error                     { return nil }

type cleanupServer struct {
	lxdclient.InstanceServer
	deleted         []string
	notFound        bool
	failGetInstance bool
	failGetProfile  bool
}

func (s *cleanupServer) GetInstanceFull(name string) (*api.InstanceFull, string, error) {
	if s.notFound {
		return nil, "", api.StatusErrorf(http.StatusNotFound, "Instance not found")
	}
	return &api.InstanceFull{
		Instance: api.Instance{Name: name, Type: string(api.InstanceTypeVM), StatusCode: api.Running},
	}, "", nil
}

func (s *cleanupServer) GetProfile(string) (*api.Profile, string, error) {
	if s.failGetProfile {
		return nil, "", errors.New("profile read failed")
	}
	return &api.Profile{}, "", nil
}

func (s *cleanupServer) GetInstance(name string) (*api.Instance, string, error) {
	if s.failGetInstance {
		return nil, "", errors.New("instance check failed")
	}
	return &api.Instance{Name: name, Type: string(api.InstanceTypeVM), StatusCode: api.Running}, "", nil
}

func (s *cleanupServer) DeleteInstance(name string, _ bool) (lxdclient.Operation, error) {
	s.deleted = append(s.deleted, name)
	return cleanupOperation{}, nil
}

func TestStartCleansUpVMOnFailure(t *testing.T) {
	for _, keep := range []bool{false, true} {
		t.Run(fmt.Sprintf("keep=%v", keep), func(t *testing.T) {
			server := &cleanupServer{notFound: true, failGetProfile: true}
			client := lxd.NewWithServer(server, zerolog.Nop(), nil)
			agt := agent.NewAgent(zerolog.Nop(), nil, agent.Options{})
			cfg := config.Config{VM: config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "5G"}, Prompt: "task"}

			if _, err := Start(context.Background(), agt, client, cfg, Options{Keep: keep}); err == nil {
				t.Fatal("Start expected Launch to fail")
			}

			want := 0
			if !keep {
				want = 1
			}
			if len(server.deleted) != want {
				t.Fatalf("deletes = %v, want %d", server.deleted, want)
			}
		})
	}
}

func TestStartKeepsExistingVMAfterFailure(t *testing.T) {
	server := &cleanupServer{failGetInstance: true}
	client := lxd.NewWithServer(server, zerolog.Nop(), nil)
	agt := agent.NewAgent(zerolog.Nop(), nil, agent.Options{})
	cfg := config.Config{VM: config.VMConfig{Name: "vm"}, Prompt: "task"}

	if _, err := Start(context.Background(), agt, client, cfg, Options{IgnoreExisting: true}); err == nil {
		t.Fatal("Start expected EnsureRunning to fail")
	}
	if len(server.deleted) != 0 {
		t.Fatalf("deletes = %v, want none for a pre-existing VM", server.deleted)
	}
}
