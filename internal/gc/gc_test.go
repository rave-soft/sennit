package gc

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	sennitdb "github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/thread"
	"github.com/stretchr/testify/require"
)

// fixture opens a fresh migrated DB and seeds it with sessions/threads
// exercising Run's selection rules: the exact-cutoff boundary, project
// scoping, and worktree-orphan reporting. cutoff is the unix timestamp
// Run will use as its retention boundary. projectA/projectB are arbitrary
// path strings -- ListSessionsForGC/ListThreadsForGC compare them as
// opaque strings, so they need not be real directories.
type fixtureIDs struct {
	Old      string // project A, 30d before cutoff -- must go
	Boundary string // project A, exactly at cutoff -- must survive (strict <)
	Recent   string // project A, 30d after cutoff -- must survive
	Other    string // project B, 30d before cutoff -- swept only without --project

	ThreadOld      string // project A, completed, 30d before cutoff -- must go
	ThreadBoundary string // project A, completed, exactly at cutoff -- must survive

	WorktreeExists string // project A, completed, old, worktree_path exists on disk -- must be reported
	WorktreeGone   string // project A, completed, old, worktree_path missing -- must NOT be reported
	WorktreeDir    string // the directory WorktreeExists's worktree_path points at
}

const (
	projectA = "/project-a"
	projectB = "/project-b"
)

func fixture(t *testing.T, cutoff int64) (*sennitdb.Queries, *sql.DB, fixtureIDs) {
	t.Helper()
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, sennitdb.Release(dataDir))
	})

	conn, err := sennitdb.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	q := sennitdb.New(conn)
	ctx := context.Background()

	old := time.Unix(cutoff, 0).Add(-30 * 24 * time.Hour).Unix()
	recent := time.Unix(cutoff, 0).Add(30 * 24 * time.Hour).Unix()

	setSessionTime := func(id string, updatedAt int64) {
		_, err := conn.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, updatedAt, id)
		require.NoError(t, err)
	}
	setThread := func(id string, updatedAt int64) {
		_, err := conn.ExecContext(ctx, `UPDATE threads SET status = ?, updated_at = ? WHERE id = ?`, "completed", updatedAt, id)
		require.NoError(t, err)
	}

	ids := fixtureIDs{
		Old:      "sess-old",
		Boundary: "sess-boundary",
		Recent:   "sess-recent",
		Other:    "sess-other-project",

		ThreadOld:      "thread-old",
		ThreadBoundary: "thread-boundary",

		WorktreeExists: "thread-worktree-exists",
		WorktreeGone:   "thread-worktree-gone",
	}

	mustSession := func(id, projectPath string) {
		_, err := q.CreateSession(ctx, sennitdb.CreateSessionParams{ID: id, Title: id, ProjectPath: projectPath})
		require.NoError(t, err)
	}
	mustSession(ids.Old, projectA)
	mustSession(ids.Boundary, projectA)
	mustSession(ids.Recent, projectA)
	mustSession(ids.Other, projectB)

	mustThread := func(id, name, worktreePath string) {
		_, err := q.CreateThread(ctx, sennitdb.CreateThreadParams{
			ID: id, Name: name, ProjectPath: projectA, Goal: "goal", BaseBranch: "main",
			Branch: "thread/" + name, WorktreePath: worktreePath, Status: "pending", MergePolicy: "auto", Kind: "thread",
		})
		require.NoError(t, err)
	}
	mustThread(ids.ThreadOld, "old", "/tmp/nonexistent-"+ids.ThreadOld)
	mustThread(ids.ThreadBoundary, "boundary", "/tmp/nonexistent-"+ids.ThreadBoundary)

	ids.WorktreeDir = filepath.Join(t.TempDir(), "worktree-exists")
	require.NoError(t, os.MkdirAll(ids.WorktreeDir, 0o755))
	mustThread(ids.WorktreeExists, "worktree-exists", ids.WorktreeDir)
	mustThread(ids.WorktreeGone, "worktree-gone", filepath.Join(t.TempDir(), "never-created"))

	// Backdating writes go last: an AFTER UPDATE trigger stamps
	// updated_at to "now" on every UPDATE, exempting only an UPDATE that
	// sets updated_at explicitly (see migration 20260811000001).
	setSessionTime(ids.Old, old)
	setSessionTime(ids.Boundary, cutoff)
	setSessionTime(ids.Recent, recent)
	setSessionTime(ids.Other, old)
	setThread(ids.ThreadOld, old)
	setThread(ids.ThreadBoundary, cutoff)
	setThread(ids.WorktreeExists, old)
	setThread(ids.WorktreeGone, old)

	return q, conn, ids
}

