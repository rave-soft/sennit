package appws

import (
	"os"
	"testing"

	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws/grpcwstest"
)

// wireEnvVar is the CI "wire" job's switch (CLIENT-SERVER.md, "PR 0.7"):
// when set, a handful of this package's integration-style tests -- the
// ones that drive an *AppWorkspace strictly through the workspace.Workspace
// interface, with no reach into its unexported fields -- route every call
// through wsrpc.NewLoopback's JSON codec instead of calling AppWorkspace
// directly, so a type or error that would not survive a real wire hop
// breaks here in CI instead of only once gRPC exists. "grpc" (PR 1.6)
// instead serves ws behind a real grpcws.NewServer over bufconn and hands
// back a connected *grpcws.Client.
const wireEnvVar = "SENNIT_TEST_WIRE"

// wireWorkspace wraps ws in wsrpc.NewLoopback when wireEnvVar is "1", or
// serves it behind a real gRPC server (grpcwstest.ServeGRPC) when
// wireEnvVar is "grpc", and returns ws unchanged otherwise.
func wireWorkspace(t *testing.T, ws workspace.Workspace) workspace.Workspace {
	t.Helper()
	switch os.Getenv(wireEnvVar) {
	case "1":
		return wsrpc.NewLoopback(ws)
	case "grpc":
		return grpcwstest.ServeGRPC(t, ws)
	default:
		return ws
	}
}
