package thread_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/thread"

	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// -- tests --

func TestNewManager_WorktreeDirResolution(t *testing.T) {
	repoRoot := filepath.Join(string(filepath.Separator), "home", "user", "myrepo")

	// Worktrees live inside the workspace's own data directory. That
	// directory carries a "*" .gitignore, so a checkout there is not
	// something the repository sees as a second, untracked copy of
	// itself — which is what makes keeping them in-repo workable at all.
	t.Run("empty defaults to threads inside the data directory", func(t *testing.T) {
		dataDir := filepath.Join(repoRoot, ".sennit")
		mgr := thread.NewManager(thread.ManagerOptions{RepoRoot: repoRoot, DataDir: dataDir})
		require.Equal(t, filepath.Join(dataDir, "threads"), mgr.WorktreeDirForTest())
	})

	t.Run("empty with no data directory falls back to the repo's own .sennit", func(t *testing.T) {
		mgr := thread.NewManager(thread.ManagerOptions{RepoRoot: repoRoot})
		require.Equal(t, filepath.Join(repoRoot, ".sennit", "threads"), mgr.WorktreeDirForTest())
	})

	// A relocated data directory takes the worktrees with it: they are
	// workspace state, and splitting them from the rest of it would put
	// a checkout back inside a repo that has no .gitignore covering it.
	t.Run("a relocated data directory takes the worktrees with it", func(t *testing.T) {
		dataDir := filepath.Join(string(filepath.Separator), "var", "lib", "sennit", "myrepo")
		mgr := thread.NewManager(thread.ManagerOptions{RepoRoot: repoRoot, DataDir: dataDir})
		require.Equal(t, filepath.Join(dataDir, "threads"), mgr.WorktreeDirForTest())
	})

	t.Run("relative resolves against repo root's parent", func(t *testing.T) {
		mgr := thread.NewManager(thread.ManagerOptions{RepoRoot: repoRoot, WorktreeDir: "../thread-worktrees"})
		require.Equal(t, filepath.Join(string(filepath.Separator), "home", "thread-worktrees"), mgr.WorktreeDirForTest())
	})

	t.Run("absolute is used as-is", func(t *testing.T) {
		abs := filepath.Join(string(filepath.Separator), "var", "tmp", "sennit-threads")
		mgr := thread.NewManager(thread.ManagerOptions{RepoRoot: repoRoot, WorktreeDir: abs})
		require.Equal(t, abs, mgr.WorktreeDirForTest())
	})
}

// TestManager_CreateDispatchesGoalWithAgentOrigin guards the wire/UI
// half of the origin feature: a thread's goal is dispatched as a plain
// message.User prompt (see agent.WithPromptOrigin), tagged
// message.OriginAgent so the transcript can mark it as not the person's
// own words, without changing Role or reaching the model differently.
func TestManager_CreateDispatchesGoalWithAgentOrigin(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name: "origin-goal",
		Goal: "implement the thing",
	})
	require.NoError(t, err)

	coord := spawner.coordFor(st.WorktreePath)
	require.NotNil(t, coord)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, time.Second, 5*time.Millisecond)

	require.Equal(t, message.OriginAgent, coord.runs[0].origin)
}

func TestManager_CreateHappyPath(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	events := mgr.Subscribe(t.Context())

	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name: "alpha",
		Goal: "implement the thing",
	})
	require.NoError(t, err)
	require.Equal(t, "alpha", st.Name)
	require.Equal(t, "main", st.BaseBranch)
	require.Equal(t, "thread/alpha", st.Branch)
	require.Equal(t, thread.StatusRunning, st.Status)
	require.NotEmpty(t, st.SessionID)
	require.DirExists(t, st.WorktreePath)

	branch := runGit(t, repo, "branch", "--list", "thread/alpha")
	require.Contains(t, branch, "thread/alpha")

	coord := spawner.coordFor(st.WorktreePath)
	require.NotNil(t, coord)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, time.Second, 5*time.Millisecond)

	var gotCreated, gotRunning bool
	for i := 0; i < 2; i++ {
		select {
		case ev := <-events:
			switch ev.Payload.Type {
			case thread.EventCreated:
				gotCreated = true
			case thread.EventStatusChanged:
				if ev.Payload.Thread.Status == thread.StatusRunning {
					gotRunning = true
				}
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for events")
		}
	}
	require.True(t, gotCreated)
	require.True(t, gotRunning)
}