func sessionExists(t *testing.T, q *sennitdb.Queries, id string) bool {
	t.Helper()
	_, err := q.GetSessionByID(context.Background(), id)
	return err == nil
}

func threadExists(t *testing.T, q *sennitdb.Queries, id string) bool {
	t.Helper()
	_, err := q.GetThread(context.Background(), id)
	return err == nil
}

// TestRun_RetentionCutoffBoundary confirms the strict "<" comparison: a
// row exactly at cutoff survives, one 30 days before it does not.
func TestRun_RetentionCutoffBoundary(t *testing.T) {
	cutoff := time.Now().Unix()
	q, conn, ids := fixture(t, cutoff)

	report, err := Run(context.Background(), Deps{Queries: q, Conn: conn}, Policy{Cutoff: cutoff})
	require.NoError(t, err)

	require.False(t, sessionExists(t, q, ids.Old))
	require.True(t, sessionExists(t, q, ids.Boundary))
	require.False(t, threadExists(t, q, ids.ThreadOld))
	require.True(t, threadExists(t, q, ids.ThreadBoundary))
	require.Positive(t, report.SessionsDeleted)
}

// TestRun_ProjectFilter confirms --project's scoping: passing ProjectPath
// must leave another project's old session alone.
func TestRun_ProjectFilter(t *testing.T) {
	cutoff := time.Now().Unix()
	q, conn, ids := fixture(t, cutoff)

	_, err := Run(context.Background(), Deps{Queries: q, Conn: conn}, Policy{Cutoff: cutoff, ProjectPath: projectA})
	require.NoError(t, err)

	require.False(t, sessionExists(t, q, ids.Old), "project A's old session must go")
	require.True(t, sessionExists(t, q, ids.Other), "project B is out of scope for --project")
}

// TestRun_DryRunMakesNoWrites confirms a dry run reports what it would
// delete without touching a single row or issuing VACUUM.
func TestRun_DryRunMakesNoWrites(t *testing.T) {
	cutoff := time.Now().Unix()
	q, conn, ids := fixture(t, cutoff)

	report, err := Run(context.Background(), Deps{Queries: q, Conn: conn}, Policy{Cutoff: cutoff, DryRun: true})
	require.NoError(t, err)

	require.Positive(t, report.SessionsDeleted, "dry run must still report what it would delete")
	require.False(t, report.Vacuumed, "dry run must never vacuum")

	require.True(t, sessionExists(t, q, ids.Old), "dry run must not delete anything")
	require.True(t, sessionExists(t, q, ids.Other))
	require.True(t, threadExists(t, q, ids.ThreadOld))
}

// TestRun_OrphanedWorktrees_OnlyExistingPaths confirms the report lists
// only worktree paths that still exist on disk -- an already-cleaned-up
// worktree_path must not appear even though its owning thread row is
// still deleted.
func TestRun_OrphanedWorktrees_OnlyExistingPaths(t *testing.T) {
	cutoff := time.Now().Unix()
	q, conn, ids := fixture(t, cutoff)

	report, err := Run(context.Background(), Deps{Queries: q, Conn: conn}, Policy{Cutoff: cutoff})
	require.NoError(t, err)

	require.Contains(t, report.OrphanedWorktrees, ids.WorktreeDir)
	require.Len(t, report.OrphanedWorktrees, 1)
	require.False(t, threadExists(t, q, ids.WorktreeExists))
	require.False(t, threadExists(t, q, ids.WorktreeGone), "the gone-worktree thread row is still deleted")
}

