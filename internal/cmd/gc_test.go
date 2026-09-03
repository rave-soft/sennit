package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/config"
	sennitdb "github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/testenv"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// newGCTestCmd builds a minimal cobra.Command carrying the flags gcCmd.RunE
// reads, matching the pattern in stat_test.go/models_test.go: tests invoke
// RunE directly rather than going through rootCmd.Execute() to keep them
// hermetic.
func newGCTestCmd(t *testing.T) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	testCmd := &cobra.Command{Use: "gc"}
	testCmd.Flags().StringP("cwd", "c", "", "")
	testCmd.Flags().StringP("data-dir", "D", "", "")
	testCmd.Flags().Int("days", 0, "")
	testCmd.Flags().Bool("dry-run", false, "")
	testCmd.Flags().Bool("project", false, "")
	testCmd.Flags().Bool("json", false, "")

	var stdout bytes.Buffer
	testCmd.SetOut(&stdout)
	testCmd.SetArgs(nil)
	return testCmd, &stdout
}

// gcFixtureIDs names every session/thread gcFixture seeds, so tests can
// assert on exactly which ones survive a gc run.
type gcFixtureIDs struct {
	OldParent    string // project A, updated 120d ago: past the 90d default
	OldChild     string // project A, child of OldParent, updated 1h ago -- must still go with its parent
	YoungParent  string // project A, updated 1h ago: recent parent
	OldOrphan    string // project A, child of YoungParent, updated 120d ago -- old enough on its own
	Boundary     string // project A, just after cutoff -- must survive (strict <)
	Recent       string // project A, updated 1h ago -- must survive
	OtherProject string // project B, updated 120d ago -- only swept with --all-projects (the default)

	ThreadOldDone    string // project A, status completed, 120d old -- must go
	ThreadOldRunning string // project A, status running, 120d old -- must survive (never touch running)
	ThreadRecentDone string // project A, status merged, 1h old -- must survive (not old enough)

	// Worktree-orphan fixtures: WorktreeExists holds a real directory on
	// disk, WorktreeGone's directory is never created, so the two exercise
	// the os.Stat gate that decides whether a selected thread's worktree is
	// reported as orphaned.
	WorktreeExists   string // project A, status completed, 120d old, worktree_path exists -- must be reported
	WorktreeGone     string // project A, status completed, 120d old, worktree_path missing -- must NOT be reported
	WorktreeDir      string // the directory WorktreeExists's worktree_path points at
	TaskWithWorktree string // project A, kind=task, 120d old, worktree_path exists -- must NOT be reported (tasks own no worktree)
}

