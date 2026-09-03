package filetracker

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rave-soft/sennit/internal/db"
	"github.com/stretchr/testify/require"
)

type testEnv struct {
	ctx context.Context
	q   *db.Queries
	svc Service
}

func setupTest(t *testing.T) *testEnv {
	t.Helper()
	return setupTestWithWorkingDir(t, "/")
}

func setupTestWithWorkingDir(t *testing.T, workingDir string) *testEnv {
	t.Helper()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	q := db.New(conn)
	return &testEnv{
		ctx: t.Context(),
		q:   q,
		svc: NewService(q, workingDir),
	}
}

func (e *testEnv) createSession(t *testing.T, sessionID string) {
	t.Helper()
	_, err := e.q.CreateSession(e.ctx, db.CreateSessionParams{
		ID:    sessionID,
		Title: "Test Session",
	})
	require.NoError(t, err)
}

func TestService_ConcurrentCoverageUpdatesAcrossConnections(t *testing.T) {
	dataDir := t.TempDir()
	setup, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := db.New(setup)
	_, err = q.CreateSession(t.Context(), db.CreateSessionParams{ID: "concurrent", Title: "Concurrent"})
	require.NoError(t, err)
	require.NoError(t, db.Release(dataDir))

	for iteration := range 50 {
		first, err := db.OpenDB(t.Context(), filepath.Join(dataDir, "sennit.db"))
		require.NoError(t, err)
		second, err := db.OpenDB(t.Context(), filepath.Join(dataDir, "sennit.db"))
		require.NoError(t, err)

		firstQueries := db.New(first)
		firstService := NewService(firstQueries, "/")
		secondService := NewService(db.New(second), "/")
		require.NoError(t, firstQueries.RecordFileRead(t.Context(), db.RecordFileReadParams{
			SessionID:  "concurrent",
			Path:       "file.go",
			ReadRanges: "[[201,210]]",
		}))

		ready := make(chan struct{}, 2)
		start := make(chan struct{})
		var wait sync.WaitGroup
		wait.Add(2)
		go func() {
			defer wait.Done()
			ready <- struct{}{}
			<-start
			firstService.RecordPartialRead(t.Context(), "concurrent", "/file.go", 200, 210)
		}()
		go func() {
			defer wait.Done()
			ready <- struct{}{}
			<-start
			secondService.RecordEdit(t.Context(), "concurrent", "/file.go", 100, 100, 101)
		}()
		<-ready
		<-ready
		close(start)
		wait.Wait()

		coverage := firstService.ReadCoverage(t.Context(), "concurrent", "/file.go")
		allowed := []Coverage{
			{Ranges: []LineRange{{Start: 100, End: 101}, {Start: 201, End: 211}}},
			{Ranges: []LineRange{{Start: 100, End: 101}, {Start: 200, End: 211}}},
		}
		require.Containsf(t, allowed, coverage, "iteration %d: non-canonical coverage", iteration)
		require.NoError(t, first.Close())
		require.NoError(t, second.Close())
	}
}

func TestService_RecordRead(t *testing.T) {
	env := setupTest(t)

	sessionID := "test-session-1"
	path := "/path/to/file.go"
	env.createSession(t, sessionID)

	env.svc.RecordRead(env.ctx, sessionID, path)

	lastRead := env.svc.LastReadTime(env.ctx, sessionID, path)
	require.False(t, lastRead.IsZero(), "expected non-zero time after recording read")
	require.WithinDuration(t, time.Now(), lastRead, 2*time.Second)
}

