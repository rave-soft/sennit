package thread

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// store scaffolding for tests, exported for the same reason
// [NewTaskManagerFromManager] is: it names sqlc's db.Querier/db.Thread and
// builds a real Store around them, which only makes sense from within this
// package, but every test that wants a real store — this package's own
// (store_test.go) and every other package's (package thread_test, and
// beyond) — needs it. It imports none of app/threadspawn, so it carries no
// risk of the test import cycle the app-referencing fakes must avoid (see
// fakes_test.go, package thread_test).
// ---------------------------------------------------------------------------

// NewStoreForTest builds a real (sqlite-backed) Store using the same sqlc
// queries threadspawn.NewStore uses, scoped to a throwaway project path —
// for tests that want a real store without threadspawn's global-DB-dir
// dependency (threadspawn.NewStore cannot be imported here either way: it
// would be the same test import cycle NewTaskManagerFromManager's doc comment
// describes).
//
// Cleanup releases only this call's own dataDir, deliberately not
// db.ResetPool(): the pool is one process-wide map, and a test that builds
// more than one store (or a real message/session service alongside one, as
// several in this package do) has more than one live entry in it at once.
// ResetPool nukes every entry regardless of owner, so calling it here would
// yank connections a sibling helper's own, not-yet-run cleanup still
// legitimately holds — the exact "release closes it out from under another
// holder" hazard db.Release itself now refuses to do silently (see
// TECHDEBT.md / internal/db).
func NewStoreForTest(t testing.TB) Store {
	t.Helper()
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
	})
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	return &testStoreDB{q: db.New(conn), projectPath: dataDir}
}

// testStoreDB is a minimal Store over the sqlc queries, mirroring the
// threadspawn implementation (which cannot be imported here).
type testStoreDB struct {
	q           db.Querier
	projectPath string
}

var _ Store = (*testStoreDB)(nil)

func (s *testStoreDB) Create(ctx context.Context, params CreateParams) (Thread, error) {
	kind := params.Kind
	if kind == "" {
		kind = KindThread
	}
	mergePolicy := params.MergePolicy
	if mergePolicy == "" && kind == KindThread {
		mergePolicy = MergeAuto
	}
	dbThread, err := s.q.CreateThread(ctx, db.CreateThreadParams{
		// A time-derived ID (fmt.Sprintf("thread-%d", time.Now().UnixNano()))
		// used to sit here; on a coarse wall clock — Windows' default
		// granularity is ~15.6ms — two Creates in the same test can land in
		// the same tick and collide on the threads.id UNIQUE constraint
		// (TestStore_List did, in CI). uuid.New() matches what the real
		// store (threadspawn.store.Create) already uses and carries no
		// clock dependency.
		ID:              uuid.New().String(),
		Name:            params.Name,
		ProjectPath:     s.projectPath,
		Goal:            params.Goal,
		BaseBranch:      params.BaseBranch,
		Branch:          params.Branch,
		WorktreePath:    params.WorktreePath,
		SessionID:       params.SessionID,
		Status:          string(StatusPending),
		MergePolicy:     string(mergePolicy),
		Kind:            string(kind),
		ParentSessionID: params.ParentSessionID,
	})
	if err != nil {
		return Thread{}, err
	}
	return testFromDBItem(dbThread), nil
}

func (s *testStoreDB) Get(ctx context.Context, id string) (Thread, error) {
	dbThread, err := s.q.GetThread(ctx, id)
	if err != nil {
		return Thread{}, err
	}
	return testFromDBItem(dbThread), nil
}

func (s *testStoreDB) GetByName(ctx context.Context, name string) (Thread, error) {
	dbThread, err := s.q.GetThreadByName(ctx, db.GetThreadByNameParams{
		Name:        name,
		ProjectPath: s.projectPath,
	})
	if err != nil {
		return Thread{}, err
	}
	return testFromDBItem(dbThread), nil
}

func (s *testStoreDB) List(ctx context.Context) ([]Thread, error) {
	rows, err := s.q.ListThreads(ctx, s.projectPath)
	if err != nil {
		return nil, err
	}
	out := make([]Thread, len(rows))
	for i, r := range rows {
		out[i] = testFromDBItem(r)
	}
	return out, nil
}

func (s *testStoreDB) ListAll(ctx context.Context) ([]Thread, error) {
	rows, err := s.q.ListThreadsAll(ctx, s.projectPath)
	if err != nil {
		return nil, err
	}
	out := make([]Thread, len(rows))
	for i, r := range rows {
		out[i] = testFromDBItem(r)
	}
	return out, nil
}

func (s *testStoreDB) SetStatus(ctx context.Context, id string, params SetStatusParams) (Thread, error) {
	dbThread, err := s.q.UpdateThreadStatus(ctx, db.UpdateThreadStatusParams{
		ID:            id,
		Status:        string(params.Status),
		Error:         params.Error,
		ResultSummary: params.ResultSummary,
		CompletedAt:   sqlInt64(params.CompletedAt),
	})
	if err != nil {
		return Thread{}, err
	}
	return testFromDBItem(dbThread), nil
}

func (s *testStoreDB) SetSession(ctx context.Context, id, sessionID string) (Thread, error) {
	dbThread, err := s.q.UpdateThreadSession(ctx, db.UpdateThreadSessionParams{
		ID:        id,
		SessionID: sessionID,
	})
	if err != nil {
		return Thread{}, err
	}
	return testFromDBItem(dbThread), nil
}

func (s *testStoreDB) Delete(ctx context.Context, id string) error {
	return s.q.DeleteThread(ctx, id)
}

// sqlInt64 mirrors the threadspawn store's CompletedAt handling: zero leaves
// the column NULL.
func sqlInt64(v int64) sql.NullInt64 {
	return sql.NullInt64{Int64: v, Valid: v != 0}
}

// testFromDBItem mirrors threadspawn.fromDBItem (which cannot be imported
// here); the field mapping is identical.
func testFromDBItem(item db.Thread) Thread {
	return Thread{
		Delegation: Delegation{
			ID:              item.ID,
			Name:            item.Name,
			Goal:            item.Goal,
			SessionID:       item.SessionID,
			Status:          Status(item.Status),
			Kind:            Kind(item.Kind),
			ResultSummary:   item.ResultSummary,
			Error:           item.Error,
			CreatedAt:       item.CreatedAt,
			UpdatedAt:       item.UpdatedAt,
			CompletedAt:     item.CompletedAt.Int64,
			ParentSessionID: item.ParentSessionID,
		},
		BaseBranch:   item.BaseBranch,
		Branch:       item.Branch,
		WorktreePath: item.WorktreePath,
		MergePolicy:  MergePolicy(item.MergePolicy),
	}
}