// gcFixture opens a fresh migrated DB at dir (config.GlobalDBDir() for
// tests that exercise runGC, an arbitrary temp dir otherwise) and seeds it
// with sessions/threads exercising gc's selection rules: age filtering,
// parent/child expansion, the exact-cutoff boundary, project scoping, and
// thread status gating. cutoff is the unix timestamp gc will use as its
// retention boundary (time.Now() minus the retention window); the fixture
// stamps Boundary just after that value to tolerate crossing a Unix-second
// boundary before runGC computes its cutoff. projectA/projectB
// must be real directories: runGC's --cwd resolution chdirs into them.
func gcFixture(t *testing.T, dir string, cutoff int64, projectA, projectB string) gcFixtureIDs {
	t.Helper()
	dataDir := dir
	if dataDir == "" {
		dataDir = t.TempDir()
	}
	t.Cleanup(func() {
		require.NoError(t, sennitdb.Release(dataDir))
		sennitdb.ResetPool()
	})

	conn, err := sennitdb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := sennitdb.New(conn)
	ctx := t.Context()

	// sessions/threads each carry an AFTER UPDATE trigger stamping
	// updated_at to strftime('now'), but only WHEN NEW = OLD — an UPDATE
	// that sets updated_at explicitly keeps its value (see migration
	// 20260811000001). The fixture's backdating UPDATEs below rely on
	// exactly that exemption; the cascading count-trigger UPDATE from
	// CreateMessage still bumps updated_at, which is why backdating must
	// be the last write touching each row.

	old := time.Unix(cutoff, 0).Add(-30 * 24 * time.Hour).Unix()
	recent := time.Unix(cutoff, 0).Add(30 * 24 * time.Hour).Unix()

	setSessionTime := func(id string, updatedAt int64) {
		_, err := conn.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, updatedAt, id)
		require.NoError(t, err)
	}
	setThread := func(id, status string, updatedAt int64) {
		_, err := conn.ExecContext(ctx, `UPDATE threads SET status = ?, updated_at = ? WHERE id = ?`, status, updatedAt, id)
		require.NoError(t, err)
	}

	mustSession := func(id, title, projectPath string, parent string) {
		params := sennitdb.CreateSessionParams{ID: id, Title: title, ProjectPath: projectPath}
		if parent != "" {
			params.ParentSessionID = sql.NullString{String: parent, Valid: true}
		}
		_, err := q.CreateSession(ctx, params)
		require.NoError(t, err)
	}

	ids := gcFixtureIDs{
		OldParent:        "sess-old-parent",
		OldChild:         "sess-old-child",
		YoungParent:      "sess-young-parent",
		OldOrphan:        "sess-old-orphan",
		Boundary:         "sess-boundary",
		Recent:           "sess-recent",
		OtherProject:     "sess-other-project",
		ThreadOldDone:    "thread-old-done",
		ThreadOldRunning: "thread-old-running",
		ThreadRecentDone: "thread-recent-done",

		WorktreeExists:   "thread-worktree-exists",
		WorktreeGone:     "thread-worktree-gone",
		TaskWithWorktree: "task-with-worktree",
	}

	mustSession(ids.OldParent, "old parent", projectA, "")
	// A recently-touched agent-tool child of an old parent: gc must still
	// delete it, since its parent is going away.
	mustSession(ids.OldChild, "old parent's child", projectA, ids.OldParent)
	mustSession(ids.YoungParent, "young parent", projectA, "")
	// An old child of a young (kept) parent: gc must delete it on its own
	// merits, independently of its parent's age.
	mustSession(ids.OldOrphan, "old orphan", projectA, ids.YoungParent)
	mustSession(ids.Boundary, "boundary", projectA, "")
	mustSession(ids.Recent, "recent", projectA, "")
	mustSession(ids.OtherProject, "other project", projectB, "")

	// OldParent carries a message, a file, and a read_files row so counts
	// and cascading deletes are exercised, not just the session row.
	// This must happen before setSessionTime below: inserting a message
	// fires update_session_message_count_on_insert, which UPDATEs the
	// session row and so re-fires update_sessions_updated_at, stamping
	// updated_at back to "now" -- backdating a session must be the last
	// write that touches it.
	_, err = q.CreateMessage(ctx, sennitdb.CreateMessageParams{
		ID: "msg-old-parent-1", SessionID: ids.OldParent, Role: "user", Parts: "[]",
	})
	require.NoError(t, err)
	_, err = q.CreateFile(ctx, sennitdb.CreateFileParams{
		ID: "file-old-parent-1", SessionID: ids.OldParent, Path: "main.go", Content: "package main", Version: 0,
	})
	require.NoError(t, err)
	require.NoError(t, q.RecordFileRead(ctx, sennitdb.RecordFileReadParams{
		SessionID: ids.OldParent, Path: "main.go",
	}))

	mustThread := func(id, name, projectPath string) {
		_, err := q.CreateThread(ctx, sennitdb.CreateThreadParams{
			ID: id, Name: name, ProjectPath: projectPath, Goal: "goal", BaseBranch: "main",
			Branch: "thread/" + name, WorktreePath: "/tmp/" + name, Status: "pending", MergePolicy: "auto", Kind: "thread",
		})
		require.NoError(t, err)
	}
	mustThread(ids.ThreadOldDone, "old-done", projectA)
	mustThread(ids.ThreadOldRunning, "old-running", projectA)
	mustThread(ids.ThreadRecentDone, "recent-done", projectA)

	// A real directory on disk, standing in for a worktree gc must report
	// as orphaned rather than an already-cleaned-up one it must not.
	ids.WorktreeDir = filepath.Join(t.TempDir(), "worktree-exists")
	require.NoError(t, os.MkdirAll(ids.WorktreeDir, 0o755))

	_, err = q.CreateThread(ctx, sennitdb.CreateThreadParams{
		ID: ids.WorktreeExists, Name: "worktree-exists", ProjectPath: projectA, Goal: "goal", BaseBranch: "main",
		Branch: "thread/worktree-exists", WorktreePath: ids.WorktreeDir, Status: "pending", MergePolicy: "auto", Kind: "thread",
	})
	require.NoError(t, err)
	// worktree_path points at a directory that was never created, so gc
	// must treat it as already cleaned up rather than as an orphan.
	_, err = q.CreateThread(ctx, sennitdb.CreateThreadParams{
		ID: ids.WorktreeGone, Name: "worktree-gone", ProjectPath: projectA, Goal: "goal", BaseBranch: "main",
		Branch: "thread/worktree-gone", WorktreePath: filepath.Join(t.TempDir(), "never-created"), Status: "pending", MergePolicy: "auto", Kind: "thread",
	})
	require.NoError(t, err)
	// A task whose worktree_path happens to be set and exist on disk: gc
	// must gate reporting on kind, not merely on the path existing.
	_, err = q.CreateThread(ctx, sennitdb.CreateThreadParams{
		ID: ids.TaskWithWorktree, Name: "task-with-worktree", ProjectPath: projectA, Goal: "goal", BaseBranch: "main",
		Branch: "", WorktreePath: ids.WorktreeDir, Status: "pending", MergePolicy: "auto", Kind: "task",
	})
	require.NoError(t, err)

	// Backdating writes go last: both sessions.updated_at and
	// threads.updated_at are stamped to "now" by an AFTER UPDATE trigger
	// on every UPDATE to that row (by design, so real usage always
	// reflects last activity), so any of these rows getting touched again
	// after this point would silently undo the backdating.
	setSessionTime(ids.OldParent, old)
	setSessionTime(ids.OldChild, recent)
	setSessionTime(ids.YoungParent, recent)
	setSessionTime(ids.OldOrphan, old)
	setSessionTime(ids.Boundary, cutoff+10)
	setSessionTime(ids.Recent, recent)
	setSessionTime(ids.OtherProject, old)
	setThread(ids.ThreadOldDone, "completed", old)
	setThread(ids.ThreadOldRunning, "running", old)
	setThread(ids.ThreadRecentDone, "merged", recent)
	setThread(ids.WorktreeExists, "completed", old)
	setThread(ids.WorktreeGone, "completed", old)
	setThread(ids.TaskWithWorktree, "completed", old)

	return ids
}