func TestService_PathAliasesShareCoverage(t *testing.T) {
	workingDir := t.TempDir()
	realDir := filepath.Join(workingDir, "real")
	require.NoError(t, os.Mkdir(realDir, 0o755))
	aliasDir := filepath.Join(workingDir, "alias")
	require.NoError(t, os.Symlink(realDir, aliasDir))

	env := setupTestWithWorkingDir(t, workingDir)
	env.createSession(t, "aliases")
	realPath := filepath.Join(realDir, "file.go")
	require.NoError(t, os.WriteFile(realPath, []byte("one\ntwo\n"), 0o644))
	aliasPath := filepath.Join(aliasDir, "file.go")

	env.svc.RecordRead(env.ctx, "aliases", aliasPath)
	require.Equal(t, FullCoverage, env.svc.ReadCoverage(env.ctx, "aliases", realPath))

	files, err := env.svc.ListReadFiles(env.ctx, "aliases")
	require.NoError(t, err)
	require.Equal(t, []string{realPath}, files)
}

func TestService_LastReadTime_NotFound(t *testing.T) {
	env := setupTest(t)

	lastRead := env.svc.LastReadTime(env.ctx, "nonexistent-session", "/nonexistent/path")
	require.True(t, lastRead.IsZero(), "expected zero time for unread file")
}

func TestService_RecordRead_UpdatesTimestamp(t *testing.T) {
	env := setupTest(t)

	sessionID := "test-session-2"
	path := "/path/to/file.go"
	env.createSession(t, sessionID)

	env.svc.RecordRead(env.ctx, sessionID, path)
	firstRead := env.svc.LastReadTime(env.ctx, sessionID, path)
	require.False(t, firstRead.IsZero())

	synctest.Test(t, func(t *testing.T) {
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		env.svc.RecordRead(env.ctx, sessionID, path)
		secondRead := env.svc.LastReadTime(env.ctx, sessionID, path)

		require.False(t, secondRead.Before(firstRead), "second read time should not be before first")
	})
}

func TestService_RecordRead_DifferentSessions(t *testing.T) {
	env := setupTest(t)

	path := "/shared/file.go"
	session1, session2 := "session-1", "session-2"
	env.createSession(t, session1)
	env.createSession(t, session2)

	env.svc.RecordRead(env.ctx, session1, path)

	lastRead1 := env.svc.LastReadTime(env.ctx, session1, path)
	require.False(t, lastRead1.IsZero())

	lastRead2 := env.svc.LastReadTime(env.ctx, session2, path)
	require.True(t, lastRead2.IsZero(), "session 2 should not see session 1's read")
}

func TestService_RecordRead_DifferentPaths(t *testing.T) {
	env := setupTest(t)

	sessionID := "test-session-3"
	path1, path2 := "/path/to/file1.go", "/path/to/file2.go"
	env.createSession(t, sessionID)

	env.svc.RecordRead(env.ctx, sessionID, path1)

	lastRead1 := env.svc.LastReadTime(env.ctx, sessionID, path1)
	require.False(t, lastRead1.IsZero())

	lastRead2 := env.svc.LastReadTime(env.ctx, sessionID, path2)
	require.True(t, lastRead2.IsZero(), "path2 should not be recorded")
}

// TestService_ListReadFiles_OrdersMostRecentFirst guards read_at's
// millisecond resolution: two reads landing in the same wall-clock second
// but different milliseconds must still come back most-recent-first. The
// database's own clock is real wall time (SQLite's julianday('now')), so
// this sleeps for real rather than using synctest's fake clock.
func TestService_ListReadFiles_OrdersMostRecentFirst(t *testing.T) {
	env := setupTest(t)
	sessionID := "order-session"
	env.createSession(t, sessionID)
	path1, path2 := "/path/to/file1.go", "/path/to/file2.go"

	env.svc.RecordRead(env.ctx, sessionID, path1)
	time.Sleep(20 * time.Millisecond)
	env.svc.RecordRead(env.ctx, sessionID, path2)

	files, err := env.svc.ListReadFiles(env.ctx, sessionID)
	require.NoError(t, err)
	// ListReadFiles returns paths joined against the working directory,
	// so the expectation is built the same way rather than from slash
	// literals that only match on one platform.
	require.Equal(t, []string{filepath.FromSlash(path2), filepath.FromSlash(path1)}, files, "the more recently read file should sort first")
}

