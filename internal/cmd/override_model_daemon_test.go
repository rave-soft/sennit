package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/testenv"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/wsrpctest"
)

// TestOverrideModel_NilConfig_ReturnsErrorNotPanic guards the fix for a
// nil-pointer panic: overrideModel used to read serverConfig(ws).Providers
// directly, which is nil for any workspace that doesn't implement
// workspace.ServerConfigReader -- every *grpcws.Client (daemon mode)
// included, reachable ever since a plain `sennit run -m <model>` started
// going through a project's running daemon (1e5d36339). It must now read
// ws.Config() instead and fail cleanly when that is nil too, rather than
// dereferencing it.
func TestOverrideModel_NilConfig_ReturnsErrorNotPanic(t *testing.T) {
	t.Parallel()

	ws := &wsrpctest.StubWorkspace{} // ConfigResult defaults to nil.
	err := overrideModel(context.Background(), ws, "some-model")
	require.Error(t, err)
	require.Contains(t, err.Error(), "some-model")
}

// twoProviderGlobalConfig configures two mock providers with distinct
// models, so -m can be tested both as a bare model ID (unambiguous
// across providers) and as an explicit "provider/model" -- mirrors
// AGENTS.md's "Testing without real providers" recipe.
const twoProviderGlobalConfig = `{
  "options": {"disable_default_providers": true},
  "providers": {
    "mock-a": {"id": "mock-a", "name": "Mock A", "type": "openai",
      "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
      "models": [{"id": "model-a", "name": "Model A", "context_window": 8192}]},
    "mock-b": {"id": "mock-b", "name": "Mock B", "type": "openai",
      "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
      "models": [{"id": "model-b", "name": "Model B", "context_window": 8192}]}
  },
  "models": {"large": {"provider": "mock-a", "model": "model-a"},
             "small": {"provider": "mock-a", "model": "model-a"}}
}`

func writeTwoProviderGlobalConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", dir)
	t.Setenv("SENNIT_GLOBAL_DATA", filepath.Join(dir, "data"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sennit.json"), []byte(twoProviderGlobalConfig), 0o644))
}

// waitForModel polls client.Config().Model until it matches want or ctx
// is done. Client.Config() is a class-C cache-only getter
// (CLIENT-SERVER.md, PR 1.4a): it only reflects a config change once this
// hub's workspace.ClientState publisher rebuilds and republishes state,
// which piggybacks on real events plus a 1s ticker
// (defaultClientStateTickInterval) when nothing else triggers it sooner
// -- so overrideModel succeeding is not itself proof the client's own
// cache has caught up yet, and this closes that gap the same way
// production's own event-driven UI does (by actually subscribing and
// waiting for the state to arrive), rather than asserting on a fixed
// sleep.
func waitForModel(ctx context.Context, client *grpcws.Client, want config.SelectedModel) bool {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if cfg := client.Config(); cfg != nil && cfg.Model.Provider == want.Provider && cfg.Model.Model == want.Model {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

// TestOverrideModel_ThroughDaemon_MatchesInProcess covers `sennit run
// -m <model>` against a running daemon selecting the same model it
// selects in-process (CLIENT-SERVER.md, PR 2.3), for both a bare model
// ID and an explicit "provider/model" form -- the table of -m forms
// config.TestParseModelString already exercises against ParseModelString
// directly, driven here through the whole overrideModel/Workspace path
// instead.
func TestOverrideModel_ThroughDaemon_MatchesInProcess(t *testing.T) {
	writeTwoProviderGlobalConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", testenv.ShortRuntimeDir(t))
	t.Setenv("SENNIT_CMD_REUSE_GLOBAL_PROFILE", "1")

	daemonProject := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), daemonTestTimeout)
	defer cancel()
	startTestDaemon(t, ctx, daemonProject)

	forms := []string{"model-b", "mock-b/model-b"}
	want := config.SelectedModel{Provider: "mock-b", Model: "model-b"}

	for _, form := range forms {
		t.Run(form, func(t *testing.T) {
			client := dialSecondClient(t, ctx, daemonProject)
			// A real connection always runs its own Subscribe pump
			// (runDaemonTUI's own wiring); this test needs the same
			// thing to observe the ClientState update overrideModel's
			// config change eventually publishes -- see waitForModel.
			go client.Subscribe(func(any) {})

			require.NoError(t, overrideModel(ctx, client, form))
			require.True(t, waitForModel(ctx, client, want),
				"expected the daemon client's cached config to converge on %+v", want)

			// In-process, on a separate project so it doesn't fight the
			// daemon for the workspace lock.
			localCmd := daemonCmdTestCommand(t, t.TempDir())
			localCmd.SetContext(context.Background())
			localWS, localCleanup, err := setupLocalWorkspace(localCmd)
			require.NoError(t, err)
			defer localCleanup()

			require.NoError(t, overrideModel(ctx, localWS, form))
			require.Equal(t, want.Provider, localWS.Config().Model.Provider)
			require.Equal(t, want.Model, localWS.Config().Model.Model)
		})
	}
}
