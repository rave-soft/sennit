package threadspawn

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/brand"

	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/pubsub"
	sessionstore "github.com/rave-soft/sennit/internal/session/store"
	"github.com/rave-soft/sennit/internal/thread"
	"github.com/stretchr/testify/require"
)

func newAttachTestApp(t *testing.T, path string) *app.App {
	t.Helper()
	t.Setenv(brand.EnvPrefix+"GLOBAL_CONFIG", t.TempDir())
	// WorkspaceLock mirrors both production callers (cmd/root.go and
	// spawner.go): work that is only safe while no second sennit is
	// running turns here - finalizing interrupted turns, above all - asks
	// whether the lock was actually enforced, so a fixture without one
	// would exercise a configuration production never has.
	boot, err := app.Bootstrap(t.Context(), path, app.BootstrapOptions{WorkspaceLock: true})
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

	Attach(t.Context(), a, repo, newAttachTestSpawner(t))
	require.NotNil(t, a.ThreadManager())

	subdir := filepath.Join(repo, "subdir")
	require.NoError(t, os.Mkdir(subdir, 0o755))
	nested := newAttachTestApp(t, subdir)
	Attach(t.Context(), nested, subdir, newAttachTestSpawner(t))
	require.Nil(t, nested.ThreadManager())
}

// TestAttachOnlyAtRepositoryRoot_SymlinkedAlias simulates, with a symlink,
// the same class of problem Windows hits with an 8.3 short name: the path
// passed to Attach and the path git reports for the same directory are
// spelled differently. `git rev-parse --show-toplevel` resolves the real
// path, so calling Attach with a symlinked alias of the repo root used to
// fail the `top != path` string compare and silently drop the thread
// manager for a directory that is, in fact, the repository root.
func TestAttachOnlyAtRepositoryRoot_SymlinkedAlias(t *testing.T) {
	repo := initRepo(t)

	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(repo, alias))

	a := newAttachTestApp(t, alias)
	Attach(t.Context(), a, alias, newAttachTestSpawner(t))
	require.NotNil(t, a.ThreadManager())
}

func TestAttachDoesNotPublishWhenConnectFails(t *testing.T) {
	repo := initRepo(t)
	a := newAttachTestApp(t, repo)
	deps := testAttachDeps()
	deps.connect = func(context.Context, string) (*sql.DB, error) { return nil, errors.New("connect") }

	AttachWithDeps(t.Context(), a, repo, newAttachTestSpawner(t), deps)

	require.Nil(t, a.ThreadManager())
}

func TestAttachRecoversBestEffortAndPublishes(t *testing.T) {
	repo := initRepo(t)
	a := newAttachTestApp(t, repo)
	deps := testAttachDeps()
	deps.recover = func(*thread.Manager, context.Context) error { return errors.New("recover") }

	AttachWithDeps(t.Context(), a, repo, newAttachTestSpawner(t), deps)

	require.NotNil(t, a.ThreadManager())
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

	AttachWithDeps(t.Context(), a, repo, newAttachTestSpawner(t), deps)

	require.Equal(t, 1, releases)
	require.Nil(t, a.ThreadManager())
}

func TestAttachCriticalCleanupFailureShutsDownThenReleasesWithoutPublishing(t *testing.T) {
	repo := initRepo(t)
	a := newAttachTestApp(t, repo)
	deps := testAttachDeps()
	var calls []string
	deps.addCriticalCleanup = func(*app.App, func(context.Context) error) error { return errors.New("cleanup") }
	deps.shutdown = func(mgr *thread.Manager, ctx context.Context) error {
		calls = append(calls, "shutdown")
		return mgr.Shutdown(ctx)
	}
	deps.release = func(dir string) error {
		calls = append(calls, "release")
		return db.Release(dir)
	}

	AttachWithDeps(t.Context(), a, repo, newAttachTestSpawner(t), deps)

	require.Equal(t, []string{"shutdown", "release"}, calls)
	require.Nil(t, a.ThreadManager())
}

