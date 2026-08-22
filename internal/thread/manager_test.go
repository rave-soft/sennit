package thread_test

import (
	"context"
	"errors"
	"os"
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
		Name:        "origin-goal",
		Goal:        "implement the thing",
		MergePolicy: thread.MergeManual,
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
		Name:        "alpha",
		Goal:        "implement the thing",
		MergePolicy: thread.MergeManual,
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

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "solo", MergePolicy: thread.MergeManual})
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

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "solo-send", MergePolicy: thread.MergeManual})
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
	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, settleTimeout))
	got, err = mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusCompleted, got.Status)
}

// Activate respawns a finished thread's workspace without dispatching a
// run, so a caller can attach and work in it by hand.
func TestManager_ActivateFinishedThread(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "revive", Goal: "do it", MergePolicy: thread.MergeManual})
	require.NoError(t, err)
	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, settleTimeout))
	require.Nil(t, mgr.Handle(st.ID), "a completed thread releases its workspace")

	completed, err := mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, "finished", completed.ResultSummary)

	activated, err := mgr.Activate(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusIdle, activated.Status)
	require.Equal(t, "finished", activated.ResultSummary, "the earlier run's result must survive")
	require.NotNil(t, mgr.Handle(st.ID))

	// Activation dispatches nothing on its own.
	coord := spawner.coordFor(st.WorktreePath)
	require.Never(t, func() bool { return coord.runCount() > 0 }, 200*time.Millisecond, 20*time.Millisecond)

	// Activating a live thread is a no-op that reports its current state.
	again, err := mgr.Activate(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusIdle, again.Status)
}

// A merge in flight, and a merge already landed, are the two states
// there is nothing to reactivate into: the first holds the thread's opMu
// for its whole duration, and the second has already been folded into
// its base and is on its way to being discarded, worktree included.
func TestManager_ActivateRefusesMergeInFlightAndMerged(t *testing.T) {
	repo := initRepo(t)
	store := thread.NewStoreForTest(t)
	mgr := thread.NewManager(thread.ManagerOptions{
		Store:       store,
		Spawner:     newFakeSpawner(t),
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
	})
	shutdownManagerOnCleanup(t, mgr)

	for _, status := range []thread.Status{thread.StatusMerging, thread.StatusMerged} {
		t.Run(string(status), func(t *testing.T) {
			st, err := store.Create(t.Context(), thread.CreateParams{
				Name: "merge-" + string(status), Goal: "x", BaseBranch: "main",
				Branch: "thread/merge-" + string(status), WorktreePath: t.TempDir(),
			})
			require.NoError(t, err)
			_, err = store.SetStatus(t.Context(), st.ID, thread.SetStatusParams{Status: status})
			require.NoError(t, err)

			_, err = mgr.Activate(t.Context(), st.ID)
			require.Error(t, err)
			require.Nil(t, mgr.Handle(st.ID), "a refused activation must not spawn a workspace")

			got, err := store.Get(t.Context(), st.ID)
			require.NoError(t, err)
			require.Equal(t, status, got.Status, "a refused activation must not change status")
		})
	}
}