// A thread created without a goal is an isolation-only thread: the
// worktree, branch, session, and workspace are set up, but nothing is
// dispatched. Before this, Create always called startRun, and the agent's
// empty-prompt validation pushed the thread straight to failed.
func TestManager_CreateWithoutGoalStaysIdle(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "solo"})
	require.NoError(t, err)
	require.Equal(t, thread.StatusIdle, st.Status)
	require.NotEmpty(t, st.SessionID)
	require.DirExists(t, st.WorktreePath)
	require.Contains(t, runGit(t, repo, "branch", "--list", "thread/solo"), "thread/solo")

	// The workspace stays live so attaching lands in a writable session.
	require.NotNil(t, mgr.Handle(st.ID))

	// Nothing was dispatched, and nothing arrives late either.
	coord := spawner.coordFor(st.WorktreePath)
	require.NotNil(t, coord)
	require.Never(t, func() bool { return coord.runCount() > 0 }, 200*time.Millisecond, 20*time.Millisecond)

	got, err := mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusIdle, got.Status)
	require.Empty(t, got.Error)
}

// An idle thread is a live workspace with no run in flight, so Send
// dispatches into it directly rather than respawning.
func TestManager_SendIntoIdleThread(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "solo-send"})
	require.NoError(t, err)
	require.Equal(t, thread.StatusIdle, st.Status)

	require.NoError(t, sendErr(mgr.Send(t.Context(), st.ID, "now do the thing")))

	got, err := mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusRunning, got.Status)

	coord := spawner.coordFor(st.WorktreePath)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, time.Second, 5*time.Millisecond)
	require.Equal(t, message.OriginAgent, coord.runs[0].origin,
		"a thread_send follow-up must be dispatched as agent-origin")

	// The run completes normally, which means the RunComplete watcher was
	// installed when the idle workspace was created, not only by startRun.
	writeFile(t, st.WorktreePath, "retained.txt", "keep\n")
	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, settleTimeout))
	got, err = mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusCompleted, got.Status)
}

// Activate respawns a finished thread's workspace without dispatching a
// run, so a caller can attach and work in it by hand.
func TestManager_CancelLeavesTerminalWithReasonAndKeepsWorktree(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "stoppable", Goal: "do the thing"})
	require.NoError(t, err)
	coord := spawner.coordFor(st.WorktreePath)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, time.Second, 5*time.Millisecond)
	// Deliberately no RunComplete published: the run is left in flight.

	require.NoError(t, mgr.Cancel(t.Context(), st.ID, "no longer needed"))

	got, err := mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusCancelled, got.Status)
	require.Equal(t, "no longer needed", got.Error)
	require.Nil(t, mgr.Handle(st.ID), "the runtime must be released")

	// The worktree and branch are exactly what Cancel exists to preserve
	// (Remove would have deleted both).
	require.DirExists(t, st.WorktreePath)
	require.Contains(t, runGit(t, repo, "branch", "--list", "thread/stoppable"), "thread/stoppable")

	require.False(t, coord.cancelAllWasCalled())
	require.Equal(t, []string{st.SessionID}, coord.canceledSessions(),
		"cancel must reach only the thread's own session")
}

// TestManager_CancelDefaultsReasonWhenEmpty mirrors TaskManager's
// TestTaskManager_CancelDefaultsReasonWhenEmpty: an empty reason still
// records a real one, not a blank Error field.
func TestManager_CancelDefaultsReasonWhenEmpty(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "unreasoned", Goal: "do the thing"})
	require.NoError(t, err)
	coord := spawner.coordFor(st.WorktreePath)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, time.Second, 5*time.Millisecond)

	require.NoError(t, mgr.Cancel(t.Context(), st.ID, ""))

	got, err := mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusCancelled, got.Status)
	require.Equal(t, "cancelled", got.Error)
}

func TestManager_ActivateRejectsMissingWorktree(t *testing.T) {
	repo := initRepo(t)
	store := thread.NewStoreForTest(t)
	mgr := thread.NewManager(thread.ManagerOptions{
		Store:       store,
		Spawner:     newFakeSpawner(t),
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
	})
	shutdownManagerOnCleanup(t, mgr)

	st, err := store.Create(t.Context(), thread.CreateParams{
		Name: "gone", Goal: "x", BaseBranch: "main",
		Branch: "thread/gone", WorktreePath: filepath.Join(t.TempDir(), "missing"),
	})
	require.NoError(t, err)
	_, err = store.SetStatus(t.Context(), st.ID, thread.SetStatusParams{Status: thread.StatusCompleted})
	require.NoError(t, err)

	_, err = mgr.Activate(t.Context(), st.ID)
	require.Error(t, err)
	require.Nil(t, mgr.Handle(st.ID))
}