// TestRun_ProtectsSessionOfLiveDelegation reproduces the bug where an old
// top-level session P sweeps its descendants purely by
// sessions.parent_session_id, without consulting the threads table: a
// still-running (non-terminal) thread T owns a fresh child session C
// (threads.session_id) that is reachable from P via that descendant walk,
// and T's own parent_session_id also names P. Both P and C must survive:
// deleting either while T is actively writing to C (and will later write
// its completion into P) would violate the messages.session_id FK and
// corrupt an in-flight background task. Run against unfixed selectSessions,
// C (and transitively nothing beneath it) is swept because the BFS never
// consults threads at all.
func TestRun_ProtectsSessionOfLiveDelegation(t *testing.T) {
	cutoff := time.Now().Unix()
	q, conn, ids := fixture(t, cutoff)
	ctx := context.Background()

	old := time.Unix(cutoff, 0).Add(-30 * 24 * time.Hour).Unix()
	recent := time.Unix(cutoff, 0).Add(30 * 24 * time.Hour).Unix()

	// P: an old top-level session, swept on its own age merits.
	parentID := "sess-live-parent"
	_, err := q.CreateSession(ctx, sennitdb.CreateSessionParams{ID: parentID, Title: parentID, ProjectPath: projectA})
	require.NoError(t, err)

	// C: a fresh child session parented to P via sessions.parent_session_id
	// -- exactly how a real task/delegation session is created -- so the
	// descendant BFS actually reaches it.
	childID := "sess-live-child"
	_, err = q.CreateSession(ctx, sennitdb.CreateSessionParams{
		ID: childID, Title: childID, ProjectPath: projectA,
		ParentSessionID: sql.NullString{String: parentID, Valid: true},
	})
	require.NoError(t, err)

	// T: a live (non-terminal) thread that owns C and reports back to P.
	threadID := "thread-live"
	_, err = q.CreateThread(ctx, sennitdb.CreateThreadParams{
		ID: threadID, Name: "live", ProjectPath: projectA, Goal: "goal", BaseBranch: "main",
		Branch: "thread/live", WorktreePath: "", SessionID: childID, Status: "running",
		MergePolicy: "auto", Kind: "thread", ParentSessionID: parentID,
	})
	require.NoError(t, err)

	_, err = conn.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, old, parentID)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, recent, childID)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `UPDATE threads SET updated_at = ? WHERE id = ?`, recent, threadID)
	require.NoError(t, err)

	_, err = Run(context.Background(), Deps{Queries: q, Conn: conn}, Policy{Cutoff: cutoff})
	require.NoError(t, err)

	require.True(t, sessionExists(t, q, parentID), "P is the parent session of a live thread and must survive")
	require.True(t, sessionExists(t, q, childID), "C is the session a live thread owns and must survive")
	require.True(t, threadExists(t, q, threadID), "a non-terminal thread is never deleted")

	// Unrelated fixture rows keep their ordinary fate untouched by this
	// protection.
	require.False(t, sessionExists(t, q, ids.Old))
}

// TestTerminalStatusParityWithThread pins gc's local terminal classification
// (persistedThreadStatus.terminal) against the domain classification
// thread.Status.Terminal. gc deliberately keeps its own set of the persisted
// statuses instead of importing the thread package (or the proto boundary),
// so the two packages cannot share a type; this test is what stops the two
// copies from drifting apart — a status a newer thread package calls terminal
// that gc still treats as live (or vice versa) would otherwise change what
// `sennit gc` deletes without a compiler error anywhere.
func TestTerminalStatusParityWithThread(t *testing.T) {
	t.Parallel()

	domainStatuses := []thread.Status{
		thread.StatusPending,
		thread.StatusRunning,
		thread.StatusIdle,
		thread.StatusCompleted,
		thread.StatusFailed,
		thread.StatusInterrupted,
		thread.StatusCancelled,
		thread.StatusMerging,
		thread.StatusMerged,
		thread.StatusConflict,
		thread.StatusMergeBlocked,
	}
	for _, status := range domainStatuses {
		require.Equal(t, status.Terminal(), persistedThreadStatus(status).terminal(), "status %q", status)
	}
	for _, status := range []string{"", "brand-new-status"} {
		require.False(t, persistedThreadStatus(status).terminal())
		require.False(t, thread.Status(status).Terminal())
	}
}

// TestPersistedTerminalStatusStrings pins the exact persisted strings gc's
// classification keys on, so a typo in a constant (which would make that
// status silently retained forever) fails here without touching the database.
func TestPersistedTerminalStatusStrings(t *testing.T) {
	t.Parallel()

	require.ElementsMatch(t, []string{
		"completed",
		"merged",
		"failed",
		"cancelled",
		"conflict",
		"merge_blocked",
		"interrupted",
	}, []string{
		string(persistedStatusCompleted),
		string(persistedStatusMerged),
		string(persistedStatusFailed),
		string(persistedStatusCancelled),
		string(persistedStatusConflict),
		string(persistedStatusMergeBlocked),
		string(persistedStatusInterrupted),
	})
	require.Equal(t, "thread", string(persistedKindThread))
}