// A thread whose merge stopped on a conflict (or was blocked outright) is
// exactly the one a person needs to open and work in: the half-finished
// merge is still in its worktree, and resolving it by hand and merging
// again is the recovery path mergeAttempt documents for itself. Refusing
// it left that path reachable through Send but not through the TUI, which
// attaches and so fell back to a read-only workspace.
//
// The status survives the activation. It describes the state of the merge
// in the worktree, which attaching does not change — resetting it to idle
// would drop the thread out of the dashboard's failed filter merely
// because somebody looked at it.
func TestManager_ActivateRestingMergeFlowKeepsStatus(t *testing.T) {
	repo := initRepo(t)
	store := thread.NewStoreForTest(t)
	mgr := thread.NewManager(thread.ManagerOptions{
		Store:       store,
		Spawner:     newFakeSpawner(t),
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
	})
	shutdownManagerOnCleanup(t, mgr)

	for _, status := range []thread.Status{thread.StatusConflict, thread.StatusMergeBlocked} {
		t.Run(string(status), func(t *testing.T) {
			st, err := store.Create(t.Context(), thread.CreateParams{
				Name: "rest-" + string(status), Goal: "x", BaseBranch: "main",
				Branch: "thread/rest-" + string(status), WorktreePath: t.TempDir(),
			})
			require.NoError(t, err)
			_, err = store.SetStatus(t.Context(), st.ID, thread.SetStatusParams{
				Status: status, Error: "merge conflicts: a.go", ResultSummary: "did the work",
			})
			require.NoError(t, err)

			activated, err := mgr.Activate(t.Context(), st.ID)
			require.NoError(t, err)
			require.NotNil(t, mgr.Handle(st.ID), "the workspace must be live so the person can work in it")
			require.Equal(t, status, activated.Status, "attaching to a thread must not change what it is")
			require.Equal(t, "merge conflicts: a.go", activated.Error, "the reason the merge stopped must survive")
			require.Equal(t, "did the work", activated.ResultSummary, "the earlier run's result must survive")

			got, err := store.Get(t.Context(), st.ID)
			require.NoError(t, err)
			require.Equal(t, status, got.Status)
		})
	}
}

// TestManager_CancelLeavesTerminalWithReasonAndKeepsWorktree is Cancel's
// core contract: a running thread stops and rests at StatusCancelled with
// reason recorded as its Error — a real status transition, not merely a
// released runtime — while its worktree and branch stay on disk, unlike
// Remove. Mirrors TaskManager's own
// TestTaskManager_CancelLeavesTerminalWithReasonAndParentUntouched.
func TestManager_CancelLeavesTerminalWithReasonAndKeepsWorktree(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "stoppable", Goal: "do the thing", MergePolicy: thread.MergeManual})
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

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "unreasoned", Goal: "do the thing", MergePolicy: thread.MergeManual})
	require.NoError(t, err)
	coord := spawner.coordFor(st.WorktreePath)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, time.Second, 5*time.Millisecond)

	require.NoError(t, mgr.Cancel(t.Context(), st.ID, ""))

	got, err := mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusCancelled, got.Status)
	require.Equal(t, "cancelled", got.Error)
}

// TestManager_CancelRefusesMergeFlow proves the decision this step made
// explicit: cancelling a thread already in the merge flow is refused for
// all four statuses — by the time a thread has reached one of them there
// is either nothing left running to stop, or a merge actively folding a
// branch back that is not a step to interrupt partway.
//
// Cancel stays wider than Activate here, which reactivates the two
// resting statuses (TestManager_ActivateRestingMergeFlowKeepsStatus). The
// two answer different questions: Activate asks "may a person work in
// this worktree", and for a stopped merge the answer is yes; Cancel asks
// "is there a run in flight to stop", and for all four the answer is no.
func TestManager_CancelRefusesMergeFlow(t *testing.T) {
	repo := initRepo(t)
	store := thread.NewStoreForTest(t)
	mgr := thread.NewManager(thread.ManagerOptions{
		Store:       store,
		Spawner:     newFakeSpawner(t),
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
	})
	shutdownManagerOnCleanup(t, mgr)

	for _, status := range []thread.Status{thread.StatusMerging, thread.StatusMerged, thread.StatusConflict, thread.StatusMergeBlocked} {
		t.Run(string(status), func(t *testing.T) {
			st, err := store.Create(t.Context(), thread.CreateParams{
				Name: "cancel-" + string(status), Goal: "x", BaseBranch: "main",
				Branch: "thread/cancel-" + string(status), WorktreePath: t.TempDir(),
			})
			require.NoError(t, err)
			_, err = store.SetStatus(t.Context(), st.ID, thread.SetStatusParams{Status: status})
			require.NoError(t, err)

			err = mgr.Cancel(t.Context(), st.ID, "stop")
			require.Error(t, err)

			got, err := store.Get(t.Context(), st.ID)
			require.NoError(t, err)
			require.Equal(t, status, got.Status, "a refused cancel must not change status")
		})
	}
}

