package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/stretchr/testify/require"
)

// TestNew_ConfigBypassSkipsPermissionsAtStartup verifies that a persisted
// permissions.bypass:true in the workspace config is picked up by New,
// with no --yolo override involved, matching the seeding logic added to
// New alongside allowedTools.
func TestNew_ConfigBypassSkipsPermissionsAtStartup(t *testing.T) {
	setBootstrapTestEnv(t)

	cwd := t.TempDir()
	dataDir := t.TempDir()

	// Written before Bootstrap so config.Init/Load picks it up as the
	// workspace config file, the same path watch_test.go writes to for
	// its external-change scenario (dataDir/sennit.json, highest
	// priority, merged last in config.Load).
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "sennit.json"),
		[]byte(`{"permissions":{"bypass":true}}`), 0o600))

	result, err := Bootstrap(context.Background(), cwd, BootstrapOptions{
		DataDir: dataDir,
	})
	require.NoError(t, err)
	t.Cleanup(result.App.Shutdown)

	require.True(t, result.App.Permissions().SkipRequests())
	require.True(t, result.App.lastConfigBypass)
}

// TestApplyConfigPermissionsBypass_AppliesChange verifies that when the
// config-file bypass value changes, applyConfigPermissionsBypass pushes
// the new value onto the live permission service and updates
// lastConfigBypass to match.
func TestApplyConfigPermissionsBypass_AppliesChange(t *testing.T) {
	setBootstrapTestEnv(t)

	store, err := config.Init(t.TempDir(), t.TempDir(), false)
	require.NoError(t, err)

	app := &App{
		config:      store,
		permissions: permission.NewPermissionService("", false, nil),
	}
	require.False(t, app.Permissions().SkipRequests())

	store.Config().Permissions = &config.Permissions{Bypass: true}
	app.applyConfigPermissionsBypass()

	require.True(t, app.Permissions().SkipRequests())
	require.True(t, app.lastConfigBypass)
}

// TestApplyConfigPermissionsBypass_DoesNotClobberManualToggle verifies the
// guard this method exists for: a manual ctrl+y / /yolo toggle (calling
// SetSkipRequests directly on the live service) must survive a reload
// that fires for an unrelated reason, as long as permissions.bypass in
// config did not itself change.
func TestApplyConfigPermissionsBypass_DoesNotClobberManualToggle(t *testing.T) {
	setBootstrapTestEnv(t)

	store, err := config.Init(t.TempDir(), t.TempDir(), false)
	require.NoError(t, err)

	app := &App{
		config:      store,
		permissions: permission.NewPermissionService("", false, nil),
	}

	// Simulate a manual ctrl+y toggle: session-only, config.bypass stays false.
	app.Permissions().SetSkipRequests(true)

	// Re-invoke the reload path with the config value unchanged (still
	// false/unset) -- this must be a no-op with respect to the manual
	// toggle.
	app.applyConfigPermissionsBypass()

	require.True(t, app.Permissions().SkipRequests(), "manual toggle should survive a no-op config reload")
	require.False(t, app.lastConfigBypass)
}
