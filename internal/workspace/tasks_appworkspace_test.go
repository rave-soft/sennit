package workspace

import (
	"testing"

	"github.com/rave-soft/braid/internal/app"
	"github.com/rave-soft/braid/internal/app/threadspawn"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/db"
	"github.com/rave-soft/braid/internal/thread"
	"github.com/stretchr/testify/require"
)

// newAttachedTaskTestApp bootstraps a real *app.App over repo (mirroring
// internal/thread/attach_test.go's newAttachTestApp) and wires it with
// thread.Attach so a.TaskManager() is reachable the same way it is in
// production: only thread.Attach can construct a *thread.TaskManager,
// since NewTaskManager requires the Manager's own unexported lifecycle
// and context (see thread.NewTaskManager's doc comment).
func newAttachedTaskTestApp(t *testing.T, repo string) *app.App {
	t.Helper()
	t.Setenv("BRAID_GLOBAL_CONFIG", t.TempDir())
	boot, err := app.Bootstrap(t.Context(), repo, app.BootstrapOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		boot.App.Shutdown()
		db.ResetPool()
	})
	a := boot.App
	// Deterministic session/coordinator fakes, same as attach_test.go's
	// task-manager tests, so a task's dispatch doesn't hit a real LLM.
	a.SetSessionsForTest(newFakeThreadSessions())
	a.AgentCoordinator = &fakeThreadCoordinator{}
	threadspawn.Attach(t.Context(), a, repo, newFakeThreadSpawner(t))
	return a
}

func TestAppWorkspace_SupportsTasks(t *testing.T) {
	repo := initRepoForWorkspaceThreadsTest(t)
	a := newAttachedTaskTestApp(t, repo)
	aw := NewAppWorkspace(a, config.NewTestStore(&config.Config{}, repo))
	require.True(t, aw.SupportsTasks())

	plain := app.NewForTest(t.Context())
	t.Cleanup(plain.ShutdownForTest)
	plainWS := NewAppWorkspace(plain, config.NewTestStore(&config.Config{}, t.TempDir()))
	require.False(t, plainWS.SupportsTasks())
}

func TestAppWorkspace_ListTasks(t *testing.T) {
	repo := initRepoForWorkspaceThreadsTest(t)
	a := newAttachedTaskTestApp(t, repo)
	aw := NewAppWorkspace(a, config.NewTestStore(&config.Config{}, repo))
	ctx := t.Context()

	tasks, err := aw.ListTasks(ctx)
	require.NoError(t, err)
	require.Empty(t, tasks)

	tm, ok := a.TaskManager().(*thread.TaskManager)
	require.True(t, ok)
	parent, err := a.Sessions().Create(ctx, "parent")
	require.NoError(t, err)
	created, err := tm.Create(ctx, thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: parent.ID})
	require.NoError(t, err)

	tasks, err = aw.ListTasks(ctx)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.Equal(t, created.ID, tasks[0].ID)
	require.Equal(t, "task", tasks[0].Kind)
	// A task has no workspace of its own (see TaskController's doc
	// comment): WorkspaceID must always be empty.
	require.Empty(t, tasks[0].WorkspaceID)
}

func TestAppWorkspace_CancelTask(t *testing.T) {
	repo := initRepoForWorkspaceThreadsTest(t)
	a := newAttachedTaskTestApp(t, repo)
	aw := NewAppWorkspace(a, config.NewTestStore(&config.Config{}, repo))
	ctx := t.Context()

	tm, ok := a.TaskManager().(*thread.TaskManager)
	require.True(t, ok)
	parent, err := a.Sessions().Create(ctx, "parent")
	require.NoError(t, err)
	created, err := tm.Create(ctx, thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: parent.ID})
	require.NoError(t, err)

	require.NoError(t, aw.CancelTask(ctx, created.ID, "no longer needed"))

	got, err := tm.Get(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusCancelled, got.Status)
	require.Equal(t, "no longer needed", got.Error)
}

func TestAppWorkspace_Tasks_NotSupported(t *testing.T) {
	a := app.NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)
	aw := NewAppWorkspace(a, config.NewTestStore(&config.Config{}, t.TempDir()))

	require.False(t, aw.SupportsTasks())
	_, err := aw.ListTasks(t.Context())
	require.ErrorIs(t, err, ErrTasksNotSupported)
	require.ErrorIs(t, aw.CancelTask(t.Context(), "id", ""), ErrTasksNotSupported)
}