func testAttachDeps() attachDeps { return ProductionAttachDeps() }

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
	deps.shutdown = func(mgr *thread.Manager, ctx context.Context) error {
		calls = append(calls, "shutdown")
		return mgr.Shutdown(ctx)
	}
	deps.release = func(dir string) error {
		calls = append(calls, "release")
		return db.Release(dir)
	}

	AttachWithDeps(t.Context(), a, repo, newAttachTestSpawner(t), deps)

	require.NotNil(t, a.ThreadManager())
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

	Attach(t.Context(), a, repo, newAttachTestSpawner(t))
	mgr := a.ThreadManager()
	require.NotNil(t, mgr)

	// Let ForwardEvents subscribe before publishing, as production event tests
	// do for post-construction sources.
	time.Sleep(10 * time.Millisecond)
	mgr.PublishForTest(thread.EventStatusChanged, thread.Thread{Delegation: thread.Delegation{ID: "thread-1", Name: "one"}})
	timeout := time.After(5 * time.Second)
waitForThreadEvent:
	for {
		select {
		case event := <-ch:
			forwarded, ok := event.Payload.(pubsub.Event[thread.Event])
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

// TestAttach_TaskManagerReachableAndSharesRecoverySweep proves the task
// manager is wired up by Attach the way NewTaskManager requires: reachable
// off the app, its Spawner wrapping the attached App itself rather than a
// new one, and sharing the thread manager's lifecycle strongly enough
// that a task and a thread created through them both get reconciled by
// one Manager.Recover call.
func TestAttach_TaskManagerReachableAndSharesRecoverySweep(t *testing.T) {
	repo := initRepo(t)
	a := newAttachTestApp(t, repo)
	// Swap in fakes the same way fakeSpawner does for a thread's own
	// isolated App, so a task's dispatch (which runs inside a itself,
	// per ParentAppSpawner) is deterministic instead of hitting a real
	// LLM/session store.
	a.SetSessionsForTest(&attachFakeSessions{})
	a.SetAgentCoordinatorForTest(&attachFakeCoordinator{})

	Attach(t.Context(), a, repo, newAttachTestSpawner(t))

	mgr := a.ThreadManager()
	require.NotNil(t, mgr, "thread manager should be reachable off the app after attach")
	tasks := a.TaskManager()
	require.NotNil(t, tasks, "task manager should be reachable off the app after attach")

	taskSt, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.NoError(t, err)

	// The task's runtime wraps the attached App itself, not a new one —
	// Manager.Handle is kind-agnostic, so it reaches a task's runtime too.
	h := mgr.Handle(taskSt.ID)
	require.NotNil(t, h)
	// A task's runtime is produced by Attach's NewParentAppSpawner, so its
	// handle is the production *parentHandle wrapping the attached App —
	// not the thread-spawner's attachTestHandle.
	adh, ok := h.(*parentHandle)
	require.True(t, ok)
	parentWorkspace, ok := adh.Workspace().(*AppWorkspaceAdapter)
	require.True(t, ok)
	require.Same(t, a, parentWorkspace.App)

	threadSt, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "sibling-thread", Goal: "go", MergePolicy: thread.MergeManual})
	require.NoError(t, err)

	// Leave both dispatched runs in flight (no RunComplete published for
	// either) and reconcile through a single recovery sweep. If
	// TaskManager had its own lifecycle instead of sharing mgr's, this
	// sweep would only ever see the thread.
	require.NoError(t, mgr.Recover(t.Context()))

	gotTask, err := mgr.Get(t.Context(), taskSt.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusInterrupted, gotTask.Status, "the task must be reachable through the same recovery sweep as the thread")

	gotThread, err := mgr.Get(t.Context(), threadSt.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusInterrupted, gotThread.Status)
}

