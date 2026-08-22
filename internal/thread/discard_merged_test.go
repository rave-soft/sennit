package thread_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/thread"

	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/stretchr/testify/require"
)

// requireDiscarded asserts that a merged thread has been cleared away
// completely: no store row, no worktree on disk, no branch. Use after a
// synchronous Merge; the automatic path finishes off-goroutine and needs
// requireDiscardedEventually instead.
func requireDiscarded(t *testing.T, mgr *thread.Manager, repo string, st thread.Thread) {
	t.Helper()
	_, err := mgr.Get(context.Background(), st.ID)
	require.Error(t, err, "a merged thread's row must be gone")
	require.NoDirExists(t, st.WorktreePath, "a merged thread's worktree must be gone")
	requireBranchGone(t, repo, st.Branch)
}

// requireDiscardedEventually is requireDiscarded for the automatic merge
// path, which discards after delivering the outcome, on its own goroutine.
func requireDiscardedEventually(t *testing.T, mgr *thread.Manager, repo string, st thread.Thread) {
	t.Helper()
	require.Eventually(t, func() bool {
		_, err := mgr.Get(context.Background(), st.ID)
		return err != nil
	}, 5*time.Second, 5*time.Millisecond, "a merged thread's row must be gone")
	require.NoDirExists(t, st.WorktreePath)
	requireBranchGone(t, repo, st.Branch)
}

func requireBranchGone(t *testing.T, repo, branch string) {
	t.Helper()
	out := runGit(t, repo, "branch", "--list", branch)
	require.Empty(t, strings.TrimSpace(out), "a merged thread's branch must be deleted")
}

// newTestManagerWithRealMessages wires a Manager whose parent App has a
// real, sqlite-backed session and message service, so the note a discard
// writes into the parent session's history can actually be read back.
func newTestManagerWithRealMessages(t *testing.T, repo string) (*thread.Manager, *fakeSpawner, session.Service, message.Service) {
	t.Helper()
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
	})
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	q := db.New(conn)
	sessions := session.NewService(q, conn, "/test/project")
	messages := message.NewService(q)

	parentApp := app.NewForTest(context.Background())
	t.Cleanup(parentApp.ShutdownForTest)
	parentApp.SetSessionsForTest(sessions)
	parentApp.SetMessagesForTest(messages)
	parentApp.AgentCoordinator = &fakeCoordinator{}

	spawner := newFakeSpawner(t)
	mgr := thread.NewManager(thread.ManagerOptions{
		Store:       thread.NewStoreForTest(t),
		Spawner:     spawner,
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
		ParentApp:   &testAppWorkspace{app: parentApp},
	})
	return mgr, spawner, sessions, messages
}

// TestDiscardMerged_WritesTheDisappearanceIntoHistory is the point of the
// whole feature: a thread that merges vanishes from the panel, and the
// user can still find out what happened to it. Without the note, a row
// silently ceasing to exist is indistinguishable from a bug.
//
// The note is a system-role message on purpose. That role is dropped when
// the prompt is assembled, so it is a record for the person, not a second
// telling for the model — which already learned of the merge through the
// completion inbox.
func TestDiscardMerged_WritesTheDisappearanceIntoHistory(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner, sessions, messages := newTestManagerWithRealMessages(t, repo)

	parent, err := sessions.Create(t.Context(), "parent")
	require.NoError(t, err)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name:            "tidy-up",
		Goal:            "do it",
		ParentSessionID: parent.ID,
	})
	require.NoError(t, err)

	writeFile(t, st.WorktreePath, "output.txt", "done\n")
	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, settleTimeout))
	requireDiscardedEventually(t, mgr, repo, st)

	var notice message.Message
	require.Eventually(t, func() bool {
		msgs, listErr := messages.List(context.Background(), parent.ID)
		if listErr != nil {
			return false
		}
		for _, m := range msgs {
			if m.Role == message.System {
				notice = m
				return true
			}
		}
		return false
	}, 5*time.Second, 5*time.Millisecond, "the merge and removal must be recorded in the parent session")

	text := notice.Content().Text
	require.Contains(t, text, "tidy-up", "the note must name the thread")
	require.Contains(t, text, "merged")
	require.Contains(t, text, "removed")
	require.NotContains(t, text, "kept", "nothing was left behind, so the note must not claim otherwise")
}