// A vanished worktree cannot be reopened, and the failure must not leave
// a half-installed runtime behind.
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
		MergePolicy:     thread.MergeManual,
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

	_, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "dup", Goal: "x", MergePolicy: thread.MergeManual})
	require.NoError(t, err)

	_, err = mgr.Create(t.Context(), thread.CreateArgs{Name: "dup", Goal: "x", MergePolicy: thread.MergeManual})
	require.Error(t, err)
}

func TestManager_CreateRejectsInvalidName(t *testing.T) {
	repo := initRepo(t)
	mgr, _ := newTestManager(t, repo)

	_, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "Not Valid!", Goal: "x"})
	require.Error(t, err)
}

func TestManager_ManualPolicyCompleted(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "beta", Goal: "do it", MergePolicy: thread.MergeManual})
	require.NoError(t, err)

	writeFile(t, st.WorktreePath, "output.txt", "did the work\n")
	coord := spawner.coordFor(st.WorktreePath)
	require.Eventually(t, func() bool { return coord.runCount() >= 1 }, time.Second, time.Millisecond)
	coord.mu.Lock()
	runID := coord.runs[0].runID
	coord.mu.Unlock()
	spawner.appFor(st.WorktreePath).RunCompletions().Publish(pubsub.UpdatedEvent, notify.RunComplete{SessionID: st.SessionID, RunID: runID, Text: "finished"})

	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, settleTimeout))

	st, err = mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusCompleted, st.Status)
	require.Equal(t, "finished", st.ResultSummary)
	require.NotZero(t, st.CompletedAt)
	require.NoFileExists(t, filepath.Join(repo, "output.txt"))

	_, mergeErr := mgr.Merge(t.Context(), st.ID)
	require.NoError(t, mergeErr)

	require.FileExists(t, filepath.Join(repo, "output.txt"))
	requireDiscarded(t, mgr, repo, st)
}

func TestManager_RunCompleteSuccessAutoMerge(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	// Default MergePolicy is auto.
	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "gamma", Goal: "do it"})
	require.NoError(t, err)
	require.Equal(t, thread.MergeAuto, st.MergePolicy)

	writeFile(t, st.WorktreePath, "output.txt", "auto merged\n")
	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)

	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, settleTimeout))

	require.FileExists(t, filepath.Join(repo, "output.txt"))
	require.Equal(t, "auto merged\n", runGit(t, repo, "show", "main:output.txt"),
		"the thread's work must be on the base branch")
	requireDiscardedEventually(t, mgr, repo, st)
}

func TestManager_ConflictAndRetryAfterResolution(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "delta", Goal: "do it", MergePolicy: thread.MergeManual})
	require.NoError(t, err)

	// The base branch (main, checked out in repo itself) and the thread
	// branch each edit README.md, guaranteeing a conflict on merge.
	writeFile(t, repo, "README.md", "main version\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "edit on main")

	writeFile(t, st.WorktreePath, "README.md", "thread version\n")
	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, settleTimeout))

	_, mergeErr := mgr.Merge(t.Context(), st.ID)
	require.NoError(t, mergeErr)
	st, err = mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusConflict, st.Status)
	require.Contains(t, st.Error, "README.md")

	// Resolve: write the merged content and stage it, but don't commit —
	// Merge's own CommitAll step finishes the merge commit.
	writeFile(t, st.WorktreePath, "README.md", "resolved version\n")
	runGit(t, st.WorktreePath, "add", "README.md")

	merged, mergeErr := mgr.Merge(t.Context(), st.ID)
	require.NoError(t, mergeErr)
	require.Equal(t, thread.StatusMerged, merged.Status,
		"Merge must report the outcome itself; the row is gone by the time it returns")
	requireDiscarded(t, mgr, repo, st)

	content, err := os.ReadFile(filepath.Join(repo, "README.md"))
	require.NoError(t, err)
	// git, not sennit, owns line-ending translation on checkout: with
	// core.autocrlf=true — the default on a lot of Windows git installs —
	// the working-tree copy comes back CRLF even though writeFile wrote
	// LF and sennit never touches the content in between. Normalize
	// before comparing; what this test cares about is that the resolved
	// content made it through the merge, not which EOL convention the
	// local git config happens to apply on checkout.
	require.Equal(t, "resolved version\n", strings.ReplaceAll(string(content), "\r\n", "\n"))
}

