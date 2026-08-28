package history

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/pubsub"
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

// receiveOrContext waits for a test phase to complete or fails when the test's
// overall watchdog context expires.
func receiveOrContext[T any](t *testing.T, ctx context.Context, ch <-chan T) T {
	t.Helper()

	select {
	case value := <-ch:
		return value
	case <-ctx.Done():
		t.Fatalf("test context expired while waiting for phase: %v", ctx.Err())
		var zero T
		return zero
	}
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

type serializedVersionStore struct {
	permit       chan struct{}
	beginAttempt chan struct{}
	outsideRead  chan struct{}
	firstRead    chan struct{}
	releaseFirst chan struct{}

	mu       sync.Mutex
	files    []db.File
	beginSeq int
}

func newSerializedVersionStore() *serializedVersionStore {
	store := &serializedVersionStore{
		permit:       make(chan struct{}, 1),
		beginAttempt: make(chan struct{}, 2),
		outsideRead:  make(chan struct{}, 2),
		firstRead:    make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	store.permit <- struct{}{}
	return store
}

func (s *serializedVersionStore) NextFileVersion(_ context.Context, path string) (int64, error) {
	s.outsideRead <- struct{}{}
	return s.nextFileVersion(path), nil
}

func (s *serializedVersionStore) Begin(ctx context.Context) (fileVersionTransaction, error) {
	s.beginAttempt <- struct{}{}
	select {
	case <-s.permit:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	s.mu.Lock()
	sequence := s.beginSeq
	s.beginSeq++
	s.mu.Unlock()
	return &serializedVersionTransaction{store: s, sequence: sequence}, nil
}

type serializedVersionTransaction struct {
	store    *serializedVersionStore
	sequence int
	pending  *db.File
	closed   bool
}

func (s *serializedVersionStore) nextFileVersion(path string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := int64(0)
	for _, file := range s.files {
		if file.Path == path && file.Version >= next {
			next = file.Version + 1
		}
	}
	return next
}

func (tx *serializedVersionTransaction) NextFileVersion(ctx context.Context, path string) (int64, error) {
	next := tx.store.nextFileVersion(path)
	if tx.sequence == 0 {
		close(tx.store.firstRead)
		select {
		case <-tx.store.releaseFirst:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return next, nil
}

func (tx *serializedVersionTransaction) CreateFile(_ context.Context, params db.CreateFileParams) (db.File, error) {
	file := db.File{
		ID:        params.ID,
		SessionID: params.SessionID,
		Path:      params.Path,
		Content:   params.Content,
		Version:   params.Version,
	}
	tx.pending = &file
	return file, nil
}

func (tx *serializedVersionTransaction) Commit() error {
	if tx.pending != nil {
		tx.store.mu.Lock()
		tx.store.files = append(tx.store.files, *tx.pending)
		tx.store.mu.Unlock()
	}
	tx.close()
	return nil
}

func (tx *serializedVersionTransaction) Rollback() error {
	tx.close()
	return nil
}

func (tx *serializedVersionTransaction) close() {
	if tx.closed {
		return
	}
	tx.closed = true
	tx.store.permit <- struct{}{}
}

// TestCreateVersionAllocatesInsideSerializedTransaction forces a second call
// to attempt its transaction while the first is paused after reading. The
// store models BEGIN IMMEDIATE ownership, making every ordering edge an
// explicit channel handshake rather than a scheduler timing assumption.
func TestCreateVersionAllocatesInsideSerializedTransaction(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	store := newSerializedVersionStore()
	files := &service{Broker: pubsub.NewBroker[File](), versions: store}

	type result struct {
		file File
		err  error
	}
	firstResult := make(chan result, 1)
	secondResult := make(chan result, 1)
	go func() {
		file, err := files.CreateVersion(ctx, "first-session", "serialized.go", "first")
		firstResult <- result{file: file, err: err}
	}()

	select {
	case <-store.beginAttempt:
	case <-store.outsideRead:
		t.Fatal("version was read before opening its transaction")
	case <-ctx.Done():
		t.Fatalf("test context expired while waiting for first operation: %v", ctx.Err())
	}
	receiveOrContext(t, ctx, store.firstRead)

	go func() {
		file, err := files.CreateVersion(ctx, "second-session", "serialized.go", "second")
		secondResult <- result{file: file, err: err}
	}()
	receiveOrContext(t, ctx, store.beginAttempt)

	close(store.releaseFirst)
	first := receiveOrContext(t, ctx, firstResult)
	second := receiveOrContext(t, ctx, secondResult)

	require.NoError(t, first.err)
	require.NoError(t, second.err)
	require.Equal(t, int64(0), first.file.Version)
	require.Equal(t, int64(1), second.file.Version)
	require.Equal(t, "first", first.file.Content)
	require.Equal(t, "second", second.file.Content)

	store.mu.Lock()
	persisted := append([]db.File(nil), store.files...)
	store.mu.Unlock()
	require.Len(t, persisted, 2)
	require.Equal(t, []int64{0, 1}, []int64{persisted[0].Version, persisted[1].Version})
}

// TestCreateVersionConcurrent stress-tests version allocation through
// independent SQLite connections and verifies all resulting content persists.
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
	for _, file := range deleted {
		require.NoError(t, files.Delete(t.Context(), file.ID))
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

// TestListLatestSessionFilesIgnoresOtherSessions covers the case the old
// query got wrong: version numbers run globally per path, so taking the
// maximum across the whole table matched a sibling session's newer row
// and dropped this session's file out of the result entirely.
func TestListLatestSessionFilesIgnoresOtherSessions(t *testing.T) {
	files, sessions, sessionID, _ := newTestService(t)
	other, err := sessions.CreateTaskSession(t.Context(), "other", sessionID, "other")
	require.NoError(t, err)

	_, err = files.CreateVersion(t.Context(), sessionID, "shared.go", "mine v0")
	require.NoError(t, err)
	mine, err := files.CreateVersion(t.Context(), sessionID, "shared.go", "mine v1")
	require.NoError(t, err)

	// The other session then writes a strictly newer version of the same
	// path, which used to hide this session's own latest version.
	theirs, err := files.CreateVersion(t.Context(), other.ID, "shared.go", "theirs")
	require.NoError(t, err)
	require.Greater(t, theirs.Version, mine.Version)

	latest, err := files.ListLatestSessionFiles(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, latest, 1)
	require.Equal(t, mine.ID, latest[0].ID)
	require.Equal(t, "mine v1", latest[0].Content)

	otherLatest, err := files.ListLatestSessionFiles(t.Context(), other.ID)
	require.NoError(t, err)
	require.Len(t, otherLatest, 1)
	require.Equal(t, theirs.ID, otherLatest[0].ID)
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
