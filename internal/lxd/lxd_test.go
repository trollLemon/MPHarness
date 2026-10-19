package lxd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	lxdclient "github.com/canonical/lxd/client"
	"github.com/canonical/lxd/shared/api"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/trollLemon/MPHarness/internal/config"
	"github.com/trollLemon/MPHarness/internal/secrets"
)

type fakeOperation struct {
	lxdclient.Operation
	metadata map[string]any
}

func (o fakeOperation) Get() api.Operation              { return api.Operation{Metadata: o.metadata} }
func (fakeOperation) WaitContext(context.Context) error { return nil }
func (fakeOperation) Cancel() error                     { return nil }

type remoteOperation struct {
	lxdclient.RemoteOperation
	err error
}

func (o remoteOperation) Wait() error       { return o.err }
func (remoteOperation) CancelTarget() error { return nil }

type reply struct {
	out    string
	stderr string
	exit   int
}

func commandReplies(replies map[string]reply) func(string, api.InstanceExecPost, *lxdclient.InstanceExecArgs) (lxdclient.Operation, error) {
	return func(_ string, post api.InstanceExecPost, args *lxdclient.InstanceExecArgs) (lxdclient.Operation, error) {
		key := ""
		if len(post.Command) >= 3 {
			key = post.Command[2]
		}
		r := replies[key]
		writeExecReply(args, r)
		return &fakeOperation{metadata: map[string]any{"return": float64(r.exit)}}, nil
	}
}

func writeExecReply(args *lxdclient.InstanceExecArgs, r reply) {
	if args == nil {
		return
	}
	if args.Stdout != nil {
		_, _ = io.WriteString(args.Stdout, r.out)
	}
	if args.Stderr != nil {
		_, _ = io.WriteString(args.Stderr, r.stderr)
	}
	if args.DataDone != nil {
		close(args.DataDone)
	}
}

var identityReplies = map[string]reply{
	"id -u ubuntu":         {out: "1000\n"},
	"id -g ubuntu":         {out: "1000\n"},
	"getent passwd ubuntu": {out: "ubuntu:x:1000:1000:Ubuntu:/home/ubuntu:/bin/bash\n"},
	"echo ready":           {out: "ready\n"},
}

type createdFile struct {
	name    string
	path    string
	args    lxdclient.InstanceFileArgs
	content string
}

type fakeServer struct {
	lxdclient.InstanceServer

	mu sync.Mutex

	getInstance             func(name string) (*api.Instance, string, error)
	getInstanceFull         func(name string) (*api.InstanceFull, string, error)
	getInstanceState        func(name string) (*api.InstanceState, string, error)
	getProfile              func(name string) (*api.Profile, string, error)
	updateInstanceState     func(name string, state api.InstanceStatePut, etag string) (lxdclient.Operation, error)
	deleteInstance          func(name string, force bool) (lxdclient.Operation, error)
	createInstanceFromImage func(source lxdclient.ImageServer, image api.Image, req api.InstancesPost) (lxdclient.RemoteOperation, error)
	createInstanceFile      func(name, filePath string, args lxdclient.InstanceFileArgs) error
	exec                    func(name string, post api.InstanceExecPost, args *lxdclient.InstanceExecArgs) (lxdclient.Operation, error)
	hasExtension            func(string) bool

	execPosts    []api.InstanceExecPost
	files        []createdFile
	actions      []api.InstanceStatePut
	force        *bool
	launchReq    *api.InstancesPost
	launchSource lxdclient.ImageServer
	launchImage  api.Image
}

func (f *fakeServer) GetInstance(name string) (*api.Instance, string, error) {
	if f.getInstance != nil {
		return f.getInstance(name)
	}
	return nil, "", api.StatusErrorf(http.StatusNotFound, "Instance not found")
}

func (f *fakeServer) GetInstanceFull(name string) (*api.InstanceFull, string, error) {
	if f.getInstanceFull != nil {
		return f.getInstanceFull(name)
	}
	return nil, "", api.StatusErrorf(http.StatusNotFound, "Instance not found")
}

func (f *fakeServer) GetInstanceState(name string) (*api.InstanceState, string, error) {
	if f.getInstanceState != nil {
		return f.getInstanceState(name)
	}
	return &api.InstanceState{StatusCode: api.Running}, "", nil
}

func (f *fakeServer) GetProfile(name string) (*api.Profile, string, error) {
	if f.getProfile != nil {
		return f.getProfile(name)
	}
	return &api.Profile{
		Name: "default",
		Devices: map[string]map[string]string{
			"root": {"type": "disk", "path": "/", "pool": "default"},
		},
	}, "", nil
}

func (f *fakeServer) UpdateInstanceState(name string, state api.InstanceStatePut, etag string) (lxdclient.Operation, error) {
	f.mu.Lock()
	f.actions = append(f.actions, state)
	f.mu.Unlock()
	if f.updateInstanceState != nil {
		return f.updateInstanceState(name, state, etag)
	}
	return &fakeOperation{}, nil
}

func (f *fakeServer) DeleteInstance(name string, force bool) (lxdclient.Operation, error) {
	f.mu.Lock()
	f.force = &force
	f.mu.Unlock()
	if f.deleteInstance != nil {
		return f.deleteInstance(name, force)
	}
	return &fakeOperation{}, nil
}

func (f *fakeServer) HasExtension(extension string) bool {
	if f.hasExtension != nil {
		return f.hasExtension(extension)
	}
	return false
}

func (f *fakeServer) CreateInstanceFromImage(source lxdclient.ImageServer, image api.Image, req api.InstancesPost) (lxdclient.RemoteOperation, error) {
	f.mu.Lock()
	f.launchReq = &req
	f.launchSource = source
	f.launchImage = image
	f.mu.Unlock()
	if f.createInstanceFromImage != nil {
		return f.createInstanceFromImage(source, image, req)
	}
	return remoteOperation{}, nil
}