func sessionExists(t *testing.T, dataDir, id string) bool {
	t.Helper()
	conn, err := sennitdb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	defer sennitdb.Release(dataDir) //nolint:errcheck
	q := sennitdb.New(conn)
	_, err = q.GetSessionByID(t.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	require.NoError(t, err)
	return true
}

func threadExists(t *testing.T, dataDir, id string) bool {
	t.Helper()
	conn, err := sennitdb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	defer sennitdb.Release(dataDir) //nolint:errcheck
	q := sennitdb.New(conn)
	_, err = q.GetThread(t.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	require.NoError(t, err)
	return true
}

func TestGC_AllProjects_DeletesOldAndCascades(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	dataDir := config.GlobalDBDir()
	cutoff := time.Now().AddDate(0, 0, -90).Unix()
	projectA, projectB := t.TempDir(), t.TempDir()
	t.Cleanup(func() { testenv.AssertRemovableOnWindows(t, projectA) })
	ids := gcFixture(t, dataDir, cutoff, projectA, projectB)

	testCmd, stdout := newGCTestCmd(t)
	setCwdFlag(t, testCmd, projectA)
	require.NoError(t, runGC(testCmd, nil))
	require.NotEmpty(t, stdout.String())

	// Deleted: the old parent, its recently-touched child, the
	// independently-old orphan under a kept parent, and the other
	// project's old session (default scope is the whole database).
	require.False(t, sessionExists(t, dataDir, ids.OldParent))
	require.False(t, sessionExists(t, dataDir, ids.OldChild))
	require.False(t, sessionExists(t, dataDir, ids.OldOrphan))
	require.False(t, sessionExists(t, dataDir, ids.OtherProject))

	// Kept: everything at or after the cutoff, and the young parent whose
	// old child was removed out from under it.
	require.True(t, sessionExists(t, dataDir, ids.Boundary))
	require.True(t, sessionExists(t, dataDir, ids.Recent))
	require.True(t, sessionExists(t, dataDir, ids.YoungParent))

	// Cascade: the old parent's message/file/read_files rows are gone too.
	conn, err := sennitdb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	defer sennitdb.Release(dataDir) //nolint:errcheck
	q := sennitdb.New(conn)
	messages, err := q.ListMessagesBySession(t.Context(), ids.OldParent)
	require.NoError(t, err)
	require.Empty(t, messages)

	// Threads: only the old, finished thread is gone; the old-but-running
	// and the recent-but-finished threads both survive.
	require.False(t, threadExists(t, dataDir, ids.ThreadOldDone))
	require.True(t, threadExists(t, dataDir, ids.ThreadOldRunning))
	require.True(t, threadExists(t, dataDir, ids.ThreadRecentDone))
}

func TestGC_ProjectScope_LeavesOtherProjectsAlone(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	dataDir := config.GlobalDBDir()
	cutoff := time.Now().AddDate(0, 0, -90).Unix()
	projectA, projectB := t.TempDir(), t.TempDir()
	ids := gcFixture(t, dataDir, cutoff, projectA, projectB)

	testCmd, _ := newGCTestCmd(t)
	setCwdFlag(t, testCmd, projectA)
	require.NoError(t, testCmd.Flags().Set("project", "true"))
	require.NoError(t, runGC(testCmd, nil))

	require.False(t, sessionExists(t, dataDir, ids.OldParent))
	// --project scopes to project A; project B's old session must survive.
	require.True(t, sessionExists(t, dataDir, ids.OtherProject))
}

func TestGC_DryRun_ChangesNothing(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	dataDir := config.GlobalDBDir()
	cutoff := time.Now().AddDate(0, 0, -90).Unix()
	projectA, projectB := t.TempDir(), t.TempDir()
	ids := gcFixture(t, dataDir, cutoff, projectA, projectB)

	dbPath := filepath.Join(dataDir, "sennit.db")
	before, err := os.Stat(dbPath)
	require.NoError(t, err)

	testCmd, stdout := newGCTestCmd(t)
	setCwdFlag(t, testCmd, projectA)
	require.NoError(t, testCmd.Flags().Set("dry-run", "true"))
	require.NoError(t, runGC(testCmd, nil))
	require.Contains(t, stdout.String(), "Would delete")

	require.True(t, sessionExists(t, dataDir, ids.OldParent))
	require.True(t, sessionExists(t, dataDir, ids.OldChild))
	require.True(t, sessionExists(t, dataDir, ids.OtherProject))
	require.True(t, threadExists(t, dataDir, ids.ThreadOldDone))

	after, err := os.Stat(dbPath)
	require.NoError(t, err)
	require.Equal(t, before.Size(), after.Size())
}

func TestGC_RetentionZero_IsNoOp(t *testing.T) {
	seed := `{"options": {"history_retention_days": 0}}`
	setupHermeticConfigEnv(t, seed)
	dataDir := config.GlobalDBDir()
	// Any cutoff works here: retention 0 must skip selection entirely, so
	// use the default-retention cutoff for a fixture that would otherwise
	// have plenty to delete.
	cutoff := time.Now().AddDate(0, 0, -90).Unix()
	projectA, projectB := t.TempDir(), t.TempDir()
	ids := gcFixture(t, dataDir, cutoff, projectA, projectB)

	testCmd, stdout := newGCTestCmd(t)
	setCwdFlag(t, testCmd, projectA)
	require.NoError(t, runGC(testCmd, nil))
	require.Contains(t, stdout.String(), "nothing to do")

	require.True(t, sessionExists(t, dataDir, ids.OldParent))
	require.True(t, sessionExists(t, dataDir, ids.OtherProject))
	require.True(t, threadExists(t, dataDir, ids.ThreadOldDone))
}

func TestGC_DaysFlagOverridesConfig(t *testing.T) {
	seed := `{"options": {"history_retention_days": 365}}`
	setupHermeticConfigEnv(t, seed)
	dataDir := config.GlobalDBDir()
	// The fixture's "old" sessions sit 120 days before the cutoff passed
	// in; with the configured 365-day retention they would survive, but
	// --days 90 should override the config and delete them anyway.
	cutoff := time.Now().AddDate(0, 0, -90).Unix()
	projectA, projectB := t.TempDir(), t.TempDir()
	ids := gcFixture(t, dataDir, cutoff, projectA, projectB)

	testCmd, _ := newGCTestCmd(t)
	setCwdFlag(t, testCmd, projectA)
	require.NoError(t, testCmd.Flags().Set("days", "90"))
	require.NoError(t, runGC(testCmd, nil))

	require.False(t, sessionExists(t, dataDir, ids.OldParent))
}

func TestGC_VacuumShrinksFile(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	dataDir := config.GlobalDBDir()
	cutoff := time.Now().AddDate(0, 0, -90).Unix()
	projectA, projectB := t.TempDir(), t.TempDir()
	gcFixture(t, dataDir, cutoff, projectA, projectB)

	// Bulk up the DB with padding that gc will delete, so VACUUM has
	// something measurable to reclaim.
	conn, err := sennitdb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := sennitdb.New(conn)
	old := time.Unix(cutoff, 0).Add(-30 * 24 * time.Hour).Unix()
	for i := range 200 {
		id := "sess-pad-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		_, err := q.CreateSession(t.Context(), sennitdb.CreateSessionParams{ID: id, Title: "pad", ProjectPath: projectA})
		require.NoError(t, err)
		_, err = conn.ExecContext(t.Context(), `UPDATE sessions SET updated_at = ? WHERE id = ?`, old, id)
		require.NoError(t, err)
		// One version per padding session, numbered globally for the
		// path: UNIQUE(path, version) is what stops two sessions from
		// both claiming version 0 of "p".
		_, err = q.CreateFile(t.Context(), sennitdb.CreateFileParams{
			ID: id + "-f", SessionID: id, Path: "p", Content: string(make([]byte, 8192)), Version: int64(i),
		})
		require.NoError(t, err)
	}
	require.NoError(t, sennitdb.Release(dataDir))

	dbPath := filepath.Join(dataDir, "sennit.db")
	before, err := os.Stat(dbPath)
	require.NoError(t, err)

	testCmd, _ := newGCTestCmd(t)
	setCwdFlag(t, testCmd, projectA)
	require.NoError(t, runGC(testCmd, nil))

	after, err := os.Stat(dbPath)
	require.NoError(t, err)
	require.Less(t, after.Size(), before.Size())
}

func TestGC_AuthoritativeSelectionKeepsNewlyActiveSession(t *testing.T) {
	dataDir := t.TempDir()
	cutoff := time.Now().AddDate(0, 0, -90).Unix()
	projectA, projectB := t.TempDir(), t.TempDir()
	ids := gcFixture(t, dataDir, cutoff, projectA, projectB)

	conn, err := sennitdb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := sennitdb.New(conn)
	preview, err := gcCollect(t.Context(), q, conn, cutoff, "")
	require.NoError(t, err)
	require.Contains(t, preview.sessionIDs, ids.OldParent)

	// This models activity in the interval before the authoritative writer
	// transaction begins. The old preview must not be used for deletion.
	_, err = conn.ExecContext(t.Context(), `UPDATE sessions SET updated_at = ? WHERE id = ?`, time.Now().Unix(), ids.OldParent)
	require.NoError(t, err)

	selection, err := gcDelete(t.Context(), conn, q, cutoff, "")
	require.NoError(t, err)
	require.NotContains(t, selection.sessionIDs, ids.OldParent)
	require.Zero(t, selection.messagesDeleted)
	require.True(t, sessionExists(t, dataDir, ids.OldParent))
}

func TestGC_AuthoritativeSelectionIncludesNewDescendant(t *testing.T) {
	dataDir := t.TempDir()
	cutoff := time.Now().AddDate(0, 0, -90).Unix()
	projectA, projectB := t.TempDir(), t.TempDir()
	ids := gcFixture(t, dataDir, cutoff, projectA, projectB)

	conn, err := sennitdb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := sennitdb.New(conn)
	preview, err := gcCollect(t.Context(), q, conn, cutoff, "")
	require.NoError(t, err)
	require.NotContains(t, preview.sessionIDs, "sess-new-child")

	_, err = q.CreateSession(t.Context(), sennitdb.CreateSessionParams{
		ID:              "sess-new-child",
		Title:           "new child",
		ProjectPath:     projectA,
		ParentSessionID: sql.NullString{String: ids.OldParent, Valid: true},
	})
	require.NoError(t, err)

	selection, err := gcDelete(t.Context(), conn, q, cutoff, "")
	require.NoError(t, err)
	require.Contains(t, selection.sessionIDs, "sess-new-child")
	require.False(t, sessionExists(t, dataDir, "sess-new-child"))
}

func TestGC_AuthoritativeSelectionKeepsActiveAndUnknownThreads(t *testing.T) {
	dataDir := t.TempDir()
	cutoff := time.Now().AddDate(0, 0, -90).Unix()
	projectA, projectB := t.TempDir(), t.TempDir()
	ids := gcFixture(t, dataDir, cutoff, projectA, projectB)

	conn, err := sennitdb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := sennitdb.New(conn)
	preview, err := gcCollect(t.Context(), q, conn, cutoff, "")
	require.NoError(t, err)
	require.Contains(t, preview.threadIDs, ids.ThreadOldDone)

	_, err = conn.ExecContext(t.Context(), `UPDATE threads SET status = 'running' WHERE id = ?`, ids.ThreadOldDone)
	require.NoError(t, err)
	_, err = q.CreateThread(t.Context(), sennitdb.CreateThreadParams{
		ID: "thread-unknown", Name: "unknown", ProjectPath: projectA, Goal: "goal", BaseBranch: "main",
		Branch: "thread/unknown", WorktreePath: "/tmp/unknown", Status: "future_status", MergePolicy: "auto", Kind: "thread",
	})
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `UPDATE threads SET updated_at = ? WHERE id = 'thread-unknown'`, cutoff-1)
	require.NoError(t, err)

	selection, err := gcDelete(t.Context(), conn, q, cutoff, "")
	require.NoError(t, err)
	require.NotContains(t, selection.threadIDs, ids.ThreadOldDone)
	require.NotContains(t, selection.threadIDs, "thread-unknown")
	require.True(t, threadExists(t, dataDir, ids.ThreadOldDone))
	require.True(t, threadExists(t, dataDir, "thread-unknown"))
}

func TestGC_DeleteRollbackRestoresAllRows(t *testing.T) {
	dataDir := t.TempDir()
	cutoff := time.Now().AddDate(0, 0, -90).Unix()
	projectA, projectB := t.TempDir(), t.TempDir()
	ids := gcFixture(t, dataDir, cutoff, projectA, projectB)

	conn, err := sennitdb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := sennitdb.New(conn)
	errInjected := errors.New("injected delete failure")
	_, err = gcDeleteWith(t.Context(), conn, q, cutoff, "", func(ctx context.Context, q *sennitdb.Queries, selection gcSelection) error {
		require.NotEmpty(t, selection.sessionIDs)
		require.NoError(t, q.DeleteSessionMessages(ctx, selection.sessionIDs[0]))
		return errInjected
	})
	require.ErrorIs(t, err, errInjected)
	require.True(t, sessionExists(t, dataDir, ids.OldParent))
	messages, err := q.ListMessagesBySession(t.Context(), ids.OldParent)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.True(t, threadExists(t, dataDir, ids.ThreadOldDone))
}

// gc is the only thing that reclaims rows from the delegations table. A
// thread is otherwise removed only by merging, and a task by nothing at
// all: it is never merged and the task API has no removal of its own. So
// scoping this collection to threads meant every finished task stayed for
// the life of the database.
func TestGC_CollectsFinishedTasksToo(t *testing.T) {
	dataDir := t.TempDir()
	cutoff := time.Now().AddDate(0, 0, -90).Unix()
	projectA, projectB := t.TempDir(), t.TempDir()
	gcFixture(t, dataDir, cutoff, projectA, projectB)

	conn, err := sennitdb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := sennitdb.New(conn)

	// A task old enough to collect, and one that only just finished.
	// Tasks carry no worktree: they share their parent's app rather than
	// getting an isolated one, which is why reclaiming one is the row
	// alone with nothing left orphaned on disk.
	for _, tc := range []struct {
		id        string
		status    string
		updatedAt int64
	}{
		{"task-old-done", "completed", cutoff - 1},
		{"task-recent-done", "completed", cutoff + 1},
		{"task-old-running", "running", cutoff - 1},
	} {
		_, err = q.CreateThread(t.Context(), sennitdb.CreateThreadParams{
			ID: tc.id, Name: tc.id, ProjectPath: projectA, Goal: "goal", BaseBranch: "main",
			Branch: "", WorktreePath: "", Status: tc.status, MergePolicy: "auto", Kind: "task",
		})
		require.NoError(t, err)
		_, err = conn.ExecContext(t.Context(), `UPDATE threads SET updated_at = ? WHERE id = ?`, tc.updatedAt, tc.id)
		require.NoError(t, err)
	}

	selection, err := gcDelete(t.Context(), conn, q, cutoff, "")
	require.NoError(t, err)

	require.Contains(t, selection.threadIDs, "task-old-done",
		"a finished task past the cutoff is exactly what nothing else would ever reclaim")
	require.NotContains(t, selection.threadIDs, "task-recent-done",
		"retention applies to tasks the same way it applies to threads")
	require.NotContains(t, selection.threadIDs, "task-old-running",
		"an unfinished task is not collectable however old it is")

	require.False(t, threadExists(t, dataDir, "task-old-done"))
	require.True(t, threadExists(t, dataDir, "task-recent-done"))
	require.True(t, threadExists(t, dataDir, "task-old-running"))
}

// TestGC_ReportOnly_OrphanedWorktree covers the field, human, and JSON
// surfaces together for a single fixture run, plus the negative cases:
// an already-cleaned-up worktree, an empty worktree_path, a task, and an
// ineligible thread must never be reported.
func TestGC_ReportOnly_OrphanedWorktree(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	dataDir := config.GlobalDBDir()
	cutoff := time.Now().AddDate(0, 0, -90).Unix()
	projectA, projectB := t.TempDir(), t.TempDir()
	ids := gcFixture(t, dataDir, cutoff, projectA, projectB)

	testCmd, stdout := newGCTestCmd(t)
	setCwdFlag(t, testCmd, projectA)
	require.NoError(t, testCmd.Flags().Set("json", "true"))
	require.NoError(t, runGC(testCmd, nil))

	var report gcReport
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &report))
	require.Contains(t, report.OrphanedWorktrees, ids.WorktreeDir)
	require.Len(t, report.OrphanedWorktrees, 1)

	// ThreadOldDone's worktree_path ("/tmp/old-done" from the base
	// fixture) does not exist on disk, so it must not be reported even
	// though its row was eligible and deleted.
	require.False(t, threadExists(t, dataDir, ids.ThreadOldDone))
	// The gone-worktree and task-with-worktree rows were also deleted
	// (both are eligible), but neither contributes an orphan path.
	require.False(t, threadExists(t, dataDir, ids.WorktreeGone))
	require.False(t, threadExists(t, dataDir, ids.TaskWithWorktree))
}