func TestManager_MergeBlockedWhenBaseCheckedOutAndDirty(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	// main is checked out in repo (the primary worktree) throughout, so
	// the fast-forward step must fall back to the ff-only merge path —
	// which this test then blocks by leaving repo dirty.
	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "epsilon", Goal: "do it", MergePolicy: thread.MergeManual})
	require.NoError(t, err)

	writeFile(t, st.WorktreePath, "output.txt", "content\n")
	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, settleTimeout))

	writeFile(t, repo, "dirty.txt", "uncommitted\n")

	_, mergeErr := mgr.Merge(t.Context(), st.ID)
	require.NoError(t, mergeErr)
	st, err = mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusMergeBlocked, st.Status)
	require.NotEmpty(t, st.Error)
}

func TestManager_FastForwardWhenBaseNotCheckedOut(t *testing.T) {
	repo := initRepo(t)
	// A base branch that is never checked out anywhere, so the
	// push-based FastForward succeeds directly without falling back to
	// the ff-only merge path.
	runGit(t, repo, "branch", "other-base")

	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name:        "zeta",
		Goal:        "do it",
		BaseBranch:  "other-base",
		MergePolicy: thread.MergeManual,
	})
	require.NoError(t, err)
	require.Equal(t, "other-base", st.BaseBranch)

	writeFile(t, st.WorktreePath, "output.txt", "content\n")
	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, settleTimeout))

	_, mergeErr := mgr.Merge(t.Context(), st.ID)
	require.NoError(t, mergeErr)

	require.Equal(t, "content\n", runGit(t, repo, "show", "other-base:output.txt"),
		"the thread's work must be on its own base branch, not on main")
	requireDiscarded(t, mgr, repo, st)
}

func TestManager_WaitWakesOnCompletion(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "eta", Goal: "do it", MergePolicy: thread.MergeManual})
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

func TestManager_Recover(t *testing.T) {
	repo := initRepo(t)
	store := thread.NewStoreForTest(t)
	mgr := thread.NewManager(thread.ManagerOptions{
		Store:       store,
		Spawner:     newFakeSpawner(t),
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
	})
	shutdownManagerOnCleanup(t, mgr)

	running, err := store.Create(t.Context(), thread.CreateParams{
		Name: "running-gone", Goal: "x", BaseBranch: "main",
		Branch: "thread/running-gone", WorktreePath: filepath.Join(t.TempDir(), "missing"),
	})
	require.NoError(t, err)
	_, err = store.SetStatus(t.Context(), running.ID, thread.SetStatusParams{Status: thread.StatusRunning})
	require.NoError(t, err)

	presentWorktree := t.TempDir()
	stillRunning, err := store.Create(t.Context(), thread.CreateParams{
		Name: "running-present", Goal: "x", BaseBranch: "main",
		Branch: "thread/running-present", WorktreePath: presentWorktree,
	})
	require.NoError(t, err)
	_, err = store.SetStatus(t.Context(), stillRunning.ID, thread.SetStatusParams{Status: thread.StatusRunning})
	require.NoError(t, err)

	merged, err := store.Create(t.Context(), thread.CreateParams{
		Name: "already-merged", Goal: "x", BaseBranch: "main",
		Branch: "thread/already-merged", WorktreePath: filepath.Join(t.TempDir(), "also-missing"),
	})
	require.NoError(t, err)
	_, err = store.SetStatus(t.Context(), merged.ID, thread.SetStatusParams{Status: thread.StatusMerged, CompletedAt: 1})
	require.NoError(t, err)

	require.NoError(t, mgr.Recover(t.Context()))

	got, err := store.Get(t.Context(), running.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusFailed, got.Status)

	got, err = store.Get(t.Context(), stillRunning.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusInterrupted, got.Status)

	got, err = store.Get(t.Context(), merged.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusMerged, got.Status)
}

