package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/daemon"
	"github.com/rave-soft/sennit/internal/daemon/supervisor"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
)

// TestSetupRunWorkspace_NoDaemon_NeverSpawns covers
// `sennit run`'s own contract (CLIENT-SERVER.md, PR 2.3): with no daemon
// running for the project, it must fall back to the in-process path --
// never start one, unlike the interactive root command's own
// options.daemon=auto path.
func TestSetupRunWorkspace_NoDaemon_NeverSpawns(t *testing.T) {
	writeGlobalConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	project := t.TempDir()

	cmd := daemonCmdTestCommand(t, project)
	cmd.Flags().Bool("yolo", false, "")
	cmd.Flags().StringSlice("channels", nil, "")
	cmd.SetContext(context.Background())

	ws, cleanup, err := setupRunWorkspace(cmd)
	require.NoError(t, err)
	defer cleanup()

	_, isClient := ws.(*grpcws.Client)
	require.False(t, isClient, "expected the in-process path when no daemon is running")

	socketPath, _, err := daemon.ResolveSocketPath(context.Background(), project, "", false)
	require.NoError(t, err)
	require.False(t, supervisor.ProbeHealthy(context.Background(), socketPath, 200*time.Millisecond),
		"a plain `sennit run` must never spawn a daemon")
}

// TestSetupRunWorkspace_DaemonRunning_GoesThroughIt covers the other
// half: with a daemon already running for the project, `sennit run` must
// use it (dial + Connect) rather than bootstrapping its own in-process
// App.
func TestSetupRunWorkspace_DaemonRunning_GoesThroughIt(t *testing.T) {
	writeGlobalConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	project := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), daemonTestTimeout)
	defer cancel()
	startTestDaemon(t, ctx, project)

	cmd := daemonCmdTestCommand(t, project)
	cmd.SetContext(ctx)

	ws, cleanup, err := setupRunWorkspace(cmd)
	require.NoError(t, err)
	defer cleanup()

	client, isClient := ws.(*grpcws.Client)
	require.True(t, isClient, "expected the daemon path when one is running")
	_, err = client.ListSessions(ctx)
	require.NoError(t, err)
}

// TestRunDetached_PrintsSessionID covers `sennit run
// --detach`: it hands the daemon the turn with AgentRun (fire-and-forget)
// and prints the session ID, which a second client can then see via
// ListSessions -- the turn itself needn't complete (this test's mock
// provider is unreachable) for the session to exist and be visible.
func TestRunDetached_PrintsSessionID(t *testing.T) {
	writeGlobalConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	project := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), daemonTestTimeout)
	defer cancel()

	// This daemon must see the mock provider writeGlobalConfig wrote --
	// runDetached checks Config().IsConfigured() -- so, unlike most of
	// this file's daemons, it must not re-isolate its own global profile
	// (see main_test.go's TestMain and loggingHelperCommand's own
	// comment on the same env var).
	t.Setenv("SENNIT_CMD_REUSE_GLOBAL_PROFILE", "1")

	// Pre-start the daemon so runDetached's own setupDaemonWorkspace call
	// (which uses the real EnsureRunning path, not a test Command hook)
	// finds it healthy and never actually needs to spawn one.
	startTestDaemon(t, ctx, project)

	cmd := daemonCmdTestCommand(t, project)
	cmd.SetContext(ctx)
	var out strings.Builder
	cmd.SetOut(&out)

	require.NoError(t, runDetached(cmd, "hello", "", "", false))

	sessID := strings.TrimSpace(out.String())
	require.NotEmpty(t, sessID)

	client, _, cleanup, err := setupAttachWorkspace(ctx, project, "", false)
	require.NoError(t, err)
	defer cleanup()

	sessions, err := client.ListSessions(ctx)
	require.NoError(t, err)
	var found bool
	for _, s := range sessions {
		if s.ID == sessID {
			found = true
		}
	}
	require.True(t, found, "expected the detached run's session to be visible to another client")
}