// Idle threads are not active, so Recover leaves them alone: their
// workspace is simply not spawned until something attaches.
func TestManager_RecoverLeavesIdleThreads(t *testing.T) {
	repo := initRepo(t)
	store := thread.NewStoreForTest(t)
	mgr := thread.NewManager(thread.ManagerOptions{
		Store:       store,
		Spawner:     newFakeSpawner(t),
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
	})
	shutdownManagerOnCleanup(t, mgr)

	idle, err := store.Create(t.Context(), thread.CreateParams{
		Name: "idle-across-restart", Goal: "", BaseBranch: "main",
		Branch: "thread/idle-across-restart", WorktreePath: t.TempDir(),
	})
	require.NoError(t, err)
	_, err = store.SetStatus(t.Context(), idle.ID, thread.SetStatusParams{Status: thread.StatusIdle})
	require.NoError(t, err)

	require.NoError(t, mgr.Recover(t.Context()))

	got, err := store.Get(t.Context(), idle.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusIdle, got.Status)
}

func TestManager_CreateMarksAgentThreadSessionAsChild(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name:            "child-session",
		Goal:            "implement the thing",
		ParentSessionID: "parent-session",
	})
	require.NoError(t, err)

	sessions := spawner.appFor(st.WorktreePath).SessionsForTest().(*fakeSessions)
	sessions.mu.Lock()
	created := sessions.createdSession
	sessions.mu.Unlock()
	require.Equal(t, st.SessionID, created.ID)
	require.Equal(t, "parent-session", created.ParentSessionID)
}

func TestManager_CreateRejectsDuplicateName(t *testing.T) {
	repo := initRepo(t)
	mgr, _ := newTestManager(t, repo)

	_, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "dup", Goal: "x"})
	require.NoError(t, err)

	_, err = mgr.Create(t.Context(), thread.CreateArgs{Name: "dup", Goal: "x"})
	require.Error(t, err)
}

// racingGetByNameStore wraps a real Store and always reports "not found"
// from GetByName, simulating the window in Create's check-then-act race:
// a concurrent Create for the same name can win between this Manager's
// own GetByName check and its store.Create call, so the check alone
// cannot be trusted to catch a duplicate.
type racingGetByNameStore struct {
	thread.Store
}

func (s *racingGetByNameStore) GetByName(ctx context.Context, name string) (thread.Thread, error) {
	return thread.Thread{}, sql.ErrNoRows
}

// TestManager_CreateMapsRaceLostUniqueConstraintToFriendlyMessage is the
// regression test for the store's UNIQUE(project_path, kind, name)
// violation surfacing as raw driver text instead of the same "name ...
// is already in use" message the check-then-act GetByName guard gives.
// GetByName is stubbed to always report not-found (simulating a
// concurrent Create winning the race), so the only thing standing
// between this Create and a duplicate row is the store's own insert.
func TestManager_CreateMapsRaceLostUniqueConstraintToFriendlyMessage(t *testing.T) {
	repo := initRepo(t)
	real := thread.NewStoreForTest(t)
	_, err := real.Create(t.Context(), thread.CreateParams{
		Name: "dup", Branch: "thread/dup", WorktreePath: t.TempDir(),
	})
	require.NoError(t, err)

	mgr := thread.NewManager(thread.ManagerOptions{
		Store:       &racingGetByNameStore{Store: real},
		Spawner:     newFakeSpawner(t),
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
	})
	shutdownManagerOnCleanup(t, mgr)

	_, err = mgr.Create(t.Context(), thread.CreateArgs{Name: "dup", Goal: "x"})
	require.Error(t, err)
	require.Contains(t, err.Error(), `name "dup" is already in use`)
	require.NotContains(t, err.Error(), "UNIQUE constraint",
		"the raw driver error text must not leak past Create")
}

func TestManager_CreateRejectsInvalidName(t *testing.T) {
	repo := initRepo(t)
	mgr, _ := newTestManager(t, repo)

	_, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "Not Valid!", Goal: "x"})
	require.Error(t, err)
}

