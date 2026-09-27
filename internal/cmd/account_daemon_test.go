package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
)

// TestSetupAccountWorkspace_NoDaemon_NeverSpawns covers login/logout/
// accounts' own contract (CLIENT-SERVER.md, PR 2.3): with no daemon
// running for the project, setupAccountWorkspace must fall back to the
// in-process path -- never start one, exactly like `sennit run`'s own
// setupRunWorkspace.
func TestSetupAccountWorkspace_NoDaemon_NeverSpawns(t *testing.T) {
	writeGlobalConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	project := t.TempDir()

	cmd := daemonCmdTestCommand(t, project)
	cmd.Flags().Bool("yolo", false, "")
	cmd.Flags().StringSlice("channels", nil, "")
	cmd.SetContext(context.Background())

	ws, cleanup, err := setupAccountWorkspace(cmd)
	require.NoError(t, err)
	defer cleanup()

	_, isClient := ws.(*grpcws.Client)
	require.False(t, isClient, "expected the in-process path when no daemon is running")
}

// TestSetupAccountWorkspace_DaemonRunning_GoesThroughIt is the other
// half: with a daemon already running for the project,
// login/logout/accounts must connect to it (dial + Connect) rather than
// bootstrapping their own in-process App, which would otherwise fail
// taking the project's workspace lock (the bug this PR fixes -- the
// daemon already holds it).
func TestSetupAccountWorkspace_DaemonRunning_GoesThroughIt(t *testing.T) {
	writeGlobalConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	project := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), daemonTestTimeout)
	defer cancel()
	startTestDaemon(t, ctx, project)

	cmd := daemonCmdTestCommand(t, project)
	cmd.SetContext(ctx)

	ws, cleanup, err := setupAccountWorkspace(cmd)
	require.NoError(t, err)
	defer cleanup()

	client, isClient := ws.(*grpcws.Client)
	require.True(t, isClient, "expected the daemon path when one is running")
	_, err = client.ListAccounts("mock")
	require.NoError(t, err)
}

// dialSecondClient connects a fresh *grpcws.Client to the daemon already
// running for project, so a test can verify a write one client made is
// visible to a second, independent connection -- proving the write went
// through the daemon's own state rather than something local to the
// first client.
func dialSecondClient(t *testing.T, ctx context.Context, project string) *grpcws.Client {
	t.Helper()
	client, _, cleanup, err := setupAttachWorkspace(ctx, project, "", false)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	return client
}

// TestAccountsAddListRemove_ThroughDaemon covers `sennit accounts add/
// list/remove` operating through a running daemon (CLIENT-SERVER.md, PR
// 2.3): authAddAPIKey, authListAll and runAuthRemove -- the RunE bodies
// accountsAddCmd/accountsListCmd/accountsRemoveCmd delegate to -- driven
// against a *grpcws.Client from one connection, observed from a second,
// independent one.
func TestAccountsAddListRemove_ThroughDaemon(t *testing.T) {
	writeGlobalConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	// The daemon this test spawns must see the "mock" provider
	// writeGlobalConfig wrote, not re-isolate its own global profile --
	// see main_test.go's TestMain and TestRunDetached_PrintsSessionID's
	// own comment on the same env var.
	t.Setenv("SENNIT_CMD_REUSE_GLOBAL_PROFILE", "1")
	project := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), daemonTestTimeout)
	defer cancel()
	startTestDaemon(t, ctx, project)

	writer := dialSecondClient(t, ctx, project)
	require.NoError(t, authAddAPIKey(writer, "mock", "key-one"))

	reader := dialSecondClient(t, ctx, project)
	accts, err := reader.ListAccounts("mock")
	require.NoError(t, err)
	// The mock provider's own api_key entry (mockGlobalConfig) already
	// seeds one account; authAddAPIKey's ForceNewAccount added a second,
	// distinct one -- find it by the label authAddAPIKey/RecordAccount
	// give a fresh api-key account ("Account", the provider's name).
	var added *workspace.FrontendAccount
	for i := range accts {
		if accts[i].Label == "Account" {
			added = &accts[i]
		}
	}
	require.NotNil(t, added, "expected authAddAPIKey's account to be visible from a second client")

	// authListAll (accountsListCmd with no args) must see it too, reading
	// through ws.Config() rather than the in-process-only serverConfig.
	require.NoError(t, authListAll(reader))

	require.NoError(t, runAuthRemove(writer, "mock", added.ID))

	after, err := reader.ListAccounts("mock")
	require.NoError(t, err)
	for _, a := range after {
		require.NotEqual(t, added.ID, a.ID, "removal through one client must be visible from another")
	}
}

// TestLogoutProvider_ThroughDaemon covers `sennit logout` operating
// through a running daemon: an account recorded on one client is
// removed by logoutProvider driven on a second, independent connection.
func TestLogoutProvider_ThroughDaemon(t *testing.T) {
	writeGlobalConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("SENNIT_CMD_REUSE_GLOBAL_PROFILE", "1")
	project := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), daemonTestTimeout)
	defer cancel()
	startTestDaemon(t, ctx, project)

	writer := dialSecondClient(t, ctx, project)
	require.NoError(t, authAddAPIKey(writer, "mock", "key-one"))

	logoutClient := dialSecondClient(t, ctx, project)
	require.NoError(t, logoutProvider(logoutClient, "mock", "Mock"))

	reader := dialSecondClient(t, ctx, project)
	accts, err := reader.ListAccounts("mock")
	require.NoError(t, err)
	require.Empty(t, accts)
}

// TestPickLoggedInProvider_ThroughDaemon covers logout.go's
// pickLoggedInProvider reading ws.Config() (a *workspace.FrontendConfig,
// available over the wire) instead of the in-process-only serverConfig,
// against a real daemon connection reporting no OAuth providers logged
// in -- the mock provider this package's daemon tests use is an api-key
// provider, so Provider(...).Auth.HasOAuth is false for it and
// pickLoggedInProvider correctly reports nothing logged in rather than
// panicking or misreading a nil config.
func TestPickLoggedInProvider_ThroughDaemon(t *testing.T) {
	writeGlobalConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	project := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), daemonTestTimeout)
	defer cancel()
	startTestDaemon(t, ctx, project)

	client := dialSecondClient(t, ctx, project)
	require.Empty(t, pickLoggedInProvider(client))
}
