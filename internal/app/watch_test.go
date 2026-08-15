package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// TestStartExternalChangeWatchers_LocalModePicksUpMCPWithoutBackend is the
// local-mode regression test for the bug this fixes: WatchForExternalChanges
// used to start only in the now-removed client/server backend, so a bare
// App built via app.New/Bootstrap — the default local path — never picked
// up a config file edited outside the process at all. This exercises
// Bootstrap+New directly, and checks that an MCP server added straight to
// the workspace's
// .braid/braid.json (as an agent's Write tool would do, bypassing
// ConfigStore.SetConfigFields) shows up in cfg.MCP and a WorkspaceChanged
// event is published, purely from app.New's own wiring.
func TestStartExternalChangeWatchers_LocalModePicksUpMCPWithoutBackend(t *testing.T) {
	setBootstrapTestEnv(t)

	cwd := t.TempDir()
	dataDir := t.TempDir()

	result, err := Bootstrap(context.Background(), cwd, BootstrapOptions{
		DataDir: dataDir,
	})
	require.NoError(t, err)
	t.Cleanup(result.App.Shutdown)

	require.NotContains(t, result.Config.Config().MCP, "added-externally")

	events := result.App.Events(context.Background())

	// Write directly to the workspace config file on disk -- the same
	// thing an agent's Write/Edit tool would do -- instead of going
	// through result.Config.SetConfigField. app.New must have started
	// the watcher itself.
	workspacePath := filepath.Join(dataDir, "braid.json")
	require.NoError(t, os.WriteFile(workspacePath,
		[]byte(`{"mcp":{"added-externally":{"command":"echo"}}}`), 0o600))

	deadline := time.After(6 * time.Second)
	for {
		select {
		case ev := <-events:
			if _, ok := ev.Payload.(pubsub.Event[WorkspaceChanged]); ok {
				goto found
			}
		case <-deadline:
			t.Fatal("timed out waiting for WorkspaceChanged event")
		}
	}
found:

	require.Contains(t, result.Config.Config().MCP, "added-externally")
	require.Eventually(t, func() bool {
		_, ok := result.App.MCP.GetState("added-externally")
		return ok
	}, 5*time.Second, 20*time.Millisecond, "expected the externally-added MCP server to reach the registry")
}