func TestManager_WaitWakesOnCompletion(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "eta", Goal: "do it"})
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		done <- mgr.Wait(context.Background(), []string{st.ID}, 5*time.Second)
	}()

	select {
	case <-done:
		t.Fatal("Wait returned before the thread reached a terminal state")
	case <-time.After(100 * time.Millisecond):
	}

	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not wake up after completion")
	}
}

func TestManager_RecoverReconcilesNonThreadKinds(t *testing.T) {
	repo := initRepo(t)
	store := thread.NewStoreForTest(t)
	mgr := thread.NewManager(thread.ManagerOptions{
		Store:       store,
		Spawner:     newFakeSpawner(t),
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
	})
	shutdownManagerOnCleanup(t, mgr)

	task, err := store.Create(t.Context(), thread.CreateParams{
		Name: "task-left-running", Goal: "x",
		Kind: thread.KindTask,
	})
	require.NoError(t, err)
	_, err = store.SetStatus(t.Context(), task.ID, thread.SetStatusParams{Status: thread.StatusRunning})
	require.NoError(t, err)

	// Confirm the setup actually exercises the gap this test guards:
	// the thread-facing listing must not surface a task-kind row.
	threads, err := store.List(t.Context())
	require.NoError(t, err)
	for _, th := range threads {
		require.NotEqual(t, task.ID, th.ID, "a task-kind row must not appear in the thread-scoped listing")
	}

	require.NoError(t, mgr.Recover(t.Context()))

	got, err := store.Get(t.Context(), task.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusInterrupted, got.Status, "recovery must reconcile every delegation kind, not just threads")
}

func TestManager_RemoveRefusesActiveWithoutForce(t *testing.T) {
	repo := initRepo(t)
	mgr, _ := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "theta", Goal: "do it"})
	require.NoError(t, err)

	err = mgr.Remove(t.Context(), st.ID, false, false)
	require.Error(t, err)
}

func TestManager_RemoveRefusesDirtyUnmergedWithoutForce(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "iota", Goal: "do it"})
	require.NoError(t, err)

	writeFile(t, st.WorktreePath, "uncommitted.txt", "x\n")
	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, settleTimeout))

	err = mgr.Remove(t.Context(), st.ID, false, false)
	require.Error(t, err)
}

func TestManager_RemoveForce(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "kappa", Goal: "do it"})
	require.NoError(t, err)
	worktreePath := st.WorktreePath

	require.NoError(t, mgr.Remove(t.Context(), st.ID, true, true))

	require.True(t, spawner.wasReleased(worktreePath))
	require.NoDirExists(t, worktreePath)

	_, err = mgr.Get(t.Context(), st.ID)
	require.Error(t, err)

	exists := runGit(t, repo, "branch", "--list", "thread/kappa")
	require.Empty(t, exists)
}

// A record whose worktree and branch someone already removed by hand is
// exactly the one a user is trying to get rid of, and the only way to get
// rid of it is Remove. It used to fail on the missing worktree and leave the
// record listed forever.
func TestManager_RemoveClearsARecordCleanedUpByHand(t *testing.T) {
	repo := initRepo(t)
	mgr, _ := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "lambda", Goal: "do it"})
	require.NoError(t, err)

	runGit(t, repo, "worktree", "remove", "--force", st.WorktreePath)
	runGit(t, repo, "branch", "-D", st.Branch)

	require.NoError(t, mgr.Remove(t.Context(), st.ID, true, true))

	_, err = mgr.Get(t.Context(), st.ID)
	require.Error(t, err)
}

func TestManager_SendRedispatches(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "lambda", Goal: "do it"})
	require.NoError(t, err)

	writeFile(t, st.WorktreePath, "output.txt", "content\n")
	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, settleTimeout))

	require.NoError(t, sendErr(mgr.Send(t.Context(), st.ID, "keep going")))

	st, err = mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusRunning, st.Status)

	coord := spawner.coordFor(st.WorktreePath)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, time.Second, 5*time.Millisecond)
}

