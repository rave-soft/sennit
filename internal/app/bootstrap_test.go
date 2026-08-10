package app

import (
	"context"
	"errors"
	"testing"

	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/skills"
	"github.com/stretchr/testify/require"
)

// setBootstrapTestEnv isolates config/skills discovery from the running
// user's real home and XDG directories, matching the pattern backend
// tests use for exercising the same underlying config.Init/db.Connect path.
func setBootstrapTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
}

// TestBootstrap_Success covers the happy path: config, data directory,
// DB, skills, and App all come back wired together, and the PostDataDir
// / PostConnect hooks fire in order with the config being built.
func TestBootstrap_Success(t *testing.T) {
	setBootstrapTestEnv(t)

	cwd := t.TempDir()
	dataDir := t.TempDir()

	var order []string
	result, err := Bootstrap(context.Background(), cwd, BootstrapOptions{
		DataDir: dataDir,
		Debug:   true,
		YOLO:    true,
		PostDataDir: func(cfg *config.ConfigStore) error {
			order = append(order, "post-data-dir")
			require.Equal(t, dataDir, cfg.Config().Options.DataDirectory)
			return nil
		},
		PostConnect: func(cfg *config.ConfigStore) error {
			order = append(order, "post-connect")
			return nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(result.App.Shutdown)

	require.NotNil(t, result.App)
	require.NotNil(t, result.Config)
	require.NotNil(t, result.Skills)
	require.Equal(t, []string{"post-data-dir", "post-connect"}, order)
	require.True(t, result.Config.Overrides().SkipPermissionRequests)
}

// TestBootstrap_GlobalSkillsMirror confirms the GlobalSkillsMirror option
// is actually threaded into the skills manager: with it set, discovery
// updates the package-level globals; without it, they are left alone.
func TestBootstrap_GlobalSkillsMirror(t *testing.T) {
	setBootstrapTestEnv(t)

	cwd := t.TempDir()

	// Poison the global cache first so a genuine mirror write is
	// distinguishable from state left over by other tests.
	skills.SetLatestStates(nil)

	result, err := Bootstrap(context.Background(), cwd, BootstrapOptions{
		DataDir:            t.TempDir(),
		GlobalSkillsMirror: true,
	})
	require.NoError(t, err)
	t.Cleanup(result.App.Shutdown)

	require.NotNil(t, skills.GetLatestStates(),
		"global mirror should have been written to when GlobalSkillsMirror is set")
}

// TestBootstrap_PostDataDirError verifies a failing PostDataDir hook
// aborts the sequence before the DB connection is opened.
func TestBootstrap_PostDataDirError(t *testing.T) {
	setBootstrapTestEnv(t)

	wantErr := errors.New("boom")
	_, err := Bootstrap(context.Background(), t.TempDir(), BootstrapOptions{
		DataDir: t.TempDir(),
		PostDataDir: func(cfg *config.ConfigStore) error {
			return wantErr
		},
		PostConnect: func(cfg *config.ConfigStore) error {
			t.Fatal("PostConnect must not run when PostDataDir fails")
			return nil
		},
	})
	require.ErrorIs(t, err, wantErr)
}

// TestBootstrap_DataDirLockOptionApplies is a smoke test for the
// DataDirLock option: db.Connect must accept it without error along the
// full Bootstrap sequence, matching how the backend calls Bootstrap.
func TestBootstrap_DataDirLockOptionApplies(t *testing.T) {
	setBootstrapTestEnv(t)

	result, err := Bootstrap(context.Background(), t.TempDir(), BootstrapOptions{
		DataDir:     t.TempDir(),
		DataDirLock: true,
	})
	require.NoError(t, err)
	t.Cleanup(result.App.Shutdown)
}