// TestService_UsesInjectedWorkingDir_NotProcessCwd guards against a
// regression where paths were resolved against the process's os.Getwd()
// instead of the workspace's working directory. In server mode the
// process cwd need not match the workspace being served, so the service
// must use the injected workingDir for both writes and reads.
func TestService_UsesInjectedWorkingDir_NotProcessCwd(t *testing.T) {
	workspaceDir := filepath.Join(string(filepath.Separator), "workspace", "project")
	env := setupTestWithWorkingDir(t, workspaceDir)

	processCwd, err := os.Getwd()
	require.NoError(t, err)
	require.NotEqual(t, workspaceDir, processCwd, "test workspace dir must differ from process cwd")

	sessionID := "test-session-workdir"
	env.createSession(t, sessionID)

	path := filepath.Join(workspaceDir, "pkg", "file.go")
	env.svc.RecordRead(env.ctx, sessionID, path)

	files, err := env.svc.ListReadFiles(env.ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, []string{path}, files, "path should resolve against the injected workspace dir, not process cwd")
}

// TestService_RecordPartialRead_NoPriorRow guards against treating a file
// this session has never touched as fully read. Before the update
// callback distinguished "no row" from "row holding the empty string",
// the first partial read on a fresh file computed coverage from
// FullCoverage and stored "" — marking the whole file read from a single
// windowed read.
func TestService_RecordPartialRead_NoPriorRow(t *testing.T) {
	env := setupTest(t)
	env.createSession(t, "s1")

	env.svc.RecordPartialRead(env.ctx, "s1", "/f.go", 1, 500)

	cov := env.svc.ReadCoverage(env.ctx, "s1", "/f.go")
	require.False(t, cov.Full, "a windowed read of a fresh file must not mark it fully covered")
	require.True(t, cov.Covers(1, 500), "the served window must be covered")
	require.False(t, cov.Covers(2800, 2800), "lines outside the served window must not be covered")
}

// TestService_RecordEdit_NoPriorRow mirrors the partial-read case for
// edits: internal/agent/tools/lsp_replace_symbol.go relies on RecordEdit
// not widening coverage to the whole file when there is no row yet.
func TestService_RecordEdit_NoPriorRow(t *testing.T) {
	env := setupTest(t)
	env.createSession(t, "s2")

	env.svc.RecordEdit(env.ctx, "s2", "/g.go", 10, 12, 14)

	cov := env.svc.ReadCoverage(env.ctx, "s2", "/g.go")
	require.False(t, cov.Full, "an edit on a fresh file must not mark it fully covered")
	require.True(t, cov.Covers(10, 14), "the edited span must be covered")
}

// TestService_LegacyEmptyRangesRow_ReadsAsFullCoverage guards the other
// half of the encoding: a row that already holds read_ranges = "" — as
// every row written before range tracking existed does — must still read
// back as fully covered.
func TestService_LegacyEmptyRangesRow_ReadsAsFullCoverage(t *testing.T) {
	env := setupTest(t)
	env.createSession(t, "s3")

	require.NoError(t, env.q.RecordFileRead(env.ctx, db.RecordFileReadParams{
		SessionID:  "s3",
		Path:       "legacy.go",
		ReadRanges: "",
	}))

	cov := env.svc.ReadCoverage(env.ctx, "s3", "/legacy.go")
	require.True(t, cov.Full, "a pre-existing row with read_ranges = \"\" must read back as fully covered")
}

// TestService_RecordRead_YieldsFullCoverage is the genuine whole-file
// read counterpart to the two "no prior row" cases above: it must still
// produce full coverage.
func TestService_RecordRead_YieldsFullCoverage(t *testing.T) {
	env := setupTest(t)
	env.createSession(t, "s4")

	env.svc.RecordRead(env.ctx, "s4", "/h.go")

	cov := env.svc.ReadCoverage(env.ctx, "s4", "/h.go")
	require.True(t, cov.Full, "RecordRead must yield full coverage")
}
