package grpcws_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/wsrpctest"
)

// TestUnreachable_UMethodReturnsErrServerUnreachable dials a bufconn
// listener whose server has already stopped, so the call never reaches
// any application code -- no trailer, no WireError, just a transport
// failure. A U/U! method (has an error result) must report that as
// errors.Is(err, workspace.ErrServerUnreachable).
func TestUnreachable_UMethodReturnsErrServerUnreachable(t *testing.T) {
	t.Parallel()

	client := deadClient(t)

	_, err := client.GetSession(context.Background(), "sess-1")
	require.Error(t, err)
	require.True(t, errors.Is(err, workspace.ErrServerUnreachable), "expected ErrServerUnreachable, got: %v", err)
}

// TestUnreachable_CMethodLogsAndReturnsZero checks a C method (no error
// result) against the same dead server: it can't report the failure
// through its own signature, so it must log instead (captured here by an
// identifier this test owns -- a message substring naming the method --
// see AGENTS.md on captureLogs being process-global) and return the zero
// value rather than panicking or blocking.
func TestUnreachable_CMethodLogsAndReturnsZero(t *testing.T) {
	client := deadClient(t)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	got := client.AgentIsBusy()
	require.False(t, got)
	require.Contains(t, buf.String(), "AgentIsBusy", "expected a log line naming the failed method")
}

// deadClient dials a bufconn listener, then immediately closes it (and the
// server), so every call this client makes fails at the transport level --
// no server ever runs, so no trailer is ever set.
func deadClient(t *testing.T) *grpcws.Client {
	t.Helper()
	srv := grpcws.NewServer(&wsrpctest.StubWorkspace{})
	dialer := startServer(t, srv)
	srv.Stop() // stop immediately: every subsequent call is transport-level unreachable
	return dialClient(t, dialer)
}