// TestDiscardMerged_PublishesRemovalSoThePanelDrops: the panel drops a row
// the moment a removal is announced rather than waiting for a re-list (see
// threadsDockState.applyThreadEvent), so this event is what actually makes
// the thread leave the screen. EventRemoved is also the one that maps to
// pubsub.DeletedEvent on the way to the TUI (threadEventPubsubType), which
// is what the drop keys off — announcing the discard as a mere status
// change would leave the row on screen.
func TestDiscardMerged_PublishesRemovalSoThePanelDrops(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)
	events := mgr.Subscribe(t.Context())

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "vanishes", Goal: "do it"})
	require.NoError(t, err)

	writeFile(t, st.WorktreePath, "output.txt", "done\n")
	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, settleTimeout))

	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Payload.Type == thread.EventRemoved && ev.Payload.Thread.ID == st.ID {
				return
			}
		case <-deadline:
			t.Fatal("a merged thread never announced its removal")
		}
	}
}

// TestDiscardMerged_ConflictKeepsEverything: a conflict is precisely the
// state where the user needs the worktree — that is where they resolve it,
// and Merge is retried against it afterwards. Clearing it away because the
// merge flow "finished" would destroy the work.
func TestDiscardMerged_ConflictKeepsEverything(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "collides", Goal: "do it"})
	require.NoError(t, err)

	// Both branches edit README.md, so the automatic merge conflicts.
	writeFile(t, repo, "README.md", "main version\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "edit on main")
	writeFile(t, st.WorktreePath, "README.md", "thread version\n")

	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, settleTimeout))

	got, err := mgr.Get(t.Context(), st.ID)
	require.NoError(t, err, "a conflicted thread must keep its row")
	require.Equal(t, thread.StatusConflict, got.Status)
	require.DirExists(t, st.WorktreePath, "a conflicted thread must keep its worktree to resolve in")
	require.Contains(t, runGit(t, repo, "branch", "--list", st.Branch), st.Branch)

	// And once resolved by hand, the same thread is discarded like any
	// other merge.
	writeFile(t, st.WorktreePath, "README.md", "resolved\n")
	runGit(t, st.WorktreePath, "add", "README.md")
	_, mergeErr := mgr.Merge(t.Context(), st.ID)
	require.NoError(t, mergeErr)
	requireDiscarded(t, mgr, repo, st)
}

// TestDiscardMerged_DeliversTheOutcomeBeforeRemovingTheRow guards the
// ordering the whole wake-up path depends on: deliverMergeOutcome re-reads
// the store row to learn which outcome the attempt reached, so a discard
// that ran first would leave the parent agent never hearing that its
// thread landed.
func TestDiscardMerged_DeliversTheOutcomeBeforeRemovingTheRow(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner, parentApp := newTestManagerWithParentApp(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name:            "reports-then-goes",
		Goal:            "do it",
		ParentSessionID: "parent-sess",
	})
	require.NoError(t, err)

	writeFile(t, st.WorktreePath, "output.txt", "done\n")
	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, settleTimeout))
	requireDiscardedEventually(t, mgr, repo, st)

	coord := parentApp.AgentCoordinator.(*fakeCoordinator)
	delivered := coord.deliveredCompletions()
	require.Len(t, delivered, 1, "the parent must still be told, exactly once")
	require.Equal(t, string(thread.StatusMerged), delivered[0].completion.Status)
	require.Equal(t, st.ID, delivered[0].completion.DelegationID)
}

// TestDiscardMerged_KeepsTheRowWhenTheWorktreeCannotGo: if the directory
// cannot be removed there is nothing to gain by deleting the row on top of
// it — that would leave an orphaned worktree no code knows about. A thread
// still listed as merged is the recoverable failure.
func TestDiscardMerged_KeepsTheRowWhenTheWorktreeCannotGo(t *testing.T) {
	repo := initRepo(t)
	mgr, _ := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "stuck", Goal: "do it", MergePolicy: thread.MergeManual})
	require.NoError(t, err)

	_, err = mgr.SetStatusForTest(t.Context(), st.ID, thread.StatusMerged, "", "", time.Now().Unix())
	require.NoError(t, err)

	// Stand in for the real thing: a thread that ran a container leaves
	// behind directories this process does not own and cannot chmod, so the
	// removal fails with the files still there. blockWorktreeRemoval
	// provokes that failure the way each platform actually produces it —
	// see its two (POSIX/Windows) implementations for why one mechanism
	// does not serve both.
	t.Cleanup(blockWorktreeRemoval(t, st.WorktreePath))

	mgr.DiscardMergedForTest(t.Context(), st.ID)

	got, err := mgr.Get(t.Context(), st.ID)
	require.NoError(t, err, "the row must survive a failed worktree removal")
	require.Equal(t, thread.StatusMerged, got.Status)
	require.Contains(t, runGit(t, repo, "branch", "--list", st.Branch), st.Branch,
		"and the branch must not be deleted behind a failed removal either")
	require.DirExists(t, st.WorktreePath)
}

