package cmd

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rave-soft/braid/internal/config"
	braiddb "github.com/rave-soft/braid/internal/db"
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
	Boundary     string // project A, updated_at exactly at the cutoff -- must survive (strict <)
	Recent       string // project A, updated 1h ago -- must survive
	OtherProject string // project B, updated 120d ago -- only swept with --all-projects (the default)

	ThreadOldDone    string // project A, status completed, 120d old -- must go
	ThreadOldRunning string // project A, status running, 120d old -- must survive (never touch running)
	ThreadRecentDone string // project A, status merged, 1h old -- must survive (not old enough)
}

// gcFixture opens a fresh migrated DB at dir (config.GlobalDBDir() for
// tests that exercise runGC, an arbitrary temp dir otherwise) and seeds it
// with sessions/threads exercising gc's selection rules: age filtering,
// parent/child expansion, the exact-cutoff boundary, project scoping, and
// thread status gating. cutoff is the unix timestamp gc will use as its
// retention boundary (time.Now() minus the retention window); the fixture
// stamps Boundary's updated_at to exactly that value. projectA/projectB
// must be real directories: runGC's --cwd resolution chdirs into them.
func gcFixture(t *testing.T, dir string, cutoff int64, projectA, projectB string) gcFixtureIDs {
	t.Helper()
	dataDir := dir
	if dataDir == "" {
		dataDir = t.TempDir()
	}
	t.Cleanup(func() {
		require.NoError(t, braiddb.Release(dataDir))
		braiddb.ResetPool()
	})

	conn, err := braiddb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := braiddb.New(conn)
	ctx := t.Context()

	// sessions/threads each carry an AFTER UPDATE trigger that unconditionally
	// stamps updated_at to strftime('now') on every UPDATE -- including the
	// row's own backdating UPDATE below, and including the cascading UPDATE
	// CreateMessage triggers via update_session_message_count_on_insert. That
	// is correct production behavior (any activity bumps last-activity time),
	// but it means no UPDATE can ever set an arbitrary past updated_at, which
	// is exactly what this fixture needs to simulate old, untouched sessions.
	// Dropping the triggers for the fixture's own connection sidesteps that;
	// gc itself never updates session/thread rows (only deletes them), so
	// this does not weaken what's under test.
	_, err = conn.ExecContext(ctx, `DROP TRIGGER IF EXISTS update_sessions_updated_at`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `DROP TRIGGER IF EXISTS update_threads_updated_at`)
	require.NoError(t, err)

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
		params := braiddb.CreateSessionParams{ID: id, Title: title, ProjectPath: projectPath}
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
	_, err = q.CreateMessage(ctx, braiddb.CreateMessageParams{
		ID: "msg-old-parent-1", SessionID: ids.OldParent, Role: "user", Parts: "[]",
	})
	require.NoError(t, err)
	_, err = q.CreateFile(ctx, braiddb.CreateFileParams{
		ID: "file-old-parent-1", SessionID: ids.OldParent, Path: "main.go", Content: "package main", Version: 0,
	})
	require.NoError(t, err)
	require.NoError(t, q.RecordFileRead(ctx, braiddb.RecordFileReadParams{
		SessionID: ids.OldParent, Path: "main.go",
	}))

	mustThread := func(id, name, projectPath string) {
		_, err := q.CreateThread(ctx, braiddb.CreateThreadParams{
			ID: id, Name: name, ProjectPath: projectPath, Goal: "goal", BaseBranch: "main",
			Branch: "thread/" + name, WorktreePath: "/tmp/" + name, Status: "pending", MergePolicy: "auto",
		})
		require.NoError(t, err)
	}
	mustThread(ids.ThreadOldDone, "old-done", projectA)
	mustThread(ids.ThreadOldRunning, "old-running", projectA)
	mustThread(ids.ThreadRecentDone, "recent-done", projectA)

	// Backdating writes go last: both sessions.updated_at and
	// threads.updated_at are stamped to "now" by an AFTER UPDATE trigger
	// on every UPDATE to that row (by design, so real usage always
	// reflects last activity), so any of these rows getting touched again
	// after this point would silently undo the backdating.
	setSessionTime(ids.OldParent, old)
	setSessionTime(ids.OldChild, recent)
	setSessionTime(ids.YoungParent, recent)
	setSessionTime(ids.OldOrphan, old)
	setSessionTime(ids.Boundary, cutoff)
	setSessionTime(ids.Recent, recent)
	setSessionTime(ids.OtherProject, old)
	setThread(ids.ThreadOldDone, "completed", old)
	setThread(ids.ThreadOldRunning, "running", old)
	setThread(ids.ThreadRecentDone, "merged", recent)

	return ids
}

func sessionExists(t *testing.T, dataDir, id string) bool {
	t.Helper()
	conn, err := braiddb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	defer braiddb.Release(dataDir) //nolint:errcheck
	q := braiddb.New(conn)
	_, err = q.GetSessionByID(t.Context(), id)
	if err == sql.ErrNoRows {
		return false
	}
	require.NoError(t, err)
	return true
}

func threadExists(t *testing.T, dataDir, id string) bool {
	t.Helper()
	conn, err := braiddb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	defer braiddb.Release(dataDir) //nolint:errcheck
	q := braiddb.New(conn)
	_, err = q.GetThread(t.Context(), id)
	if err == sql.ErrNoRows {
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
	ids := gcFixture(t, dataDir, cutoff, projectA, projectB)

	testCmd, stdout := newGCTestCmd(t)
	require.NoError(t, testCmd.Flags().Set("cwd", projectA))
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
	conn, err := braiddb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	defer braiddb.Release(dataDir) //nolint:errcheck
	q := braiddb.New(conn)
	n, err := q.CountSessionMessages(t.Context(), ids.OldParent)
	require.NoError(t, err)
	require.Zero(t, n)

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
	require.NoError(t, testCmd.Flags().Set("cwd", projectA))
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

	dbPath := filepath.Join(dataDir, "braid.db")
	before, err := os.Stat(dbPath)
	require.NoError(t, err)

	testCmd, stdout := newGCTestCmd(t)
	require.NoError(t, testCmd.Flags().Set("cwd", projectA))
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
	require.NoError(t, testCmd.Flags().Set("cwd", projectA))
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
	require.NoError(t, testCmd.Flags().Set("cwd", projectA))
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
	conn, err := braiddb.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := braiddb.New(conn)
	old := time.Unix(cutoff, 0).Add(-30 * 24 * time.Hour).Unix()
	for i := range 200 {
		id := "sess-pad-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		_, err := q.CreateSession(t.Context(), braiddb.CreateSessionParams{ID: id, Title: "pad", ProjectPath: projectA})
		require.NoError(t, err)
		_, err = conn.ExecContext(t.Context(), `UPDATE sessions SET updated_at = ? WHERE id = ?`, old, id)
		require.NoError(t, err)
		_, err = q.CreateFile(t.Context(), braiddb.CreateFileParams{
			ID: id + "-f", SessionID: id, Path: "p", Content: string(make([]byte, 8192)), Version: 0,
		})
		require.NoError(t, err)
	}
	require.NoError(t, braiddb.Release(dataDir))

	dbPath := filepath.Join(dataDir, "braid.db")
	before, err := os.Stat(dbPath)
	require.NoError(t, err)

	testCmd, _ := newGCTestCmd(t)
	require.NoError(t, testCmd.Flags().Set("cwd", projectA))
	require.NoError(t, runGC(testCmd, nil))

	after, err := os.Stat(dbPath)
	require.NoError(t, err)
	require.Less(t, after.Size(), before.Size())
}