func (f *fakeServer) CreateInstanceFile(name string, filePath string, args lxdclient.InstanceFileArgs) error {
	if f.createInstanceFile != nil {
		return f.createInstanceFile(name, filePath, args)
	}
	var content []byte
	if args.Content != nil {
		var err error
		content, err = io.ReadAll(args.Content)
		if err != nil {
			return err
		}
	}
	f.mu.Lock()
	f.files = append(f.files, createdFile{name: name, path: filePath, args: args, content: string(content)})
	f.mu.Unlock()
	return nil
}

func (f *fakeServer) ExecInstance(name string, post api.InstanceExecPost, args *lxdclient.InstanceExecArgs) (lxdclient.Operation, error) {
	f.mu.Lock()
	f.execPosts = append(f.execPosts, post)
	f.mu.Unlock()
	if f.exec != nil {
		return f.exec(name, post, args)
	}
	if args != nil && args.DataDone != nil {
		close(args.DataDone)
	}
	return &fakeOperation{}, nil
}

func (f *fakeServer) posts() []api.InstanceExecPost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]api.InstanceExecPost(nil), f.execPosts...)
}

type fakeImageServer struct {
	lxdclient.ImageServer
	imageType   string
	alias       string
	target      string
	fingerprint string
}

func (f *fakeImageServer) GetImageAliasType(imageType, name string) (*api.ImageAliasesEntry, string, error) {
	f.imageType, f.alias = imageType, name
	return &api.ImageAliasesEntry{Name: name, Type: imageType, Target: f.target}, "", nil
}

func (f *fakeImageServer) GetImage(fingerprint string) (*api.Image, string, error) {
	f.fingerprint = fingerprint
	return &api.Image{Fingerprint: fingerprint, Type: "virtual-machine"}, "", nil
}

type mapResolver map[string]string

func (m mapResolver) Lookup(name string) (string, error) {
	if value, ok := m[name]; ok {
		return value, nil
	}
	return "", fmt.Errorf("%w: %s", secrets.ErrUnknownName, name)
}

func newTestClient(server lxdclient.InstanceServer, resolver secrets.Resolver) *Client {
	return NewWithServer(server, zerolog.Nop(), resolver)
}

func TestResolveImageCachesServers(t *testing.T) {
	server := &fakeServer{}
	client := newTestClient(server, nil)
	imageServer := &fakeImageServer{target: "fingerprint-noble"}
	var urls []string
	client.connectImage = func(url string, _ *lxdclient.ConnectionArgs) (lxdclient.ImageServer, error) {
		urls = append(urls, url)
		return imageServer, nil
	}

	for range 2 {
		if _, _, err := client.resolveImage("noble"); err != nil {
			t.Fatalf("resolveImage: %v", err)
		}
	}
	if _, _, err := client.resolveImage("daily:24.04"); err != nil {
		t.Fatalf("resolveImage daily: %v", err)
	}

	if len(urls) != 2 {
		t.Fatalf("connected image servers %v, want 2 (one per remote)", urls)
	}
	if imageServer.imageType != string(api.InstanceTypeVM) {
		t.Fatalf("alias type = %q, want virtual-machine", imageServer.imageType)
	}
	if imageServer.fingerprint != "fingerprint-noble" {
		t.Fatalf("resolved fingerprint = %q, want fingerprint-noble", imageServer.fingerprint)
	}
}

func TestInfo(t *testing.T) {
	server := &fakeServer{}
	client := newTestClient(server, nil)

	if _, err := client.Info(context.Background(), "vm"); !errors.Is(err, ErrInstanceNotFound) {
		t.Fatalf("Info on missing instance = %v, want ErrInstanceNotFound", err)
	}

	server.getInstanceFull = func(string) (*api.InstanceFull, string, error) {
		return &api.InstanceFull{
			Instance: api.Instance{Name: "vm", Type: "virtual-machine", StatusCode: api.Running},
			State:    &api.InstanceState{StatusCode: api.Running},
		}, "", nil
	}
	raw, err := client.Info(context.Background(), "vm")
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("Info returned invalid JSON: %v", err)
	}
	if payload["name"] != "vm" || payload["state"] == nil {
		t.Fatalf("Info payload missing name/state: %s", raw)
	}
}

