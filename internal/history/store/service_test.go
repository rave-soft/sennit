package store

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/rave-soft/sennit/internal/db"
	sessionstore "github.com/rave-soft/sennit/internal/session/store"
	"github.com/stretchr/testify/require"
)

// newTestService sets up an isolated on-disk SQLite DB (with migrations) and
// a session to attach files to, mirroring the pattern used in
// internal/session/session_test.go.
func newTestService(t *testing.T) (Service, sessionstore.Service, string, string) {
	t.Helper()

	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	sessions := sessionstore.NewService(db.New(conn), conn, dataDir)
	sess, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)

	return NewService(db.New(conn), conn), sessions, sess.ID, dataDir
}

func TestCreateVersionSequentialVersions(t *testing.T) {
	files, _, sessionID, _ := newTestService(t)

	first, err := files.CreateVersion(t.Context(), sessionID, "foo.go", "v0")
	require.NoError(t, err)
	require.Equal(t, int64(0), first.Version)

	second, err := files.CreateVersion(t.Context(), sessionID, "foo.go", "v1")
	require.NoError(t, err)
	require.Equal(t, int64(1), second.Version)
}

// TestCreateVersionConcurrent stress-tests version allocation through
// independent SQLite connections and verifies all resulting content persists.
// It runs against a real on-disk SQLite database (via newTestService), so it
// is what proves version numbers form a proper unbroken sequence under
// concurrent CreateVersion calls, without duplicates.
func TestCreateVersionConcurrent(t *testing.T) {
	files, sessions, sessionID, dataDir := newTestService(t)
	secondSession, err := sessions.CreateTaskSession(t.Context(), "concurrent-child", sessionID, "test")
	require.NoError(t, err)

	const n = 20
	services := make([]Service, n)
	services[0] = files
	for i := 1; i < n; i++ {
		conn, openErr := db.OpenDB(t.Context(), filepath.Join(dataDir, "sennit.db"))
		require.NoError(t, openErr)
		t.Cleanup(func() { require.NoError(t, conn.Close()) })
		services[i] = NewService(db.New(conn), conn)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	versions := make([]int64, n)
	contents := make([]string, n)
	errs := make([]error, n)

	for i := range n {
		wg.Go(func() {
			<-start
			content := fmt.Sprintf("content-%d", i)
			id := sessionID
			if i%2 != 0 {
				id = secondSession.ID
			}
			f, createErr := services[i].CreateVersion(t.Context(), id, "concurrent.go", content)
			errs[i] = createErr
			if createErr == nil {
				versions[i] = f.Version
				contents[i] = f.Content
			}
		})
	}
	close(start)
	wg.Wait()

	seenVersions := make(map[int64]bool, n)
	seenContents := make(map[string]bool, n)
	for i, createErr := range errs {
		require.NoError(t, createErr)
		require.False(t, seenVersions[versions[i]], "duplicate version %d", versions[i])
		seenVersions[versions[i]] = true
		require.False(t, seenContents[contents[i]], "lost content %q", contents[i])
		seenContents[contents[i]] = true
	}
	for v := range int64(n) {
		require.True(t, seenVersions[v], "missing version %d", v)
	}

	persisted, err := files.ListBySessionTree(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, persisted, n)
	persistedVersions := make(map[int64]bool, n)
	persistedContents := make(map[string]bool, n)
	for _, file := range persisted {
		persistedVersions[file.Version] = true
		persistedContents[file.Content] = true
	}
	require.Equal(t, seenVersions, persistedVersions)
	require.Equal(t, seenContents, persistedContents)

	deleted, err := files.ListBySessionTree(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, deleted, n)
	cleanupConn, err := db.OpenDB(t.Context(), filepath.Join(dataDir, "sennit.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cleanupConn.Close()) })
	q := db.New(cleanupConn)
	for _, file := range deleted {
		require.NoError(t, q.DeleteFile(t.Context(), file.ID))
	}

	const secondRound = 10
	var wg2 sync.WaitGroup
	start2 := make(chan struct{})
	secondVersions := make([]int64, secondRound)
	secondErrs := make([]error, secondRound)
	for i := range secondRound {
		wg2.Go(func() {
			<-start2
			content := fmt.Sprintf("second-round-%d", i)
			f, createErr := services[i].CreateVersion(t.Context(), sessionID, "concurrent.go", content)
			secondErrs[i] = createErr
			if createErr == nil {
				secondVersions[i] = f.Version
			}
		})
	}
	close(start2)
	wg2.Wait()

	for _, createErr := range secondErrs {
		require.NoError(t, createErr)
	}
	secondSeen := make(map[int64]bool, secondRound)
	for _, v := range secondVersions {
		require.False(t, secondSeen[v], "duplicate second-round version %d", v)
		secondSeen[v] = true
	}
	require.Len(t, secondSeen, secondRound)

	resurrected, err := files.ListBySessionTree(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, resurrected, secondRound)
	resurrectedVersions := make(map[int64]bool, secondRound)
	resurrectedContents := make(map[string]bool, secondRound)
	for _, file := range resurrected {
		resurrectedVersions[file.Version] = true
		resurrectedContents[file.Content] = true
	}
	require.Equal(t, secondSeen, resurrectedVersions)
	for i := range secondRound {
		require.True(t, resurrectedContents[fmt.Sprintf("second-round-%d", i)], "lost second-round content")
	}
}

func TestListBySessionTreeSharesFilesAcrossAgents(t *testing.T) {
	files, sessions, rootID, _ := newTestService(t)
	child, err := sessions.CreateTaskSession(t.Context(), "child", rootID, "child")
	require.NoError(t, err)
	sibling, err := sessions.CreateTaskSession(t.Context(), "sibling", rootID, "sibling")
	require.NoError(t, err)
	nested, err := sessions.CreateTaskSession(t.Context(), "nested", child.ID, "nested")
	require.NoError(t, err)

	_, err = files.CreateVersion(t.Context(), rootID, "root.go", "root")
	require.NoError(t, err)
	_, err = files.CreateVersion(t.Context(), child.ID, "child.go", "child")
	require.NoError(t, err)
	_, err = files.CreateVersion(t.Context(), nested.ID, "nested.go", "nested")
	require.NoError(t, err)

	for _, sessionID := range []string{rootID, child.ID, sibling.ID, nested.ID} {
		treeFiles, listErr := files.ListBySessionTree(t.Context(), sessionID)
		require.NoError(t, listErr)
		paths := make([]string, len(treeFiles))
		for i, file := range treeFiles {
			paths[i] = file.Path
		}
		require.ElementsMatch(t, []string{"root.go", "child.go", "nested.go"}, paths)
	}
}

// TestCreateVersionNumbersAcrossSessions pins that one path's versions
// form a single sequence no matter which session records them, which is
// what lets the UI diff a file's first version against its latest across
// a whole session tree.
func TestCreateVersionNumbersAcrossSessions(t *testing.T) {
	files, sessions, sessionID, _ := newTestService(t)
	other, err := sessions.CreateTaskSession(t.Context(), "other", sessionID, "other")
	require.NoError(t, err)

	first, err := files.CreateVersion(t.Context(), sessionID, "shared.go", "v0")
	require.NoError(t, err)
	second, err := files.CreateVersion(t.Context(), other.ID, "shared.go", "v1")
	require.NoError(t, err)
	third, err := files.CreateVersion(t.Context(), sessionID, "shared.go", "v2")
	require.NoError(t, err)

	require.Equal(t, int64(0), first.Version)
	require.Equal(t, int64(1), second.Version)
	require.Equal(t, int64(2), third.Version)
}
