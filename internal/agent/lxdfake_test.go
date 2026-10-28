package agent_test

import (
	"context"
	"io"
	"net/http"

	lxdclient "github.com/canonical/lxd/client"
	"github.com/canonical/lxd/shared/api"
	"github.com/rs/zerolog"

	"github.com/trollLemon/MPHarness/internal/lxd"
)

type fakeOperation struct {
	lxdclient.Operation
	exit int
}

func (f fakeOperation) Get() api.Operation {
	return api.Operation{Metadata: map[string]any{"return": float64(f.exit)}}
}

func (f fakeOperation) WaitContext(context.Context) error { return nil }

func (fakeOperation) Cancel() error { return nil }

type fakeServer struct {
	lxdclient.InstanceServer
}

func (fakeServer) GetInstanceFull(string) (*api.InstanceFull, string, error) {
	return nil, "", api.StatusErrorf(http.StatusNotFound, "Instance not found")
}

func (fakeServer) ExecInstance(_ string, _ api.InstanceExecPost, args *lxdclient.InstanceExecArgs) (lxdclient.Operation, error) {
	if args != nil && args.DataDone != nil {
		close(args.DataDone)
	}
	return fakeOperation{}, nil
}

type scriptedOutputServer struct {
	fakeServer
	fn func(command string) (string, int)
}

func (s scriptedOutputServer) ExecInstance(_ string, post api.InstanceExecPost, args *lxdclient.InstanceExecArgs) (lxdclient.Operation, error) {
	out, exit := "", 0
	if args != nil && len(post.Command) >= 3 {
		out, exit = s.fn(post.Command[2])
		if args.Stdout != nil && out != "" {
			_, _ = io.WriteString(args.Stdout, out)
		}
	}
	if args != nil && args.DataDone != nil {
		close(args.DataDone)
	}
	return fakeOperation{exit: exit}, nil
}

func newScriptedTestClient(_ testFatal, fn func(command string) (string, int)) *lxd.Client {
	return lxd.NewWithServer(scriptedOutputServer{fn: fn}, zerolog.Nop(), nil)
}