// TestGC_ReportsOrphanedWorktree_HumanOutput confirms the rendered text
// names the orphaned path and tells the user how to clean it up.
func TestGC_ReportsOrphanedWorktree_HumanOutput(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	dataDir := config.GlobalDBDir()
	cutoff := time.Now().AddDate(0, 0, -90).Unix()
	projectA, projectB := t.TempDir(), t.TempDir()
	ids := gcFixture(t, dataDir, cutoff, projectA, projectB)

	testCmd, stdout := newGCTestCmd(t)
	setCwdFlag(t, testCmd, projectA)
	require.NoError(t, runGC(testCmd, nil))

	require.Contains(t, stdout.String(), ids.WorktreeDir)
	require.Contains(t, stdout.String(), "git worktree remove")
	require.NotContains(t, stdout.String(), "Would orphan") // not a dry run
}

// TestGC_IneligibleThread_WorktreeNotReported confirms a thread that fails
// selection (too recent, or non-terminal) never contributes its worktree to
// the report, and its row survives.
func TestGC_IneligibleThread_WorktreeNotReported(t *testing.T) {
	dataDir := t.TempDir()
	cutoff := time.Now().AddDate(0, 0, -90).Unix()
	projectA := t.TempDir()

	conn, err := sennitdb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := sennitdb.New(conn)

	worktreeDir := filepath.Join(t.TempDir(), "ineligible-worktree")
	require.NoError(t, os.MkdirAll(worktreeDir, 0o755))
	_, err = q.CreateThread(t.Context(), sennitdb.CreateThreadParams{
		ID: "thread-ineligible", Name: "ineligible", ProjectPath: projectA, Goal: "goal", BaseBranch: "main",
		Branch: "thread/ineligible", WorktreePath: worktreeDir, Status: "running", MergePolicy: "auto", Kind: "thread",
	})
	require.NoError(t, err)
	old := time.Unix(cutoff, 0).Add(-30 * 24 * time.Hour).Unix()
	_, err = conn.ExecContext(t.Context(), `UPDATE threads SET updated_at = ? WHERE id = ?`, old, "thread-ineligible")
	require.NoError(t, err)

	selection, err := gcCollect(t.Context(), q, conn, cutoff, "")
	require.NoError(t, err)
	require.NotContains(t, selection.threadIDs, "thread-ineligible")
	require.NotContains(t, selection.orphanedWorktrees, worktreeDir)

	require.NoError(t, sennitdb.Release(dataDir))
	require.True(t, threadExists(t, dataDir, "thread-ineligible"))
}

