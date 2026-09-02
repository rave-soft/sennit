package appws

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/configruntime"
	"github.com/stretchr/testify/require"
)

// TestSharedCredentialsManager proves that the workspace side and the
// agent side of a single App resolve the OAuth credentials manager to the
// exact same instance. SignalAuthComplete (called from AppWorkspace, on
// the workspace side) and WaitForTokenChange (called from a
// coordinator's makeAuthRefreshCallback, on the agent side) coordinate
// through channels private to one credentials.Manager — see its doc
// comment. If app.New ever constructed more than one Manager (e.g. one
// per InitCoderAgent call instead of once in New), a signal fired on the
// workspace's instance would never reach an agent-side waiter, which
// would then hang until its timeout instead of resuming. This test would
// deadlock (caught via the context timeout below) if that regressed.
//
// It does not spin up a full LLM-backed Coordinator to trigger the wait
// through a real 401 — that machinery is exercised separately (see
// agent_run_real_machinery_test.go's doc comment on why the full
// provider/runtime resolution path isn't reusable here). Instead it
// takes the exact path production code takes to obtain the manager on
// each side — App.Credentials(), the same accessor initCoderAgent uses
// to populate CoordinatorOptions.Credentials (see internal/app/app.go)
// — and drives a real SignalAuthComplete/WaitForTokenChange round trip
// through AppWorkspace's own SetProviderAPIKey, exactly as production
// wires it.
func TestSharedCredentialsManager(t *testing.T) {
	globalConfigDir := t.TempDir()
	globalDataDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalConfigDir)
	t.Setenv("SENNIT_GLOBAL_DATA", globalDataDir)
	require.NoError(t, os.WriteFile(filepath.Join(globalDataDir, "sennit.json"), []byte("{}"), 0o600))

	store, err := configruntime.Load(t.TempDir(), t.TempDir(), false)
	require.NoError(t, err)
	store.Config().Providers.Set("test-provider", config.ProviderConfig{ID: "test-provider", Name: "Test"})

	a := app.NewForTest(t.Context())
	a.SetConfigForTest(store)
	t.Cleanup(a.ShutdownForTest)

	ws := NewAppWorkspace(a, store)

	// mgr is obtained exactly the way agent.CoordinatorOptions.Credentials
	// is populated in production (app.Credentials()); using it directly
	// here is equivalent to what an agent-side waiter would block on.
	mgr := a.Credentials()
	require.NotNil(t, mgr)
	require.Same(t, mgr, a.Credentials(), "App.Credentials must return the same instance on every call")

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- mgr.WaitForTokenChange(ctx, "test-provider")
	}()

	// Give the waiter a moment to register before signalling, so this
	// also exercises the non-pre-created-channel branch of
	// SignalAuthComplete.
	time.Sleep(20 * time.Millisecond)

	// This is the production workspace-side path: AppWorkspace.
	// SetProviderAPIKey signals auth completion through app.Credentials()
	// after persisting the credential.
	require.NoError(t, ws.SetProviderAPIKey(config.ScopeGlobal, "test-provider", "new-key"))

	select {
	case err := <-errCh:
		require.NoError(t, err, "WaitForTokenChange should unblock without hitting its context deadline")
	case <-ctx.Done():
		t.Fatal("WaitForTokenChange did not unblock: workspace and agent are not sharing one credentials.Manager")
	}
}