// A message handed to a thread whose agent is mid-turn does not reach that
// agent until the turn ends, and Send has to say so: the caller (the
// thread_send tool, and through it a steering agent) decides what to do
// next from this, and "sent" would tell it the opposite of the truth. See
// SendDisposition.
func TestManager_SendReportsQueuedBehindRunningTurn(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "busy", Goal: "do it"})
	require.NoError(t, err)

	// The goal run is still in flight, with two follow-ups already waiting.
	coord := spawner.coordFor(st.WorktreePath)
	coord.setQueue(true, 2)

	disp, err := mgr.Send(t.Context(), st.ID, "wrap up, you have five minutes")
	require.NoError(t, err)
	require.True(t, disp.Queued)
	require.Equal(t, 2, disp.Ahead)
	require.False(t, disp.Resumed)

	// An idle session takes the message as its own turn, and says so.
	coord.setQueue(false, 0)
	disp, err = mgr.Send(t.Context(), st.ID, "and now this")
	require.NoError(t, err)
	require.False(t, disp.Queued)
	require.Zero(t, disp.Ahead)
}

// A thread whose workspace is no longer live is respawned by Send, and
// that is never a queued delivery: the fresh workspace has no turn of its
// own in flight for the message to wait behind.
func TestManager_HandleAndWorkspaceID(t *testing.T) {
	repo := initRepo(t)
	mgr, _ := newTestManager(t, repo)

	// Unknown thread: both accessors report "not spawned".
	require.Nil(t, mgr.Handle("no-such-id"))
	require.Empty(t, mgr.WorkspaceID("no-such-id"))

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "mu", Goal: "do it"})
	require.NoError(t, err)

	// fakeHandle.ID() is the worktree path (see fakeSpawner.Spawn).
	h := mgr.Handle(st.ID)
	require.NotNil(t, h)
	require.Equal(t, st.WorktreePath, h.ID())
	require.Equal(t, st.WorktreePath, mgr.WorkspaceID(st.ID))

	require.NoError(t, mgr.Remove(t.Context(), st.ID, true, true))
	require.Nil(t, mgr.Handle(st.ID))
	require.Empty(t, mgr.WorkspaceID(st.ID))
}

// A successful Send into an idle thread must NOT release the workspace it
// just respawned (regression: the ownership-transfer defer used to fire on
// the success path), and a follow-up queued into a live run must survive
// the in-flight run's completion: ownership moves to the follow-up's
// RunID, so only its own completion releases the workspace and settles
// the thread's status.
func TestManager_CancelledRunCompleteWinsOverError(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)
	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "cancel-error", Goal: "go"})
	require.NoError(t, err)
	coord := spawner.coordFor(st.WorktreePath)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)
	coord.mu.Lock()
	runID := coord.runs[0].runID
	coord.mu.Unlock()
	spawner.appFor(st.WorktreePath).RunCompletions().Publish(pubsub.UpdatedEvent, notify.RunComplete{SessionID: st.SessionID, RunID: runID, Error: "cancelled", Cancelled: true})
	require.Eventually(t, func() bool {
		got, err := mgr.Get(t.Context(), st.ID)
		return err == nil && got.Status == thread.StatusInterrupted
	}, eventuallyTimeout, eventuallyTick)
}

// TestManager_CreateWithNilCoordinatorCompletesInsteadOfDeadlocking guards
// against a regression where startRun's nil-coordinator branch (a workspace
// with no agent configured, e.g. Coordinator() returning nil) called
// handleRunComplete synchronously from within Create. Create holds the
// thread's opMu across the whole call, and handleRunComplete takes that
// same non-reentrant mutex itself, so the inline call deadlocked forever
// with opMu held, wedging every later operation on the thread. Without the
// fix (dispatching the completion via l.goWorker instead), this test hangs
// until its own timeout rather than failing cleanly, which is why Create is
// driven from a goroutine here.
func TestManager_CreateWithNilCoordinatorCompletesInsteadOfDeadlocking(t *testing.T) {
	repo := initRepo(t)
	spawner := newFakeSpawner(t)
	spawner.noCoordinator = true
	mgr := thread.NewManager(thread.ManagerOptions{
		Store:       thread.NewStoreForTest(t),
		Spawner:     spawner,
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
	})
	shutdownManagerOnCleanup(t, mgr)

	type createResult struct {
		st  thread.Thread
		err error
	}
	done := make(chan createResult, 1)
	go func() {
		st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "no-coordinator", Goal: "go"})
		done <- createResult{st, err}
	}()

	var res createResult
	select {
	case res = <-done:
	case <-time.After(eventuallyTimeout):
		t.Fatal("Create did not return: startRun's nil-coordinator branch deadlocked on opMu")
	}
	require.NoError(t, res.err)

	require.Eventually(t, func() bool {
		got, err := mgr.Get(t.Context(), res.st.ID)
		return err == nil && got.Status == thread.StatusFailed &&
			strings.Contains(got.Error, "workspace has no agent coordinator")
	}, eventuallyTimeout, eventuallyTick)
}