// TestAttach_ShutdownJoinsBothKinds proves the shutdown hook Attach
// registers (mgr.Shutdown, wired via addShutdownHook) already joins a
// task's in-flight run the same way it does a thread's, without any
// teardown code added for the task manager specifically: both share
// mgr's lifecycle, so mgr.Shutdown's single controls-map walk reaches
// both.
func TestAttach_ShutdownJoinsBothKinds(t *testing.T) {
	repo := initRepo(t)
	a := newAttachTestApp(t, repo)
	a.SetSessionsForTest(&attachFakeSessions{})
	taskCoord := &attachFakeCoordinator{}
	a.SetAgentCoordinatorForTest(taskCoord)

	threadSpawner := newAttachTestSpawner(t)
	Attach(t.Context(), a, repo, threadSpawner)

	mgr := a.ThreadManager()
	require.NotNil(t, mgr)
	tasks := a.TaskManager()
	require.NotNil(t, tasks)

	taskSt, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.NoError(t, err)
	threadSt, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "sibling-thread", Goal: "go", MergePolicy: thread.MergeManual})
	require.NoError(t, err)

	require.Eventually(t, func() bool { return taskCoord.runCount() == 1 }, time.Second, time.Millisecond)
	threadCoord := threadSpawner.coordFor(threadSt.WorktreePath)
	require.Eventually(t, func() bool { return threadCoord.runCount() == 1 }, time.Second, time.Millisecond)
	// Deliberately no RunComplete published for either: both runs are
	// left in flight, the way TestTaskManager_ShutdownJoinsInFlightRun
	// and the thread-only shutdown tests in manager_test.go leave theirs.

	// Call mgr.Shutdown directly rather than a.Shutdown: this is exactly
	// the function Attach registered as the app's shutdown hook (see
	// deps.shutdown), and calling it directly — rather than through the
	// full app shutdown, which also releases the underlying DB
	// connection — leaves the store queryable afterward to assert on.
	require.NoError(t, mgr.Shutdown(t.Context()))

	gotTask, err := mgr.Get(t.Context(), taskSt.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusInterrupted, gotTask.Status)

	gotThread, err := mgr.Get(t.Context(), threadSt.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusInterrupted, gotThread.Status)
}

// TestAttach_ClosesOutInterruptedTurnsInThreadWorktrees covers the sweep
// app.Bootstrap cannot reach. A thread's sessions are recorded under its
// worktree, not under the repository the parent workspace was started in,
// so the parent's own sweep walks straight past them and a thread killed
// mid-run keeps a transcript of tool calls that never came back — which
// the UI reads as still running, forever.
func TestAttach_ClosesOutInterruptedTurnsInThreadWorktrees(t *testing.T) {
	repo := initRepo(t)
	a := newAttachTestApp(t, repo)

	conn, err := db.Connect(t.Context(), config.GlobalDBDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(config.GlobalDBDir())) })
	q := db.New(conn)

	// A thread of this project's, and a session belonging to its worktree
	// holding exactly what a killed process leaves behind: an assistant
	// turn with no Finish and a tool call with no result.
	worktree := t.TempDir()
	sessions := sessionstore.NewService(q, conn, worktree)
	sess, err := sessions.Create(t.Context(), "thread session")
	require.NoError(t, err)
	_, err = NewStore(q, a.Store().WorkingDir()).Create(t.Context(), thread.CreateParams{
		Name:         "interrupted-thread",
		Goal:         "do the thing",
		BaseBranch:   "main",
		Branch:       "thread/interrupted-thread",
		WorktreePath: worktree,
		SessionID:    sess.ID,
	})
	require.NoError(t, err)
	msg, err := a.Messages().Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{message.ToolCall{
			ID: "call-1", Name: "ripgrep", Input: "{}", Finished: false,
		}},
	})
	require.NoError(t, err)

	Attach(t.Context(), a, repo, newAttachTestSpawner(t))

	require.NoError(t, a.Messages().FlushAll(t.Context()))
	msgs, err := a.Messages().List(t.Context(), sess.ID)
	require.NoError(t, err)

	var answered bool
	var closed *message.Message
	for i, m := range msgs {
		if m.ID == msg.ID {
			closed = &msgs[i]
		}
		for _, tr := range m.ToolResults() {
			if tr.ToolCallID == "call-1" {
				answered = true
				require.True(t, tr.IsError, "an abandoned call must be recorded as an error, not an empty success")
			}
		}
	}
	require.NotNil(t, closed, "the assistant message should still be there")
	require.NotNil(t, closed.FinishPart(), "the interrupted turn must be closed out")
	require.Equal(t, message.FinishReasonCanceled, closed.FinishReason())
	require.True(t, answered, "the dangling tool call must be answered")
}