// TestManager_RecoverReconcilesNonThreadKinds guards the generic recovery
// sweep against silently degrading into "threads only" at either layer it
// could regress at: the query (Store.List is scoped to kind = 'thread',
// so Recover must source its sweep from ListAll, not List) and the
// git-overlay hook (recoverWorktree must decline anything that is not a
// Kind: KindThread, or a worktree-less row reads as "worktree missing"
// and gets marked failed before the generic sweep ever sees it). The
// seeded row's WorktreePath is deliberately empty, matching what a real
// task row will have — a non-empty path would silently avoid the
// hook-level half of this bug. If either layer regresses, a non-thread
// delegation left running when the process died would never be
// reconciled to interrupted — it would sit displayed as running forever
// with no goroutine behind it. Nothing creates task-kind rows yet, so
// this seeds one directly through the store, the same seam a real task
// store will use.
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

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "theta", Goal: "do it", MergePolicy: thread.MergeManual})
	require.NoError(t, err)

	err = mgr.Remove(t.Context(), st.ID, false, false)
	require.Error(t, err)
}

func TestManager_RemoveRefusesDirtyUnmergedWithoutForce(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "iota", Goal: "do it", MergePolicy: thread.MergeManual})
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

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "kappa", Goal: "do it", MergePolicy: thread.MergeManual})
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

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "lambda", Goal: "do it", MergePolicy: thread.MergeManual})
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

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "lambda", Goal: "do it", MergePolicy: thread.MergeManual})
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

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "busy", Goal: "do it", MergePolicy: thread.MergeManual})
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
func TestManager_SendReportsResumeAsImmediate(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "resumed", Goal: "do it", MergePolicy: thread.MergeManual})
	require.NoError(t, err)
	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, settleTimeout))
	require.Nil(t, mgr.Handle(st.ID))

	disp, err := mgr.Send(t.Context(), st.ID, "one more thing")
	require.NoError(t, err)
	require.True(t, disp.Resumed)
	require.False(t, disp.Queued)
}

func TestManager_HandleAndWorkspaceID(t *testing.T) {
	repo := initRepo(t)
	mgr, _ := newTestManager(t, repo)

	// Unknown thread: both accessors report "not spawned".
	require.Nil(t, mgr.Handle("no-such-id"))
	require.Empty(t, mgr.WorkspaceID("no-such-id"))

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "mu", Goal: "do it", MergePolicy: thread.MergeManual})
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
func TestManager_SendOwnershipFollowsLatestRun(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)
	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "owner", Goal: "go", MergePolicy: thread.MergeManual})
	require.NoError(t, err)
	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, time.Second))
	require.Nil(t, mgr.Handle(st.ID))

	// Respawn via Send: the workspace must stay alive after Send returns.
	require.NoError(t, sendErr(mgr.Send(t.Context(), st.ID, "first")))
	require.NotNil(t, mgr.Handle(st.ID))
	require.Zero(t, spawner.releases(st.WorktreePath)-1) // only the goal run's release so far

	coord := spawner.coordFor(st.WorktreePath)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, time.Second, time.Millisecond)
	coord.mu.Lock()
	ownerRunID := coord.runs[0].runID
	coord.mu.Unlock()

	// Queue a follow-up while the first run is in flight.
	require.NoError(t, sendErr(mgr.Send(t.Context(), st.ID, "second")))
	require.Eventually(t, func() bool { return coord.runCount() == 2 }, time.Second, time.Millisecond)
	coord.mu.Lock()
	followUpRunID := coord.runs[1].runID
	coord.mu.Unlock()
	require.NotEmpty(t, followUpRunID)
	require.NotEqual(t, ownerRunID, followUpRunID)

	// The first run completing must neither release the workspace nor
	// settle the thread: the queued follow-up still owns both.
	spawner.appFor(st.WorktreePath).RunCompletions().Publish(pubsub.UpdatedEvent, notify.RunComplete{SessionID: st.SessionID, RunID: ownerRunID, Text: "first done"})
	require.Never(t, func() bool { return mgr.Handle(st.ID) == nil }, 100*time.Millisecond, 10*time.Millisecond)
	got, err := mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusRunning, got.Status)

	// The follow-up completing settles everything.
	spawner.appFor(st.WorktreePath).RunCompletions().Publish(pubsub.UpdatedEvent, notify.RunComplete{SessionID: st.SessionID, RunID: followUpRunID, Text: "second done"})
	require.Eventually(t, func() bool { return mgr.Handle(st.ID) == nil }, time.Second, time.Millisecond)
	require.Eventually(t, func() bool {
		got, err := mgr.Get(t.Context(), st.ID)
		return err == nil && got.Status == thread.StatusCompleted
	}, time.Second, time.Millisecond)
}

