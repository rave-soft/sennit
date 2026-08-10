package history

import (
	"sync"
	"testing"

	"github.com/rave-soft/braid/internal/db"
	"github.com/rave-soft/braid/internal/session"
	"github.com/stretchr/testify/require"
)

// newTestService sets up an isolated on-disk SQLite DB (with migrations) and
// a session to attach files to, mirroring the pattern used in
// internal/session/session_test.go.
func newTestService(t *testing.T) (Service, string) {
	t.Helper()

	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	sessions := session.NewService(db.New(conn), conn, dataDir)
	sess, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)

	return NewService(db.New(conn), conn), sess.ID
}

func TestCreateVersionSequentialVersions(t *testing.T) {
	files, sessionID := newTestService(t)

	first, err := files.CreateVersion(t.Context(), sessionID, "foo.go", "v0")
	require.NoError(t, err)
	require.Equal(t, int64(0), first.Version)

	second, err := files.CreateVersion(t.Context(), sessionID, "foo.go", "v1")
	require.NoError(t, err)
	require.Equal(t, int64(1), second.Version)
}

// TestCreateVersionConcurrent creates many versions of the same path from
// concurrent goroutines and asserts every call succeeds and the resulting
// versions form a contiguous, gap-free, duplicate-free sequence. This
// guards against the historical TOCTOU bug where the next version was
// computed outside the insert transaction.
func TestCreateVersionConcurrent(t *testing.T) {
	files, sessionID := newTestService(t)

	const n = 20
	var wg sync.WaitGroup
	versions := make([]int64, n)
	errs := make([]error, n)

	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f, err := files.CreateVersion(t.Context(), sessionID, "concurrent.go", "content")
			errs[i] = err
			if err == nil {
				versions[i] = f.Version
			}
		}(i)
	}
	wg.Wait()

	seen := make(map[int64]bool, n)
	for i, err := range errs {
		require.NoError(t, err)
		require.False(t, seen[versions[i]], "duplicate version %d", versions[i])
		seen[versions[i]] = true
	}
	for v := range int64(n) {
		require.True(t, seen[v], "missing version %d", v)
	}
}