func TestExecSecretsAndExitCode(t *testing.T) {
	tests := []struct {
		name    string
		replies map[string]reply
		want    string
		wantErr string
	}{
		{
			name:    "success scrubs resolved secret",
			replies: map[string]reply{"echo s3cret": {out: "s3cret\n"}},
			want:    "%{TOKEN}\n",
		},
		{
			name:    "nonzero exit reports status and scrubbed output",
			replies: map[string]reply{"echo s3cret": {out: "boom s3cret\n", exit: 7}},
			wantErr: `vm exec "vm" "echo %{TOKEN}" failed: exit status 7: boom %{TOKEN}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &fakeServer{}
			for k, v := range identityReplies {
				tt.replies[k] = v
			}
			server.exec = commandReplies(tt.replies)
			client := newTestClient(server, mapResolver{"TOKEN": "s3cret"})

			got, err := client.Exec(context.Background(), "vm", "echo %{TOKEN}", 1024)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("Exec error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Exec: %v", err)
			}
			if got != tt.want {
				t.Fatalf("Exec output = %q, want %q", got, tt.want)
			}

			posts := server.posts()
			var exec *api.InstanceExecPost
			for i := range posts {
				if len(posts[i].Command) == 3 && posts[i].Command[2] == "echo s3cret" {
					exec = &posts[i]
				}
			}
			if exec == nil {
				t.Fatal("ExecInstance was never called with the resolved command")
			}
			if exec.User != 1000 || exec.Group != 1000 || exec.Cwd != "/home/ubuntu" {
				t.Fatalf("exec identity = %d:%d cwd %q, want 1000:1000 /home/ubuntu", exec.User, exec.Group, exec.Cwd)
			}
			if exec.Environment["HOME"] != "/home/ubuntu" || exec.Environment["USER"] != "ubuntu" {
				t.Fatalf("exec environment = %v", exec.Environment)
			}
		})
	}
}

const testSecret = "ghp_abc123"

func repliesWithIdentity(extra map[string]reply) map[string]reply {
	replies := make(map[string]reply, len(identityReplies)+len(extra))
	for name, r := range identityReplies {
		replies[name] = r
	}
	for name, r := range extra {
		replies[name] = r
	}
	return replies
}

func newSecretClient(t *testing.T, replies map[string]reply, values map[string]string) (*Client, *fakeServer, *bytes.Buffer) {
	t.Helper()
	var resolver secrets.Resolver
	if values != nil {
		resolver = mapResolver(values)
	}
	var logs bytes.Buffer
	server := &fakeServer{}
	server.exec = commandReplies(replies)
	return NewWithServer(server, zerolog.New(&logs), resolver), server, &logs
}

func TestExecSubstitutesPlaceholdersBeforeTheVM(t *testing.T) {
	client, server, logs := newSecretClient(t,
		repliesWithIdentity(map[string]reply{"sudo pro attach " + testSecret: {out: "ran\n"}}),
		map[string]string{"PRO_TOKEN": testSecret},
	)

	out, err := client.Exec(t.Context(), "vm1", "sudo pro attach %{PRO_TOKEN}", 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out != "ran\n" {
		t.Fatalf("Exec output = %q, want %q", out, "ran\n")
	}

	var exec *api.InstanceExecPost
	posts := server.posts()
	for i := range posts {
		if len(posts[i].Command) == 3 && posts[i].Command[2] == "sudo pro attach "+testSecret {
			exec = &posts[i]
		}
	}
	if exec == nil {
		t.Fatal("ExecInstance was never called with the resolved command")
	}
	if exec.Command[0] != "bash" || exec.Command[1] != "-c" {
		t.Fatalf("command = %v, want bash -c shape", exec.Command)
	}
	if strings.Contains(logs.String(), testSecret) {
		t.Errorf("log output leaked the resolved value: %s", logs.String())
	}
}

func TestExecRedactsResolvedValuesFromOutput(t *testing.T) {
	client, _, logs := newSecretClient(t,
		repliesWithIdentity(map[string]reply{"attach " + testSecret: {out: "pro attach: bad token " + testSecret + "\n"}}),
		map[string]string{"PRO_TOKEN": testSecret},
	)

	out, err := client.Exec(t.Context(), "vm1", "attach %{PRO_TOKEN}", 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.Contains(out, testSecret) {
		t.Errorf("returned output %q leaked the resolved value", out)
	}
	if !strings.Contains(out, "%{PRO_TOKEN}") {
		t.Errorf("returned output = %q, want the value masked as its placeholder", out)
	}
	if strings.Contains(logs.String(), testSecret) {
		t.Errorf("log output leaked the resolved value: %s", logs.String())
	}
}

func TestExecRedactsSecretsFromLaterCommands(t *testing.T) {
	client, _, _ := newSecretClient(t,
		repliesWithIdentity(map[string]reply{
			"attach " + testSecret:         {out: "ran\n"},
			"cat /tmp/mph-output/run1/raw": {out: "s3cret=" + testSecret + "\n"},
		}),
		map[string]string{"PRO_TOKEN": testSecret},
	)

	if _, err := client.Exec(t.Context(), "vm1", "attach %{PRO_TOKEN}", 0); err != nil {
		t.Fatalf("first Exec: %v", err)
	}

	out, err := client.Exec(t.Context(), "vm1", "cat /tmp/mph-output/run1/raw", 0)
	if err != nil {
		t.Fatalf("second Exec: %v", err)
	}
	if strings.Contains(out, testSecret) {
		t.Errorf("a later marker-free command returned the secret: %q", out)
	}
	if !strings.Contains(out, "s3cret=%{PRO_TOKEN}") {
		t.Errorf("returned output = %q, want the value masked as its placeholder", out)
	}
}

func TestExecRedactsEverySecretUsedInTheRun(t *testing.T) {
	client, _, _ := newSecretClient(t,
		repliesWithIdentity(map[string]reply{
			"attach aaa111":                {out: "ran\n"},
			"auth bbb222":                  {out: "ran\n"},
			"cat /tmp/mph-output/run1/raw": {out: "aaa111 bbb222\n"},
		}),
		map[string]string{"PRO_TOKEN": "aaa111", "GH_PAT": "bbb222"},
	)

	if _, err := client.Exec(t.Context(), "vm1", "attach %{PRO_TOKEN}", 0); err != nil {
		t.Fatalf("first Exec: %v", err)
	}
	if _, err := client.Exec(t.Context(), "vm1", "auth %{GH_PAT}", 0); err != nil {
		t.Fatalf("second Exec: %v", err)
	}

	out, err := client.Exec(t.Context(), "vm1", "cat /tmp/mph-output/run1/raw", 0)
	if err != nil {
		t.Fatalf("third Exec: %v", err)
	}
	if want := "%{PRO_TOKEN} %{GH_PAT}"; strings.TrimSpace(out) != want {
		t.Errorf("returned output = %q, want %q", strings.TrimSpace(out), want)
	}
}

func TestExecLeavesMarkerFreeOutputUntouched(t *testing.T) {
	client, _, _ := newSecretClient(t,
		repliesWithIdentity(map[string]reply{"echo hello world": {out: "hello world\n"}}),
		map[string]string{"PRO_TOKEN": testSecret},
	)

	out, err := client.Exec(t.Context(), "vm1", "echo hello world", 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out != "hello world\n" {
		t.Errorf("returned output = %q, want %q", out, "hello world\n")
	}
}

func TestExecRedactsSecretsFromErrors(t *testing.T) {
	client, _, logs := newSecretClient(t,
		repliesWithIdentity(map[string]reply{
			"attach " + testSecret: {stderr: "pro attach: bad token " + testSecret + "\n", exit: 1},
		}),
		map[string]string{"PRO_TOKEN": testSecret},
	)

	_, err := client.Exec(t.Context(), "vm1", "attach %{PRO_TOKEN}", 0)
	if err == nil {
		t.Fatal("Exec succeeded, want the non-zero exit to surface")
	}

	msg := err.Error()
	if strings.Contains(msg, testSecret) {
		t.Errorf("error text leaked the resolved value: %s", msg)
	}
	if !strings.Contains(msg, "%{PRO_TOKEN}") {
		t.Errorf("error text = %s, want the command echoed with its placeholder", msg)
	}
	if strings.Contains(logs.String(), testSecret) {
		t.Errorf("log output leaked the resolved value: %s", logs.String())
	}
}

func TestExecRejectsUnsafeValues(t *testing.T) {
	client, server, _ := newSecretClient(t,
		repliesWithIdentity(nil),
		map[string]string{"PRO_TOKEN": "$(id -u)"},
	)

	if _, err := client.Exec(t.Context(), "vm1", "attach %{PRO_TOKEN}", 0); err == nil {
		t.Fatal("Exec succeeded, want a value containing shell metacharacters rejected")
	}
	if posts := server.posts(); len(posts) != 0 {
		t.Errorf("exec ran %d commands, want none after an unsafe value was rejected", len(posts))
	}
}

func TestExecRejectsPlaceholdersWithoutAResolver(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	client, server, _ := newSecretClient(t, repliesWithIdentity(nil), nil)

	_, err := client.Exec(t.Context(), "vm1", "attach %{PRO_TOKEN}", 0)
	if !errors.Is(err, secrets.ErrUnknownName) {
		t.Fatalf("Exec error = %v, want ErrUnknownName", err)
	}
	if posts := server.posts(); len(posts) != 0 {
		t.Errorf("exec ran %d commands, want none after a resolution failure", len(posts))
	}

	var execSpan sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == "vm.exec" {
			execSpan = s
			break
		}
	}
	if execSpan == nil {
		t.Fatal("no vm.exec span recorded for the failed resolution")
	}
	if execSpan.Status().Code != codes.Error {
		t.Errorf("vm.exec span status = %v, want error", execSpan.Status().Code)
	}
}

func TestExecLeavesMarkerFreeCommandsWorkingWithoutAResolver(t *testing.T) {
	client, _, _ := newSecretClient(t,
		repliesWithIdentity(map[string]reply{"df -h": {out: "ran\n"}}),
		nil,
	)

	out, err := client.Exec(t.Context(), "vm1", "df -h", 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out != "ran\n" {
		t.Errorf("returned output = %q, want %q", out, "ran\n")
	}
}

func TestExecScrubsTransportErrors(t *testing.T) {
	server := &fakeServer{}
	server.exec = func(name string, post api.InstanceExecPost, args *lxdclient.InstanceExecArgs) (lxdclient.Operation, error) {
		if len(post.Command) == 3 && post.Command[2] == "attach s3cret" {
			return nil, fmt.Errorf("cannot exec %q: connection reset", post.Command[2])
		}
		return commandReplies(identityReplies)(name, post, args)
	}
	client := newTestClient(server, mapResolver{"PRO_TOKEN": "s3cret"})

	_, err := client.Exec(context.Background(), "vm", "attach %{PRO_TOKEN}", 1024)
	if err == nil {
		t.Fatal("Exec succeeded, want the transport error to surface")
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("error text leaked the resolved value: %s", err)
	}
	if !strings.Contains(err.Error(), "%{PRO_TOKEN}") {
		t.Fatalf("error text = %s, want the value masked as its placeholder", err)
	}
}

func TestExecDoesNotRecordSecretsInSpans(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	replies := repliesWithIdentity(map[string]reply{
		"sudo pro attach " + testSecret: {out: "ran\n"},
	})
	client, _, _ := newSecretClient(t, replies, map[string]string{"PRO_TOKEN": testSecret})

	if _, err := client.Exec(t.Context(), "vm1", "sudo pro attach %{PRO_TOKEN}", 1024); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	var execSpan sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == "vm.exec" {
			execSpan = s
			break
		}
	}
	if execSpan == nil {
		t.Fatal("no vm.exec span recorded, so this test would prove nothing")
	}

	const wantCommand = "sudo pro attach %{PRO_TOKEN}"
	var sawCommand bool
	for _, attr := range execSpan.Attributes() {
		var vals []string
		switch attr.Value.Type() {
		case attribute.STRING:
			vals = []string{attr.Value.AsString()}
		case attribute.STRINGSLICE:
			vals = attr.Value.AsStringSlice()
		}
		for _, v := range vals {
			if attr.Key == "mph.command" {
				sawCommand = true
				if v != wantCommand {
					t.Errorf("mph.command = %q, want marker form %q", v, wantCommand)
				}
			}
			if strings.Contains(v, testSecret) {
				t.Errorf("span attribute %s leaked the resolved value: %q", attr.Key, v)
			}
		}
	}
	if !sawCommand {
		t.Error("mph.command attribute missing, so the attribute assertions above proved nothing")
	}
}

func TestExecIgnoresNonFloatExitCode(t *testing.T) {
	server := &fakeServer{}
	replies := commandReplies(identityReplies)
	server.exec = func(name string, post api.InstanceExecPost, args *lxdclient.InstanceExecArgs) (lxdclient.Operation, error) {
		if len(post.Command) == 3 && post.Command[2] == "true" {
			if args.DataDone != nil {
				close(args.DataDone)
			}
			return &fakeOperation{metadata: map[string]any{"return": 3}}, nil
		}
		return replies(name, post, args)
	}
	client := newTestClient(server, nil)

	if _, err := client.Exec(context.Background(), "vm", "true", 16); err != nil {
		t.Fatalf("Exec with non-float return metadata: %v", err)
	}
}

func TestExecCombinesStdoutAndStderr(t *testing.T) {
	server := &fakeServer{}
	replies := commandReplies(identityReplies)
	server.exec = func(name string, post api.InstanceExecPost, args *lxdclient.InstanceExecArgs) (lxdclient.Operation, error) {
		if len(post.Command) >= 3 && strings.HasPrefix(post.Command[2], "id ") || len(post.Command) >= 3 && strings.HasPrefix(post.Command[2], "getent ") {
			return replies(name, post, args)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = io.WriteString(args.Stdout, strings.Repeat("o", 512))
		}()
		go func() {
			defer wg.Done()
			_, _ = io.WriteString(args.Stderr, strings.Repeat("e", 512))
		}()
		wg.Wait()
		close(args.DataDone)
		return &fakeOperation{}, nil
	}
	client := newTestClient(server, nil)

	out, err := client.Exec(context.Background(), "vm", "cat big", 64)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.Count(out, "o") != 512 || strings.Count(out, "e") != 512 {
		t.Fatalf("combined output = %d bytes, want 1024", len(out))
	}
}

type cancelOperation struct {
	lxdclient.Operation
	canceled bool
}

func (o *cancelOperation) WaitContext(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (o *cancelOperation) Cancel() error {
	o.canceled = true
	return nil
}

func TestExecCancellationCancelsOperation(t *testing.T) {
	server := &fakeServer{}
	var cancelOp *cancelOperation
	ctx, cancel := context.WithCancel(context.Background())
	replies := commandReplies(identityReplies)
	server.exec = func(name string, post api.InstanceExecPost, args *lxdclient.InstanceExecArgs) (lxdclient.Operation, error) {
		if len(post.Command) >= 3 && post.Command[2] == "sleep forever" {
			cancelOp = &cancelOperation{}
			cancel()
			return cancelOp, nil
		}
		return replies(name, post, args)
	}
	client := newTestClient(server, nil)

	_, err := client.Exec(ctx, "vm", "sleep forever", 64)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Exec error = %v, want context.Canceled", err)
	}
	if cancelOp == nil || !cancelOp.canceled {
		t.Fatal("operation was not cancelled after context cancellation")
	}
}

func TestIdentityCaching(t *testing.T) {
	server := &fakeServer{}
	server.exec = commandReplies(identityReplies)
	client := newTestClient(server, nil)

	for range 2 {
		if _, err := client.Exec(context.Background(), "vm", "echo hi", 64); err != nil {
			t.Fatalf("Exec: %v", err)
		}
	}

	uidCommands := 0
	cloudInitCommands := 0
	sudoChecks := 0
	for _, post := range server.posts() {
		if len(post.Command) != 3 {
			continue
		}
		switch post.Command[2] {
		case "id -u ubuntu":
			uidCommands++
		case "cloud-init status --wait":
			cloudInitCommands++
		case "sudo -u ubuntu sudo -n true":
			sudoChecks++
		}
	}
	if uidCommands != 1 {
		t.Fatalf("identity lookup ran %d times, want once per instance", uidCommands)
	}
	if cloudInitCommands != 1 || sudoChecks != 1 {
		t.Fatalf("guest checks ran %d cloud-init and %d sudo times, want once each", cloudInitCommands, sudoChecks)
	}
}

func TestExecSetsUpGuestUser(t *testing.T) {
	tests := []struct {
		name       string
		userExists bool
		sudoOK     bool
		wantSetup  string
	}{
		{name: "missing user", wantSetup: "useradd --create-home"},
		{name: "user without passwordless sudo", userExists: true, wantSetup: "usermod -aG sudo"},
		{name: "existing user with passwordless sudo", userExists: true, sudoOK: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			userExists := tt.userExists
			server := &fakeServer{}
			server.exec = func(_ string, post api.InstanceExecPost, args *lxdclient.InstanceExecArgs) (lxdclient.Operation, error) {
				command := ""
				if len(post.Command) >= 3 {
					command = post.Command[2]
				}
				r := reply{}
				switch command {
				case "id -u ubuntu":
					if userExists {
						r.out = "1000\n"
					} else {
						r.exit = 1
					}
				case "sudo -u ubuntu sudo -n true":
					if !tt.sudoOK {
						r.exit = 1
					}
				case "id -g ubuntu":
					r.out = "1000\n"
				case "getent passwd ubuntu":
					r.out = "ubuntu:x:1000:1000:Ubuntu:/home/ubuntu:/bin/bash\n"
				case "echo hi":
					r.out = "hi\n"
				case guestUserSetup:
					userExists = true
				}
				writeExecReply(args, r)
				return &fakeOperation{metadata: map[string]any{"return": float64(r.exit)}}, nil
			}

			var logs bytes.Buffer
			client := NewWithServer(server, zerolog.New(&logs), nil)
			out, err := client.Exec(t.Context(), "vm", "echo hi", 64)
			if err != nil {
				t.Fatalf("Exec: %v", err)
			}
			if out != "hi\n" {
				t.Fatalf("Exec output = %q, want %q", out, "hi\n")
			}

			setupPosts := 0
			setupCommand := ""
			for _, post := range server.posts() {
				if len(post.Command) == 3 && post.Command[2] == guestUserSetup {
					setupPosts++
					setupCommand = post.Command[2]
				}
			}
			if tt.wantSetup == "" {
				if setupPosts != 0 {
					t.Fatalf("guest setup ran %d times, want none", setupPosts)
				}
				return
			}
			if setupPosts != 1 {
				t.Fatalf("guest setup ran %d times, want once", setupPosts)
			}
			if !strings.Contains(setupCommand, tt.wantSetup) || !strings.Contains(setupCommand, "NOPASSWD:ALL") {
				t.Fatalf("guest setup command = %q, want %q and passwordless sudo", setupCommand, tt.wantSetup)
			}
			if !strings.Contains(logs.String(), "setting up ubuntu user with passwordless sudo") {
				t.Fatalf("logs = %q, want the guest setup message", logs.String())
			}
		})
	}
}

func TestExecReportsGuestSetupFailure(t *testing.T) {
	server := &fakeServer{}
	server.exec = func(_ string, post api.InstanceExecPost, args *lxdclient.InstanceExecArgs) (lxdclient.Operation, error) {
		command := ""
		if len(post.Command) >= 3 {
			command = post.Command[2]
		}
		r := reply{}
		switch command {
		case "id -u ubuntu":
			r.exit = 1
		case guestUserSetup:
			r.out = "useradd: cannot create user\n"
			r.exit = 1
		}
		writeExecReply(args, r)
		return &fakeOperation{metadata: map[string]any{"return": float64(r.exit)}}, nil
	}
	client := newTestClient(server, nil)

	_, err := client.Exec(context.Background(), "vm", "echo hi", 64)
	if err == nil || !strings.Contains(err.Error(), "set up ubuntu user") {
		t.Fatalf("Exec error = %v, want a guest setup error", err)
	}
	for _, post := range server.posts() {
		if len(post.Command) == 3 && post.Command[2] == "echo hi" {
			t.Fatal("Exec ran the requested command after guest setup failed")
		}
	}
}

func TestWaitForVMReadyRequiresZeroExit(t *testing.T) {
	oldTimeout, oldInterval := readyTimeout, readyPollInterval
	readyTimeout = 50 * time.Millisecond
	readyPollInterval = time.Millisecond
	t.Cleanup(func() {
		readyTimeout, readyPollInterval = oldTimeout, oldInterval
	})

	server := &fakeServer{}
	server.exec = commandReplies(map[string]reply{"echo ready": {out: "ready\n", exit: 1}})
	client := newTestClient(server, nil)

	err := client.waitForVMReady(context.Background(), "vm")
	if err == nil || !strings.Contains(err.Error(), "timeout waiting") {
		t.Fatalf("waitForVMReady = %v, want a readiness timeout", err)
	}

	polls := 0
	for _, post := range server.posts() {
		if len(post.Command) == 3 && post.Command[2] == "echo ready" {
			polls++
		}
	}
	if polls < 1 {
		t.Fatal("never polled the guest, so the readiness check did not run")
	}
}

func TestWaitForVMReadyReportsAgentError(t *testing.T) {
	oldTimeout, oldInterval := readyTimeout, readyPollInterval
	readyTimeout = 50 * time.Millisecond
	readyPollInterval = time.Millisecond
	t.Cleanup(func() {
		readyTimeout, readyPollInterval = oldTimeout, oldInterval
	})

	agentErr := errors.New("LXD VM agent is not currently running")
	server := &fakeServer{}
	server.exec = func(string, api.InstanceExecPost, *lxdclient.InstanceExecArgs) (lxdclient.Operation, error) {
		return nil, agentErr
	}
	client := newTestClient(server, nil)

	err := client.waitForVMReady(context.Background(), "vm")
	if err == nil || !strings.Contains(err.Error(), "timeout waiting") || !strings.Contains(err.Error(), agentErr.Error()) {
		t.Fatalf("waitForVMReady = %v, want a timeout including the agent error", err)
	}
}

func TestEnsureRunning(t *testing.T) {
	tests := []struct {
		name       string
		instance   *api.Instance
		getErr     error
		wantErr    string
		wantStarts int
	}{
		{
			name:       "stopped instance is started",
			instance:   &api.Instance{Name: "vm", Type: "virtual-machine", StatusCode: api.Stopped},
			wantStarts: 1,
		},
		{
			name:       "running instance is left alone",
			instance:   &api.Instance{Name: "vm", Type: "virtual-machine", StatusCode: api.Running},
			wantStarts: 0,
		},
		{
			name:     "container instance is rejected",
			instance: &api.Instance{Name: "vm", Type: "container", StatusCode: api.Running},
			wantErr:  "not a virtual-machine",
		},
		{
			name:    "missing instance",
			getErr:  api.StatusErrorf(http.StatusNotFound, "Instance not found"),
			wantErr: ErrInstanceNotFound.Error(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &fakeServer{}
			server.getInstance = func(string) (*api.Instance, string, error) {
				return tt.instance, "", tt.getErr
			}
			server.exec = commandReplies(identityReplies)
			client := newTestClient(server, nil)

			err := client.EnsureRunning(context.Background(), "vm")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("EnsureRunning error = %v, want %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("EnsureRunning: %v", err)
			}

			starts := 0
			for _, action := range server.actions {
				if action.Action == "start" {
					starts++
				}
			}
			if starts != tt.wantStarts {
				t.Fatalf("start actions = %d, want %d", starts, tt.wantStarts)
			}
		})
	}
}

func TestLaunch(t *testing.T) {
	server := &fakeServer{}
	server.exec = commandReplies(identityReplies)
	client := newTestClient(server, nil)
	imageServer := &fakeImageServer{target: "fingerprint-resolute"}
	var connectedURLs []string
	client.connectImage = func(url string, _ *lxdclient.ConnectionArgs) (lxdclient.ImageServer, error) {
		connectedURLs = append(connectedURLs, url)
		return imageServer, nil
	}

	vm := config.VMConfig{Name: "vm", CPU: 2, RAM: "4096M", Disk: "20G", Image: "resolute"}
	if err := client.Launch(context.Background(), vm); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	req := server.launchReq
	if req == nil {
		t.Fatal("CreateInstanceFromImage was not called")
	}
	if req.Name != "vm" || req.Type != api.InstanceTypeVM || !req.Start {
		t.Fatalf("launch request = %+v", req)
	}
	if req.Config["limits.cpu"] != "2" {
		t.Fatalf("limits.cpu = %q, want 2", req.Config["limits.cpu"])
	}
	if req.Config["limits.memory"] != "4GiB" {
		t.Fatalf("limits.memory = %q, want 4GiB", req.Config["limits.memory"])
	}
	if req.Config["security.secureboot"] != "false" {
		t.Fatalf("security.secureboot = %q, want false", req.Config["security.secureboot"])
	}
	root := req.Devices["root"]
	if root["size"] != "20GiB" || root["type"] != "disk" || root["path"] != "/" || root["pool"] != "default" {
		t.Fatalf("root device = %v", root)
	}
	if imageServer.imageType != string(api.InstanceTypeVM) || imageServer.alias != "resolute" {
		t.Fatalf("alias lookup type=%q alias=%q, want virtual-machine resolute", imageServer.imageType, imageServer.alias)
	}
	if server.launchSource != imageServer {
		t.Fatalf("CreateInstanceFromImage source = %T, want the resolved image server", server.launchSource)
	}
	if server.launchImage.Fingerprint != "fingerprint-resolute" || server.launchImage.Type != "virtual-machine" {
		t.Fatalf("CreateInstanceFromImage image = %+v, want fingerprint-resolute virtual-machine", server.launchImage)
	}
	if len(connectedURLs) != 1 || connectedURLs[0] != "https://cloud-images.ubuntu.com/releases/" {
		t.Fatalf("connected image servers = %v", connectedURLs)
	}

	ready := 0
	for _, post := range server.posts() {
		if len(post.Command) == 3 && post.Command[2] == "echo ready" {
			ready++
		}
	}
	if ready == 0 {
		t.Fatal("Launch never polled the guest for readiness")
	}
}

func TestLaunchInheritsDefaultProfilePool(t *testing.T) {
	server := &fakeServer{}
	server.getProfile = func(name string) (*api.Profile, string, error) {
		if name != "default" {
			t.Fatalf("GetProfile(%q), want default", name)
		}
		return &api.Profile{
			Name: "default",
			Devices: map[string]map[string]string{
				"root": {"type": "disk", "path": "/", "pool": "local", "size": "5GiB"},
			},
		}, "", nil
	}
	server.exec = commandReplies(identityReplies)
	client := newTestClient(server, nil)
	client.connectImage = func(string, *lxdclient.ConnectionArgs) (lxdclient.ImageServer, error) {
		return &fakeImageServer{target: "fingerprint-resolute"}, nil
	}

	vm := config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "10G", Image: "resolute"}
	if err := client.Launch(context.Background(), vm); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	req := server.launchReq
	if req == nil {
		t.Fatal("CreateInstanceFromImage was not called")
	}
	root := req.Devices["root"]
	if root["pool"] != "local" || root["type"] != "disk" || root["path"] != "/" {
		t.Fatalf("root device = %v, want the default profile's pool, type, and path", root)
	}
	if root["size"] != "10GiB" {
		t.Fatalf("root size = %q, want 10GiB", root["size"])
	}
}

func TestLaunchFailsWithoutDefaultProfileRoot(t *testing.T) {
	tests := []struct {
		name    string
		profile *api.Profile
		err     error
		wantErr string
	}{
		{
			name:    "missing default profile",
			err:     api.StatusErrorf(http.StatusNotFound, "Profile not found"),
			wantErr: "read default LXD profile",
		},
		{
			name:    "profile without a root device",
			profile: &api.Profile{Name: "default"},
			wantErr: "no root device",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &fakeServer{}
			server.getProfile = func(string) (*api.Profile, string, error) {
				return tt.profile, "", tt.err
			}
			server.exec = commandReplies(identityReplies)
			client := newTestClient(server, nil)
			client.connectImage = func(string, *lxdclient.ConnectionArgs) (lxdclient.ImageServer, error) {
				return &fakeImageServer{target: "fingerprint-resolute"}, nil
			}

			vm := config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "10G", Image: "resolute"}
			err := client.Launch(context.Background(), vm)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Launch error = %v, want %q", err, tt.wantErr)
			}
			if server.launchReq != nil {
				t.Fatal("CreateInstanceFromImage was called without a usable default profile root device")
			}
		})
	}
}

func TestLaunchSecureBootConfig(t *testing.T) {
	tests := []struct {
		name         string
		hasExtension bool
		wantKey      string
		wantValue    string
		absentKey    string
	}{
		{
			name:         "LXD 6.7 and newer use boot.mode",
			hasExtension: true,
			wantKey:      "boot.mode",
			wantValue:    "uefi-nosecureboot",
			absentKey:    "security.secureboot",
		},
		{
			name:      "older LXD uses security.secureboot",
			wantKey:   "security.secureboot",
			wantValue: "false",
			absentKey: "boot.mode",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &fakeServer{hasExtension: func(string) bool { return tt.hasExtension }}
			server.exec = commandReplies(identityReplies)
			client := newTestClient(server, nil)
			client.connectImage = func(string, *lxdclient.ConnectionArgs) (lxdclient.ImageServer, error) {
				return &fakeImageServer{target: "fingerprint-resolute"}, nil
			}

			vm := config.VMConfig{Name: "vm", CPU: 1, RAM: "1G", Disk: "10G", Image: "resolute"}
			if err := client.Launch(context.Background(), vm); err != nil {
				t.Fatalf("Launch: %v", err)
			}

			req := server.launchReq
			if req == nil {
				t.Fatal("CreateInstanceFromImage was not called")
			}
			if got := req.Config[tt.wantKey]; got != tt.wantValue {
				t.Fatalf("%s = %q, want %q", tt.wantKey, got, tt.wantValue)
			}
			if _, ok := req.Config[tt.absentKey]; ok {
				t.Fatalf("%s present in config %v, want it omitted", tt.absentKey, req.Config)
			}
		})
	}
}

func TestInstallKernel(t *testing.T) {
	const kernelPkg = "linux-image-unsigned-6.8.0-51-generic"

	tests := []struct {
		name      string
		kernelOut string
		wantErr   string
	}{
		{name: "reboots and verifies the new kernel", kernelOut: "6.8.0-51-generic\n"},
		{name: "old kernel after reboot fails", kernelOut: "6.8.0-40-generic\n", wantErr: "kernel version mismatch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &fakeServer{}
			var mu sync.Mutex
			var events []string
			replies := make(map[string]reply, len(identityReplies)+4)
			for k, v := range identityReplies {
				replies[k] = v
			}
			replies["sudo apt-get update && sudo apt-get install -y "+kernelPkg] = reply{}
			replies["sudo update-grub"] = reply{}
			replies["sudo systemctl reboot --no-block"] = reply{}
			replies["uname -r"] = reply{out: tt.kernelOut}
			exec := commandReplies(replies)
			server.exec = func(name string, post api.InstanceExecPost, args *lxdclient.InstanceExecArgs) (lxdclient.Operation, error) {
				mu.Lock()
				if len(post.Command) == 3 {
					events = append(events, "exec:"+post.Command[2])
				}
				mu.Unlock()
				return exec(name, post, args)
			}
			polls := 0
			server.getInstanceState = func(string) (*api.InstanceState, string, error) {
				mu.Lock()
				defer mu.Unlock()
				polls++
				events = append(events, "state")
				status := api.Running
				if polls > 1 {
					status = api.Stopped
				}
				return &api.InstanceState{StatusCode: status}, "", nil
			}
			client := newTestClient(server, nil)

			err := client.InstallKernel(context.Background(), "vm", kernelPkg)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("InstallKernel error = %v, want %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("InstallKernel: %v", err)
			}

			reboot, down, ready, uname := -1, -1, -1, -1
			for i, event := range events {
				switch event {
				case "exec:sudo systemctl reboot --no-block":
					reboot = i
				case "state":
					if down == -1 {
						down = i
					}
				case "exec:echo ready":
					if ready == -1 {
						ready = i
					}
				case "exec:uname -r":
					uname = i
				}
			}
			if reboot == -1 || down == -1 || ready == -1 || uname == -1 {
				t.Fatalf("events = %v, want reboot, state poll, readiness poll and uname", events)
			}
			if reboot >= down || down >= ready || ready >= uname {
				t.Fatalf("events = %v, want reboot < down wait < readiness < uname", events)
			}
			if polls < 2 {
				t.Fatalf("instance state polled %d times, want running then down", polls)
			}
		})
	}
}

func TestInstallKernelFailsWhenGuestDoesNotGoDown(t *testing.T) {
	const kernelPkg = "linux-image-6.8.0-51-generic"

	server := &fakeServer{}
	replies := make(map[string]reply, len(identityReplies)+3)
	for k, v := range identityReplies {
		replies[k] = v
	}
	replies["sudo apt-get update && sudo apt-get install -y "+kernelPkg] = reply{}
	replies["sudo update-grub"] = reply{}
	replies["sudo systemctl reboot --no-block"] = reply{}
	server.exec = commandReplies(replies)
	server.getInstanceState = func(string) (*api.InstanceState, string, error) {
		return &api.InstanceState{StatusCode: api.Running}, "", nil
	}
	client := newTestClient(server, nil)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := client.InstallKernel(ctx, "vm", kernelPkg)
	if err == nil || !strings.Contains(err.Error(), "did not shut down for reboot") {
		t.Fatalf("InstallKernel error = %v, want a down-wait timeout", err)
	}
	for _, post := range server.posts() {
		if len(post.Command) == 3 && (post.Command[2] == "echo ready" || post.Command[2] == "uname -r") {
			t.Fatalf("InstallKernel ran %q after the guest failed to go down", post.Command[2])
		}
	}
}

func TestTransfer(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "a.txt"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("world"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "sub", "b.txt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}

	server := &fakeServer{}
	server.exec = commandReplies(identityReplies)
	var logs strings.Builder
	client := NewWithServer(server, zerolog.New(&logs), nil)

	if err := client.Transfer(context.Background(), "vm", src, "/home/ubuntu/content"); err != nil {
		t.Fatalf("Transfer: %v", err)
	}

	resolvedSrc, err := filepath.EvalSymlinks(src)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "skipping symlink") || !strings.Contains(logs.String(), filepath.Join(resolvedSrc, "link")) {
		t.Fatalf("transfer log = %s, want a warning naming the skipped symlink", logs.String())
	}

	base := filepath.Base(src)
	want := map[string]struct {
		content string
		mode    int
		dir     bool
	}{
		path.Join("/home/ubuntu/content", base):                 {mode: 0o755, dir: true},
		path.Join("/home/ubuntu/content", base, "a.txt"):        {content: "hello", mode: 0o640},
		path.Join("/home/ubuntu/content", base, "sub"):          {mode: 0o755, dir: true},
		path.Join("/home/ubuntu/content", base, "sub", "b.txt"): {content: "world", mode: 0o600},
	}

	if len(server.files) != len(want) {
		t.Fatalf("created %d entries %v, want %d (symlink must be skipped)", len(server.files), server.files, len(want))
	}
	if server.files[0].path != path.Join("/home/ubuntu/content", base) || server.files[0].args.Type != "directory" {
		t.Fatalf("first transfer entry = %+v, want source root directory", server.files[0])
	}
	for _, file := range server.files {
		w, ok := want[file.path]
		if !ok {
			t.Fatalf("unexpected transfer path %q", file.path)
		}
		if file.args.UID != 1000 || file.args.GID != 1000 {
			t.Fatalf("%s ownership = %d:%d, want 1000:1000", file.path, file.args.UID, file.args.GID)
		}
		if w.dir {
			if file.args.Type != "directory" || file.args.Mode != w.mode {
				t.Fatalf("%s = type %q mode %o, want directory mode %o", file.path, file.args.Type, file.args.Mode, w.mode)
			}
			continue
		}
		if file.args.Type != "file" || file.args.Mode != w.mode || file.content != w.content {
			t.Fatalf("%s = type %q mode %o content %q, want file mode %o content %q", file.path, file.args.Type, file.args.Mode, file.content, w.mode, w.content)
		}
	}
}

func TestTransferSymlinkedRoot(t *testing.T) {
	real := t.TempDir()
	if err := os.WriteFile(filepath.Join(real, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "content")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	server := &fakeServer{}
	server.exec = commandReplies(identityReplies)
	client := newTestClient(server, nil)

	if err := client.Transfer(context.Background(), "vm", link, "/home/ubuntu/content"); err != nil {
		t.Fatalf("Transfer: %v", err)
	}

	base := filepath.Base(link)
	if len(server.files) != 2 {
		t.Fatalf("created %d entries %v, want the symlinked directory contents", len(server.files), server.files)
	}
	root := path.Join("/home/ubuntu/content", base)
	if server.files[0].path != root || server.files[0].args.Type != "directory" {
		t.Fatalf("first transfer entry = %+v, want source root directory", server.files[0])
	}
	file := server.files[1]
	if file.path != path.Join(root, "a.txt") || file.content != "hello" {
		t.Fatalf("file entry = %+v, want a.txt with content hello", file)
	}
}

func TestDeleteMapsPurgeToForce(t *testing.T) {
	for _, purge := range []bool{true, false} {
		server := &fakeServer{}
		client := newTestClient(server, nil)
		if err := client.Delete(context.Background(), "vm", purge); err != nil {
			t.Fatalf("Delete(purge=%v): %v", purge, err)
		}
		if server.force == nil || *server.force != purge {
			t.Fatalf("DeleteInstance force = %v, want %v", server.force, purge)
		}
	}
}
