package agent

import (
	"context"
	"net/http"
	"testing"

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

func newTestClient(t *testing.T) *lxd.Client {
	t.Helper()
	return lxd.NewWithServer(fakeServer{}, zerolog.Nop(), nil)
}
