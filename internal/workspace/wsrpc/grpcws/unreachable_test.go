package grpcws_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
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

// TestUnreachable_ConnectReturnsErrServerUnreachable checks that Connect
// itself -- the one call a class-C getter no longer makes -- still reports
// a dead server the same way any other U call does (CLIENT-SERVER.md, PR
// 1.4b): Connect's own Snapshot RPC is transport-level unreachable here.
func TestUnreachable_ConnectReturnsErrServerUnreachable(t *testing.T) {
	t.Parallel()

	client := deadClient(t)
	err := client.Connect(context.Background())
	require.Error(t, err)
	require.True(t, errors.Is(err, workspace.ErrServerUnreachable), "expected ErrServerUnreachable, got: %v", err)
}

// TestUnreachable_CGetterBeforeConnectLogsOnceAndReturnsZero checks a
// class-C getter called before Connect has ever succeeded: it makes no
// RPC at all (so a dead server changes nothing for it), returns the cache's
// zero value, and logs exactly once per Client no matter how many getters
// a caller asks before Connect finally succeeds -- a UI polling several
// getters per frame must not spam this log (CLIENT-SERVER.md, PR 1.4b,
// build step 3; captured here by an identifier this test owns, per
// AGENTS.md's note on captureLogs being process-global).
func TestUnreachable_CGetterBeforeConnectLogsOnceAndReturnsZero(t *testing.T) {
	client := deadClient(t)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	require.False(t, client.AgentIsBusy())
	require.Equal(t, "", client.WorkingDir())
	require.Equal(t, workspace.AgentModel{}, client.AgentModel())

	require.Equal(t, 1, strings.Count(buf.String(), "class-C getter called before Connect succeeded"),
		"expected exactly one log line, logged once per Client rather than once per call")
}

// deadClient dials a bufconn listener, then immediately closes it (and the
// server), so every call this client makes fails at the transport level --
// no server ever runs, so no trailer is ever set.
func deadClient(t *testing.T) *grpcws.Client {
	t.Helper()
	srv, stopHub := grpcws.NewServer(&wsrpctest.StubWorkspace{})
	dialer := startServer(t, srv, stopHub)
	srv.Stop() // stop immediately: every subsequent call is transport-level unreachable
	return dialClient(t, dialer)
}