// TestAttach_SetPermissionsSkipReachesLiveThread pins the wiring
// App.SetPermissionsSkip depends on: Attach must publish the thread
// manager to the App so ThreadManager() is non-nil by the time a toggle
// happens, or the propagation App.SetPermissionsSkip performs (see its
// doc comment) silently does nothing and a running thread is left on
// whatever bypass state it was spawned with. internal/thread's own
// TestManager_SetPermissionsSkipReachesLiveThreads covers the propagation
// itself; this covers that Attach actually wires the manager it propagates
// through.
func TestAttach_SetPermissionsSkipReachesLiveThread(t *testing.T) {
	repo := initRepo(t)
	a := newAttachTestApp(t, repo)
	a.SetSessionsForTest(&attachFakeSessions{})
	a.SetAgentCoordinatorForTest(&attachFakeCoordinator{})
	spawner := newAttachTestSpawner(t)

	Attach(t.Context(), a, repo, spawner)
	mgr := a.ThreadManager()
	require.NotNil(t, mgr)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name:        "yolo-follower",
		Goal:        "implement the thing",
		MergePolicy: thread.MergeManual,
	})
	require.NoError(t, err)

	handle := spawner.handleFor(st.WorktreePath)
	require.NotNil(t, handle)
	require.False(t, handle.App().Permissions().SkipRequests(),
		"precondition: the spawned thread starts without bypass")

	a.SetPermissionsSkip(true)
	require.True(t, handle.App().Permissions().SkipRequests(),
		"turning bypass on in the parent must reach a thread already running")

	a.SetPermissionsSkip(false)
	require.False(t, handle.App().Permissions().SkipRequests(),
		"turning bypass off must reach it too")
}

// TestAttachNonGitWorkspacesGetTaskManagerWithoutThreadManager pins the
// non-git ownership split: a directory outside any repository gets the
// task manager (tasks are workspace delegations, not git worktree
// operations) but no thread manager, and the coordinator is handed a nil
// thread adapter alongside the live task adapter.
func TestAttachNonGitWorkspacesGetTaskManagerWithoutThreadManager(t *testing.T) {
	dir := t.TempDir()
	a := newAttachTestApp(t, dir)
	a.SetSessionsForTest(&attachFakeSessions{})
	coord := &attachFakeCoordinator{}
	a.SetAgentCoordinatorForTest(coord)

	Attach(t.Context(), a, dir, newAttachTestSpawner(t))

	require.Nil(t, a.ThreadManager(), "a non-git workspace must not own a thread manager")
	require.NotNil(t, a.TaskManager(), "a non-git workspace must still own a task manager")
	threadsAdapter, tasksAdapter := coord.adapters()
	require.Nil(t, threadsAdapter)
	require.NotNil(t, tasksAdapter)
}

// TestAttachForwardsTaskEventsInNonGitWorkspace pins the fix for task
// status silently failing to reach the UI outside a git repository: a
// TaskManager is built unconditionally (see
// TestAttachNonGitWorkspacesGetTaskManagerWithoutThreadManager), and it
// shares mgr's own event broker, but forwarding that broker onto
// a.Events() used to be gated on isGitWorkspace right alongside the
// thread-only wiring around it. Thread-kind events are legitimately
// git-only (a thread needs a worktree), but a task does not, so this must
// forward regardless of isGitWorkspace.
func TestAttachForwardsTaskEventsInNonGitWorkspace(t *testing.T) {
	dir := t.TempDir()
	a := newAttachTestApp(t, dir)
	ch := a.Events(t.Context())

	var mgr *thread.Manager
	deps := ProductionAttachDeps()
	deps.newManager = func(opts thread.ManagerOptions) *thread.Manager {
		mgr = thread.NewManager(opts)
		return mgr
	}
	AttachWithDeps(t.Context(), a, dir, newAttachTestSpawner(t), deps)
	require.Nil(t, a.ThreadManager(), "precondition: a non-git workspace owns no thread manager")
	require.NotNil(t, a.TaskManager())
	require.NotNil(t, mgr)

	// Let ForwardEvents subscribe before publishing, as the git-workspace
	// counterpart (TestAttachPublishesEventsAndReleasesStoreOnShutdown) does.
	time.Sleep(10 * time.Millisecond)
	mgr.PublishForTest(thread.EventStatusChanged, thread.Thread{
		Delegation: thread.Delegation{ID: "task-1", Name: "one", Kind: thread.KindTask},
	})
	timeout := time.After(5 * time.Second)
	for {
		select {
		case event := <-ch:
			forwarded, ok := event.Payload.(pubsub.Event[thread.Event])
			if !ok {
				continue
			}
			require.Equal(t, "task-1", forwarded.Payload.Thread.ID)
			return
		case <-timeout:
			t.Fatal("timed out waiting for a task event forwarded from a non-git workspace")
		}
	}
}

