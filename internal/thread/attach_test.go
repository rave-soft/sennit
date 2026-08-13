package thread

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rave-soft/braid/internal/app"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/db"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/stretchr/testify/require"
)

func newAttachTestApp(t *testing.T, path string) *app.App {
	t.Helper()
	t.Setenv("BRAID_GLOBAL_CONFIG", t.TempDir())
	boot, err := app.Bootstrap(t.Context(), path, app.BootstrapOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		boot.App.Shutdown()
		db.ResetPool()
	})
	return boot.App
}

func TestAttachOnlyAtRepositoryRoot(t *testing.T) {
	repo := initRepo(t)
	a := newAttachTestApp(t, repo)

	Attach(t.Context(), a, repo, newFakeSpawner(t))
	require.IsType(t, (*Manager)(nil), a.ThreadManager())
	require.NotNil(t, a.Threads)

	subdir := filepath.Join(repo, "subdir")
	require.NoError(t, os.Mkdir(subdir, 0o755))
	nested := newAttachTestApp(t, subdir)
	Attach(t.Context(), nested, subdir, newFakeSpawner(t))
	require.Nil(t, nested.ThreadManager())
	require.Nil(t, nested.Threads)
}

func TestAttachDoesNotPublishWhenConnectFails(t *testing.T) {
	repo := initRepo(t)
	a := newAttachTestApp(t, repo)
	deps := testAttachDeps()
	deps.connect = func(context.Context, string) (*sql.DB, error) { return nil, errors.New("connect") }

	attachWithDeps(t.Context(), a, repo, newFakeSpawner(t), deps)

	require.Nil(t, a.ThreadManager())
	require.Nil(t, a.Threads)
}

func TestAttachRecoversBestEffortAndPublishes(t *testing.T) {
	repo := initRepo(t)
	a := newAttachTestApp(t, repo)
	deps := testAttachDeps()
	deps.recover = func(*Manager, context.Context) error { return errors.New("recover") }

	attachWithDeps(t.Context(), a, repo, newFakeSpawner(t), deps)

	require.IsType(t, (*Manager)(nil), a.ThreadManager())
	require.NotNil(t, a.Threads)
}

func TestAttachShutdownHookFailureReleasesDatabaseWithoutPublishing(t *testing.T) {
	repo := initRepo(t)
	a := newAttachTestApp(t, repo)
	deps := testAttachDeps()
	var releases int
	deps.addShutdownHook = func(*app.App, func(context.Context) error) error { return errors.New("hook") }
	deps.release = func(dir string) error {
		releases++
		return db.Release(dir)
	}

	attachWithDeps(t.Context(), a, repo, newFakeSpawner(t), deps)

	require.Equal(t, 1, releases)
	require.Nil(t, a.ThreadManager())
	require.Nil(t, a.Threads)
}

func TestAttachCriticalCleanupFailureShutsDownThenReleasesWithoutPublishing(t *testing.T) {
	repo := initRepo(t)
	a := newAttachTestApp(t, repo)
	deps := testAttachDeps()
	var calls []string
	deps.addCriticalCleanup = func(*app.App, func(context.Context) error) error { return errors.New("cleanup") }
	deps.shutdown = func(mgr *Manager, ctx context.Context) error {
		calls = append(calls, "shutdown")
		return mgr.Shutdown(ctx)
	}
	deps.release = func(dir string) error {
		calls = append(calls, "release")
		return db.Release(dir)
	}

	attachWithDeps(t.Context(), a, repo, newFakeSpawner(t), deps)

	require.Equal(t, []string{"shutdown", "release"}, calls)
	require.Nil(t, a.ThreadManager())
	require.Nil(t, a.Threads)
}

func testAttachDeps() attachDeps { return productionAttachDeps }

func TestAttachCleanupShutsDownManagerBeforeReleasingDatabase(t *testing.T) {
	repo := initRepo(t)
	a := newAttachTestApp(t, repo)
	deps := testAttachDeps()
	var shutdownHook, criticalCleanup func(context.Context) error
	var calls []string
	deps.addShutdownHook = func(_ *app.App, fn func(context.Context) error) error {
		shutdownHook = fn
		return nil
	}
	deps.addCriticalCleanup = func(_ *app.App, fn func(context.Context) error) error {
		criticalCleanup = fn
		return nil
	}
	deps.shutdown = func(mgr *Manager, ctx context.Context) error {
		calls = append(calls, "shutdown")
		return mgr.Shutdown(ctx)
	}
	deps.release = func(dir string) error {
		calls = append(calls, "release")
		return db.Release(dir)
	}

	attachWithDeps(t.Context(), a, repo, newFakeSpawner(t), deps)

	require.IsType(t, (*Manager)(nil), a.ThreadManager())
	require.NotNil(t, a.Threads)
	require.NotNil(t, shutdownHook)
	require.NotNil(t, criticalCleanup)
	require.NoError(t, shutdownHook(t.Context()))
	require.NoError(t, criticalCleanup(t.Context()))
	require.Equal(t, []string{"shutdown", "release"}, calls)

	conn, err := db.Connect(context.Background(), config.GlobalDBDir())
	require.NoError(t, err)
	require.NotNil(t, conn)
	require.NoError(t, db.Release(config.GlobalDBDir()))
}

func TestAttachPublishesEventsAndReleasesStoreOnShutdown(t *testing.T) {
	repo := initRepo(t)
	a := newAttachTestApp(t, repo)
	ch := a.Events(t.Context())

	Attach(t.Context(), a, repo, newFakeSpawner(t))
	mgr, ok := a.ThreadManager().(*Manager)
	require.True(t, ok)

	// Let ForwardEvents subscribe before publishing, as production event tests
	// do for post-construction sources.
	time.Sleep(10 * time.Millisecond)
	mgr.publish(EventStatusChanged, Thread{ID: "thread-1", Name: "one"})
	timeout := time.After(5 * time.Second)
waitForThreadEvent:
	for {
		select {
		case event := <-ch:
			forwarded, ok := event.Payload.(pubsub.Event[Event])
			if !ok {
				continue
			}
			require.Equal(t, "thread-1", forwarded.Payload.Thread.ID)
			break waitForThreadEvent
		case <-timeout:
			t.Fatal("timed out waiting for forwarded thread event")
		}
	}

	a.Shutdown()
	// Shutdown has run the manager hook before the critical DB cleanup. A new
	// pooled connection proves the attached reference was released rather than
	// leaking after its owner stopped.
	conn, err := db.Connect(context.Background(), config.GlobalDBDir())
	require.NoError(t, err)
	require.NotNil(t, conn)
	require.NoError(t, db.Release(config.GlobalDBDir()))
}