func TestManager_RunAcceptedImmediateErrorCompletesAndReleases(t *testing.T) {
	repo := initRepo(t)
	spawner := newFakeSpawner(t)
	mgr := thread.NewManager(thread.ManagerOptions{
		Store:       thread.NewStoreForTest(t),
		Spawner:     spawner,
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
	})
	// Configure the coordinator created by Spawn before Create dispatches.
	// Its RunAccepted returns this error and deliberately publishes no event.
	spawner.runErr = errors.New("boom")
	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "immediate-error", Goal: "go"})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		got, err := mgr.Get(t.Context(), st.ID)
		return err == nil && got.Status == thread.StatusFailed && strings.Contains(got.Error, "boom")
	}, eventuallyTimeout, eventuallyTick)
	require.Nil(t, mgr.Handle(st.ID))
	require.Equal(t, 1, spawner.releases(st.WorktreePath))
	require.NoError(t, mgr.Shutdown(t.Context()))
}

func TestManager_ShutdownWaitsForCancelledSpawnRollback(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)
	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "blocked", Goal: "go"})
	require.NoError(t, err)
	writeFile(t, st.WorktreePath, "retained.txt", "keep\n")
	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, time.Second))

	spawner.blockSpawn = true
	spawner.spawnEntered = make(chan struct{})
	spawner.spawnRelease = make(chan struct{})
	sendDone := make(chan error, 1)
	go func() { sendDone <- sendErr(mgr.Send(context.Background(), st.ID, "again")) }()
	<-spawner.spawnEntered
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- mgr.Shutdown(context.Background()) }()
	<-mgr.ShutdownStartedForTest()
	_, err = mgr.Create(t.Context(), thread.CreateArgs{Name: "rejected", Goal: "go"})
	require.ErrorIs(t, err, thread.ErrManagerClosed)
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned before Spawn resolved: %v", err)
	default:
	}
	close(spawner.spawnRelease)
	require.Error(t, <-sendDone)
	require.NoError(t, <-shutdownDone)
	require.Nil(t, mgr.Handle(st.ID))
	require.Equal(t, 2, spawner.releases(st.WorktreePath), "the completed original and cancelled respawn are each released once")
}

func TestManager_RemoveAndCompletionReleaseOnce(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)
	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "race-remove", Goal: "go"})
	require.NoError(t, err)
	coord := spawner.coordFor(st.WorktreePath)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)
	coord.mu.Lock()
	runID := coord.runs[0].runID
	coord.mu.Unlock()
	var wg sync.WaitGroup
	wg.Go(func() { _ = mgr.Remove(t.Context(), st.ID, true, true) })
	wg.Go(func() {
		spawner.appFor(st.WorktreePath).RunCompletions().Publish(pubsub.UpdatedEvent, notify.RunComplete{SessionID: st.SessionID, RunID: runID})
	})
	wg.Wait()
	require.Equal(t, 1, spawner.releases(st.WorktreePath))
}

// TestManager_RemoveForceCancelsThreadOwnSession guards the switch from
// CancelAll to a session-scoped Cancel at Remove's force-teardown site:
// for a thread, whose App is its own, this must still stop the thread's
// own run exactly as CancelAll used to.
func TestManager_RemoveForceCancelsThreadOwnSession(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)
	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "force-cancel", Goal: "go"})
	require.NoError(t, err)

	coord := spawner.coordFor(st.WorktreePath)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)

	require.NoError(t, mgr.Remove(t.Context(), st.ID, true, true))

	require.Equal(t, []string{st.SessionID}, coord.canceledSessions(),
		"force-removing a running thread must still cancel its own session")
}

// TestManager_ShutdownBlocksAdmission verifies that Manager.Shutdown blocks
// new operations immediately: once Shutdown starts, Create/Send/Merge/Remove
// return ErrManagerClosed even before the shutdown goroutine finishes its
// cleanup work. Uses a controlled barrier (no Sleep) to synchronize.
