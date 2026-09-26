package grpcws_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/workspace/wsrpc/wsrpctest"
)

// TestRecovery_PanicReturnsInternalAndServerKeepsServing pins CLIENT-
// SERVER.md's daemon panic-isolation requirement (PR 2.1): a handler
// panic must become a codes.Internal error on the wire, not a dead
// process. wsrpctest.StubWorkspace embeds a nil workspace.Workspace, so
// any method it doesn't override -- ListSessions here -- panics on the
// nil interface call exactly like a real handler bug would.
func TestRecovery_PanicReturnsInternalAndServerKeepsServing(t *testing.T) {
	t.Parallel()

	client := newServerAndClient(t, &wsrpctest.StubWorkspace{})

	_, err := client.ListSessions(context.Background())
	require.Error(t, err, "a panicking handler must surface as an error, not crash the test process")
	require.True(t, strings.Contains(err.Error(), "Internal") || strings.Contains(err.Error(), "internal error handling"),
		"expected the recovered panic to report codes.Internal, got: %v", err)

	// The server must still be alive and serving other calls after the
	// panic -- this is the isolation property, not just "panics don't
	// crash a test binary".
	got, err := client.Hello(context.Background())
	require.NoError(t, err, "server must keep serving other RPCs after a handler panic")
	require.NotZero(t, got.ProtocolVersion)
}
