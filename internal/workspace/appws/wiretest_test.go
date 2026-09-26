package appws

import (
	"os"

	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc"
)

// wireEnvVar is the CI "wire" job's switch (CLIENT-SERVER.md, "PR 0.7"):
// when set, a handful of this package's integration-style tests -- the
// ones that drive an *AppWorkspace strictly through the workspace.Workspace
// interface, with no reach into its unexported fields -- route every call
// through wsrpc.NewLoopback's JSON codec instead of calling AppWorkspace
// directly, so a type or error that would not survive a real wire hop
// breaks here in CI instead of only once gRPC exists.
const wireEnvVar = "SENNIT_TEST_WIRE"

// wireWorkspace wraps ws in wsrpc.NewLoopback when wireEnvVar is set to
// "1", and returns ws unchanged otherwise.
func wireWorkspace(ws workspace.Workspace) workspace.Workspace {
	if os.Getenv(wireEnvVar) == "1" {
		return wsrpc.NewLoopback(ws)
	}
	return ws
}