// TestAttachNestedGitSubdirectoryAttachesNothing covers the case easily
// confused with the one above: a subdirectory inside a repository is not
// the repository root, so Attach performs no attachment at all — no task
// manager either, unlike a directory outside git.
func TestAttachNestedGitSubdirectoryAttachesNothing(t *testing.T) {
	repo := initRepo(t)
	subdir := filepath.Join(repo, "nested")
	require.NoError(t, os.Mkdir(subdir, 0o755))

	a := newAttachTestApp(t, subdir)
	coord := &attachFakeCoordinator{}
	a.SetAgentCoordinatorForTest(coord)

	Attach(t.Context(), a, subdir, newAttachTestSpawner(t))

	require.Nil(t, a.ThreadManager())
	require.Nil(t, a.TaskManager(), "a nested directory is not an attachment point at all")
	threadsAdapter, tasksAdapter := coord.adapters()
	require.Nil(t, threadsAdapter)
	require.Nil(t, tasksAdapter)
}

// TestAttachGitRootHandsCoordinatorAdaptersFromPublishedPair proves the
// coordinator wiring: the adapters Attach hands the agent coordinator are
// the tool-facing views of the very managers it publishes on the App, so
// a thread created through the adapter is the one the App's concrete
// manager sees.
func TestAttachGitRootHandsCoordinatorAdaptersFromPublishedPair(t *testing.T) {
	repo := initRepo(t)
	a := newAttachTestApp(t, repo)
	a.SetSessionsForTest(&attachFakeSessions{})
	coord := &attachFakeCoordinator{}
	a.SetAgentCoordinatorForTest(coord)

	Attach(t.Context(), a, repo, newAttachTestSpawner(t))

	require.NotNil(t, a.ThreadManager())
	require.NotNil(t, a.TaskManager())

	threadsAdapter, tasksAdapter := coord.adapters()
	require.NotNil(t, threadsAdapter)
	require.NotNil(t, tasksAdapter)

	st, err := threadsAdapter.List(t.Context())
	require.NoError(t, err)
	got, err := a.ThreadManager().List(t.Context())
	require.NoError(t, err)
	require.Len(t, st, len(got))
	_, err = tasksAdapter.List(t.Context())
	require.NoError(t, err)
}

// SetLiveSession is inert: this fake embeds a nil coordinator, and
// the wake rule the foreground feeds is the agent's, not Attach's.
func (a *attachFakeCoordinator) SetLiveSession(string) {}

// TestAttachShutdownHookForwardsItsContext is the regression test for a
// hook that took a context and threw it away.
//
// The context a shutdown hook is handed is bounded (app.runShutdownCallback
// gives it the shutdown timeout), and thread.Manager.Shutdown counts the
// callers waiting on it: when the last one gives up, it cancels the
// teardown's own context, which is what stops the manager writing terminal
// statuses to a store the process is about to leave behind. Passing
// context.Background() instead made that wait endless — so the manager was
// never told to stop, and the goroutine running the callback was left
// behind still working while the app tore down around it.
func TestAttachShutdownHookForwardsItsContext(t *testing.T) {
	repo := initRepo(t)
	a := newAttachTestApp(t, repo)
	deps := testAttachDeps()

	var shutdownHook func(context.Context) error
	deps.addShutdownHook = func(_ *app.App, fn func(context.Context) error) error {
		shutdownHook = fn
		return nil
	}
	var got context.Context
	deps.shutdown = func(mgr *thread.Manager, ctx context.Context) error {
		got = ctx
		return mgr.Shutdown(ctx)
	}

	AttachWithDeps(t.Context(), a, repo, newAttachTestSpawner(t), deps)
	require.NotNil(t, shutdownHook)

	// A cancelled context is the giving-up signal in its most visible
	// form: the hook must hand it straight through rather than wait on
	// one of its own.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_ = shutdownHook(ctx)

	require.NotNil(t, got)
	require.ErrorIs(t, got.Err(), context.Canceled,
		"the hook must pass the context it was given, not a fresh root that never ends")
}