func TestManager_ConcurrentSendRespawnsOnce(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)
	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "once", Goal: "go", MergePolicy: thread.MergeManual})
	require.NoError(t, err)
	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, time.Second))

	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() { require.NoError(t, sendErr(mgr.Send(t.Context(), st.ID, "again"))) })
	}
	wg.Wait()
	require.Equal(t, 2, spawner.spawns())
	coord := spawner.coordFor(st.WorktreePath)
	// coordFor returns the coordinator of the respawned workspace (each
	// Spawn builds a fresh one): one respawned run from the first Send
	// plus eleven queued follow-ups — each dispatched asynchronously, so
	// wait for all of them to land before completing them.
	require.Eventually(t, func() bool { return coord.runCount() == 12 }, time.Second, time.Millisecond)
	// Every dispatched turn eventually publishes its own RunComplete. The
	// workspace is released only by the turn that currently owns the
	// runtime (the last dispatched RunID) — completing all of them models
	// that and asserts exactly one effective release.
	coord.mu.Lock()
	runIDs := make([]string, 0, len(coord.runs))
	for _, r := range coord.runs {
		runIDs = append(runIDs, r.runID)
	}
	coord.mu.Unlock()
	for _, id := range runIDs {
		spawner.appFor(st.WorktreePath).RunCompletions().Publish(pubsub.UpdatedEvent, notify.RunComplete{SessionID: st.SessionID, RunID: id, Text: "finished"})
	}
	require.Eventually(t, func() bool { return mgr.Handle(st.ID) == nil }, time.Second, time.Millisecond)
	// Wait for the release rather than asserting it outright: the
	// run-completion path clears c.runtime (which is what makes Handle
	// return nil) and only then calls Spawner.Release, so a bare
	// assertion here races that window and fails intermittently under
	// -race.
	require.Eventually(t, func() bool { return spawner.releases(st.WorktreePath) >= 2 }, time.Second, time.Millisecond)
}

func TestManager_CancelledRunCompleteWinsOverError(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)
	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "cancel-error", Goal: "go", MergePolicy: thread.MergeManual})
	require.NoError(t, err)
	coord := spawner.coordFor(st.WorktreePath)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, time.Second, time.Millisecond)
	coord.mu.Lock()
	runID := coord.runs[0].runID
	coord.mu.Unlock()
	spawner.appFor(st.WorktreePath).RunCompletions().Publish(pubsub.UpdatedEvent, notify.RunComplete{SessionID: st.SessionID, RunID: runID, Error: "cancelled", Cancelled: true})
	require.Eventually(t, func() bool {
		got, err := mgr.Get(t.Context(), st.ID)
		return err == nil && got.Status == thread.StatusInterrupted
	}, time.Second, time.Millisecond)
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
	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "immediate-error", Goal: "go", MergePolicy: thread.MergeManual})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		got, err := mgr.Get(t.Context(), st.ID)
		return err == nil && got.Status == thread.StatusFailed && strings.Contains(got.Error, "boom")
	}, time.Second, time.Millisecond)
	require.Nil(t, mgr.Handle(st.ID))
	require.Equal(t, 1, spawner.releases(st.WorktreePath))
	require.NoError(t, mgr.Shutdown(t.Context()))
}