// TestGC_DryRun_ReportsWouldBeOrphaned exercises the dry-run wording and
// confirms nothing is touched: neither the DB row nor the worktree
// directory on disk.
func TestGC_DryRun_ReportsWouldBeOrphaned(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	dataDir := config.GlobalDBDir()
	cutoff := time.Now().AddDate(0, 0, -90).Unix()
	projectA, projectB := t.TempDir(), t.TempDir()
	ids := gcFixture(t, dataDir, cutoff, projectA, projectB)

	testCmd, stdout := newGCTestCmd(t)
	setCwdFlag(t, testCmd, projectA)
	require.NoError(t, testCmd.Flags().Set("dry-run", "true"))
	require.NoError(t, runGC(testCmd, nil))

	require.Contains(t, stdout.String(), ids.WorktreeDir)
	require.Contains(t, stdout.String(), "Would orphan")

	require.True(t, threadExists(t, dataDir, ids.WorktreeExists))
	_, err := os.Stat(ids.WorktreeDir)
	require.NoError(t, err)
}

// TestGC_NeverRemovesOrphanedWorktree pins the report-only contract: a real
// gc run deletes the owning thread row but the worktree directory it
// pointed at is still present on disk afterward.
func TestGC_NeverRemovesOrphanedWorktree(t *testing.T) {
	setupHermeticConfigEnv(t, `{}`)
	dataDir := config.GlobalDBDir()
	cutoff := time.Now().AddDate(0, 0, -90).Unix()
	projectA, projectB := t.TempDir(), t.TempDir()
	ids := gcFixture(t, dataDir, cutoff, projectA, projectB)

	testCmd, _ := newGCTestCmd(t)
	setCwdFlag(t, testCmd, projectA)
	require.NoError(t, runGC(testCmd, nil))

	require.False(t, threadExists(t, dataDir, ids.WorktreeExists))
	info, err := os.Stat(ids.WorktreeDir)
	require.NoError(t, err)
	require.True(t, info.IsDir())
}
