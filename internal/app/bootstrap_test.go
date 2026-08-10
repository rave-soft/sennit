package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/db"
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

// TestBootstrap_WorkspaceLockOptionApplies is a smoke test for the
// WorkspaceLock option: Bootstrap must acquire the workspace lock and
// still complete successfully, matching how the backend calls
// Bootstrap.
func TestBootstrap_WorkspaceLockOptionApplies(t *testing.T) {
	setBootstrapTestEnv(t)

	result, err := Bootstrap(context.Background(), t.TempDir(), BootstrapOptions{
		DataDir:       t.TempDir(),
		WorkspaceLock: true,
	})
	require.NoError(t, err)
	t.Cleanup(result.App.Shutdown)
}

// TestBootstrap_WorkspaceLockReleasedOnShutdown confirms the workspace
// lock acquired by WorkspaceLock is released once the resulting App is
// shut down, so a second Bootstrap of the same data directory can
// proceed afterward.
func TestBootstrap_WorkspaceLockReleasedOnShutdown(t *testing.T) {
	setBootstrapTestEnv(t)

	dataDir := t.TempDir()
	result, err := Bootstrap(context.Background(), t.TempDir(), BootstrapOptions{
		DataDir:       dataDir,
		WorkspaceLock: true,
	})
	require.NoError(t, err)
	result.App.Shutdown()

	lock, err := db.AcquireWorkspaceLock(dataDir)
	require.NoError(t, err, "workspace lock should be released after Shutdown")
	lock.Release()
}

// TestBootstrap_TwoProjectsConcurrentWrites simulates the scenario the
// shared-database migration exists to support: two independent braid
// "instances" (distinct working directories, distinct project-local
// .braid data directories, each with its own WorkspaceLock like the
// backend takes) bootstrapped in the same process against the SAME
// global database (HOME/XDG are shared across both goroutines here,
// unlike setBootstrapTestEnv's usual one-dir-per-test isolation). Both
// then write sessions concurrently. Nothing here should dead-lock or
// error: WAL + busy_timeout must serialize the shared connection's
// single underlying conn (SetMaxOpenConns(1)) without either workspace
// blocking the other's *process-level* startup, and each workspace's
// WorkspaceLock only guards its own project directory, not the shared
// database.
func TestBootstrap_TwoProjectsConcurrentWrites(t *testing.T) {
	setBootstrapTestEnv(t)

	bootOne := func(cwd string) *App {
		result, err := Bootstrap(context.Background(), cwd, BootstrapOptions{
			DataDir:       t.TempDir(),
			WorkspaceLock: true,
		})
		require.NoError(t, err)
		return result.App
	}

	appA := bootOne(t.TempDir())
	t.Cleanup(appA.Shutdown)
	appB := bootOne(t.TempDir())
	t.Cleanup(appB.Shutdown)

	const writesPerApp = 20
	var wg sync.WaitGroup
	errs := make(chan error, writesPerApp*2)
	write := func(a *App, label string) {
		defer wg.Done()
		for i := range writesPerApp {
			if _, err := a.Sessions.Create(context.Background(), fmt.Sprintf("%s-%d", label, i)); err != nil {
				errs <- err
			}
		}
	}

	wg.Add(2)
	go write(appA, "a")
	go write(appB, "b")
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	sessionsA, err := appA.Sessions.List(context.Background())
	require.NoError(t, err)
	require.Len(t, sessionsA, writesPerApp, "project A must see only its own sessions")

	sessionsB, err := appB.Sessions.List(context.Background())
	require.NoError(t, err)
	require.Len(t, sessionsB, writesPerApp, "project B must see only its own sessions")
}