func TestManager_ConcurrentShutdownClosesMutations(t *testing.T) {
	repo := initRepo(t)
	mgr, _ := newTestManager(t, repo)
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() { require.NoError(t, mgr.Shutdown(t.Context())) })
	}
	wg.Wait()
	_, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "closed", Goal: "go"})
	require.ErrorIs(t, err, thread.ErrManagerClosed)
	require.ErrorIs(t, sendErr(mgr.Send(t.Context(), "missing", "go")), thread.ErrManagerClosed)
	_, mergeErr := mgr.Merge(t.Context(), "missing")
	require.ErrorIs(t, mergeErr, thread.ErrManagerClosed)
	require.ErrorIs(t, mgr.Remove(t.Context(), "missing", true, false), thread.ErrManagerClosed)
	require.ErrorIs(t, mgr.Recover(t.Context()), thread.ErrManagerClosed)
}

func TestManager_ShutdownWaitsForCancelledSpawnRollback(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)
	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "blocked", Goal: "go", MergePolicy: thread.MergeManual})
	require.NoError(t, err)
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
	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "race-remove", Goal: "go", MergePolicy: thread.MergeManual})
	require.NoError(t, err)
	coord := spawner.coordFor(st.WorktreePath)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, time.Second, time.Millisecond)
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
	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "force-cancel", Goal: "go", MergePolicy: thread.MergeManual})
	require.NoError(t, err)

	coord := spawner.coordFor(st.WorktreePath)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, time.Second, time.Millisecond)

	require.NoError(t, mgr.Remove(t.Context(), st.ID, true, true))

	require.Equal(t, []string{st.SessionID}, coord.canceledSessions(),
		"force-removing a running thread must still cancel its own session")
}

// TestManager_ShutdownBlocksAdmission verifies that Manager.Shutdown blocks
// new operations immediately: once Shutdown starts, Create/Send/Merge/Remove
// return ErrManagerClosed even before the shutdown goroutine finishes its
// cleanup work. Uses a controlled barrier (no Sleep) to synchronize.
func TestManager_ShutdownBlocksAdmission(t *testing.T) {
	repo := initRepo(t)
	mgr, _ := newTestManager(t, repo)

	// barrier: test signals shutdown to start, then waits for it to block,
	// then releases it.
	barrier := make(chan struct{})
	go func() {
		// Call Shutdown (sets closed=true inside sync.Once, then launches
		// cleanup goroutine). This blocks until barrier is closed.
		_ = mgr.Shutdown(context.Background())
		close(barrier)
	}()

	// Yield to let the shutdown goroutine run and set closed=true.
	time.Sleep(10 * time.Millisecond)

	// Operations during shutdown should fail immediately.
	_, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "blocked", Goal: "x"})
	require.ErrorIs(t, err, thread.ErrManagerClosed)
	err = sendErr(mgr.Send(t.Context(), "missing", "x"))
	require.ErrorIs(t, err, thread.ErrManagerClosed)
	_, err = mgr.Merge(t.Context(), "missing")
	require.ErrorIs(t, err, thread.ErrManagerClosed)
	err = mgr.Remove(t.Context(), "missing", true, false)
	require.ErrorIs(t, err, thread.ErrManagerClosed)
	err = mgr.Recover(t.Context())
	require.ErrorIs(t, err, thread.ErrManagerClosed)

	<-barrier
}