// TestDiscardMerged_FinishesACleanupDoneByHand: the other half of the rule
// above. A worktree and branch someone already removed themselves leave a
// row whose only remaining job is to go away, and every git step of the
// discard has nothing left to do. Failing any of them here is what used to
// strand these rows: nothing could remove them afterwards, because the
// removal insisted on deleting a worktree that was no longer there.
func TestDiscardMerged_FinishesACleanupDoneByHand(t *testing.T) {
	repo := initRepo(t)
	mgr, _ := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "by-hand", Goal: "do it", MergePolicy: thread.MergeManual})
	require.NoError(t, err)

	_, err = mgr.SetStatusForTest(t.Context(), st.ID, thread.StatusMerged, "", "", time.Now().Unix())
	require.NoError(t, err)

	runGit(t, repo, "worktree", "remove", "--force", st.WorktreePath)
	runGit(t, repo, "branch", "-D", st.Branch)

	mgr.DiscardMergedForTest(t.Context(), st.ID)

	_, err = mgr.Get(t.Context(), st.ID)
	require.Error(t, err, "the row must be gone")
}

// TestThreadWorktreeLivesInsideTheDataDirectory pins where a thread's
// checkout goes, end to end, and the property that makes putting it there
// safe: the repository must not see the worktree as a second, untracked
// copy of itself.
//
// That safety comes from the "*" .gitignore the workspace writes into its
// data directory on first use (app.ensureDataDir). This test creates
// that file itself, so if it ever stops being written, this stays green
// while reality does not — the link is only as strong as that one call.
func TestThreadWorktreeLivesInsideTheDataDirectory(t *testing.T) {
	repo := initRepo(t)
	dataDir := filepath.Join(repo, ".sennit")
	require.NoError(t, os.MkdirAll(dataDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, ".gitignore"), []byte("*\n"), 0o644))

	mgr := thread.NewManager(thread.ManagerOptions{
		Store:    thread.NewStoreForTest(t),
		Spawner:  newFakeSpawner(t),
		RepoRoot: repo,
		DataDir:  dataDir,
	})

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "in-place", Goal: "do it"})
	require.NoError(t, err)

	require.Equal(t, filepath.Join(dataDir, "threads", "in-place"), st.WorktreePath)
	require.DirExists(t, st.WorktreePath)
	require.FileExists(t, filepath.Join(st.WorktreePath, "README.md"),
		"the worktree must be a real checkout, not just a directory")

	require.Empty(t, strings.TrimSpace(runGit(t, repo, "status", "--porcelain")),
		"the repository must not see its own thread worktrees")
}

// TestResolve_MissingThreadReportsADomainError: a merged thread is
// deleted, so asking about one by name is an ordinary thing to do and the
// answer has to be readable. It used to surface the store's own
// "sql: no rows in result set" all the way into the agent's tool result,
// which says nothing about threads at all.
func TestResolve_MissingThreadReportsADomainError(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "lands-and-goes", Goal: "do it"})
	require.NoError(t, err)

	writeFile(t, st.WorktreePath, "output.txt", "done\n")
	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, settleTimeout))
	requireDiscardedEventually(t, mgr, repo, st)

	_, err = mgr.Get(t.Context(), st.Name)
	require.ErrorIs(t, err, thread.ErrNotFound)
	require.Contains(t, err.Error(), st.Name, "and it must name what was asked for")
	require.NotContains(t, err.Error(), "sql:", "the store's wording must not reach the caller")

	_, err = mgr.Get(t.Context(), "never-existed")
	require.ErrorIs(t, err, thread.ErrNotFound)
}
