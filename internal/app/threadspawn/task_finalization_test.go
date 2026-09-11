package threadspawn

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/db"
	sessionstore "github.com/rave-soft/sennit/internal/session/store"
	"github.com/rave-soft/sennit/internal/thread"
	"github.com/stretchr/testify/require"
)

func TestTransactionalStoreFinalizeTaskExactlyOnce(t *testing.T) {
	project := t.TempDir()
	conn, err := db.Connect(t.Context(), config.GlobalDBDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(config.GlobalDBDir())) })

	sessions := sessionstore.NewService(db.New(conn), conn, project)
	parent, err := sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	child, err := sessions.CreateTaskSession(t.Context(), uuid.NewString(), parent.ID, "child")
	require.NoError(t, err)
	child.Cost = 1.25
	_, err = sessions.Save(t.Context(), child)
	require.NoError(t, err)

	store := NewTransactionalStore(conn, project).(*store)
	st, err := store.Create(t.Context(), thread.CreateParams{
		Name: "task-finalize", Goal: "goal", Kind: thread.KindTask,
		SessionID: child.ID, ParentSessionID: parent.ID,
	})
	require.NoError(t, err)
	_, err = store.SetStatus(t.Context(), st.ID, thread.SetStatusParams{Status: thread.StatusRunning})
	require.NoError(t, err)

	const contenders = 16
	results := make(chan bool, contenders)
	errs := make(chan error, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, won, err := store.FinalizeTask(t.Context(), st.ID, thread.FinalizeTaskParams{
				Status: thread.StatusCompleted, ResultSummary: "done", CompletedAt: 10,
				CompletionDepth: 2, TerminalAt: 20,
			})
			results <- won
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	wins := 0
	for won := range results {
		if won {
			wins++
		}
	}
	for err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, 1, wins)

	gotParent, err := sessions.Get(t.Context(), parent.ID)
	require.NoError(t, err)
	require.InDelta(t, 1.25, gotParent.Cost, 1e-9)
	pending, err := store.ListPendingTaskCompletions(t.Context())
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, st.ID, pending[0].ID)
	require.Equal(t, 2, pending[0].CompletionDepth)
	require.Equal(t, int64(20), pending[0].TerminalAt)

	require.NoError(t, store.AcknowledgeTaskCompletionGeneration(t.Context(), st.ID, 19))
	pending, err = store.ListPendingTaskCompletions(t.Context())
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.NoError(t, store.AcknowledgeTaskCompletionGeneration(t.Context(), st.ID, 20))
	pending, err = store.ListPendingTaskCompletions(t.Context())
	require.NoError(t, err)
	require.Empty(t, pending)
}

func TestTransactionalStoreCompletionGenerationsRemainImmutable(t *testing.T) {
	project := t.TempDir()
	conn, err := db.Connect(t.Context(), config.GlobalDBDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(config.GlobalDBDir())) })
	sessions := sessionstore.NewService(db.New(conn), conn, project)
	parent, err := sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	child, err := sessions.CreateTaskSession(t.Context(), uuid.NewString(), parent.ID, "child")
	require.NoError(t, err)
	store := NewTransactionalStore(conn, project).(*store)
	st, err := store.Create(t.Context(), thread.CreateParams{Name: "generations", Goal: "goal", Kind: thread.KindTask, SessionID: child.ID, ParentSessionID: parent.ID})
	require.NoError(t, err)
	_, err = store.SetStatus(t.Context(), st.ID, thread.SetStatusParams{Status: thread.StatusRunning})
	require.NoError(t, err)
	first, won, err := store.FinalizeTask(t.Context(), st.ID, thread.FinalizeTaskParams{Status: thread.StatusCompleted, ResultSummary: "first", CompletionDepth: 1, TerminalAt: 20})
	require.NoError(t, err)
	require.True(t, won)
	_, err = store.SetStatus(t.Context(), st.ID, thread.SetStatusParams{Status: thread.StatusRunning})
	require.NoError(t, err)
	second, won, err := store.FinalizeTask(t.Context(), st.ID, thread.FinalizeTaskParams{Status: thread.StatusFailed, Error: "second", CompletionDepth: 2, TerminalAt: 10})
	require.NoError(t, err)
	require.True(t, won)
	require.Greater(t, second.TerminalAt, first.TerminalAt)
	pending, err := store.ListPendingTaskCompletions(t.Context())
	require.NoError(t, err)
	require.Len(t, pending, 2)
	require.Equal(t, "first", pending[0].ResultSummary)
	require.Equal(t, thread.StatusCompleted, pending[0].Status)
	require.Equal(t, "second", pending[1].Error)
	require.Equal(t, thread.StatusFailed, pending[1].Status)
	require.NoError(t, store.AcknowledgeTaskCompletionGeneration(t.Context(), st.ID, first.TerminalAt))
	require.NoError(t, store.AcknowledgeTaskCompletionGeneration(t.Context(), st.ID, first.TerminalAt))
	pending, err = store.ListPendingTaskCompletions(t.Context())
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, second.TerminalAt, pending[0].TerminalAt)
	got, err := store.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.True(t, got.CompletionPending)
	require.NoError(t, store.AcknowledgeTaskCompletionGeneration(t.Context(), st.ID, second.TerminalAt))
	got, err = store.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.False(t, got.CompletionPending)
}

func TestTransactionalStoreOutboxInsertFailureRollsBack(t *testing.T) {
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	sessions := sessionstore.NewService(db.New(conn), conn, dataDir)
	parent, err := sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	child, err := sessions.CreateTaskSession(t.Context(), uuid.NewString(), parent.ID, "child")
	require.NoError(t, err)
	child.Cost = 3.25
	_, err = sessions.Save(t.Context(), child)
	require.NoError(t, err)
	storage := NewTransactionalStore(conn, dataDir).(*store)
	st, err := storage.Create(t.Context(), thread.CreateParams{Name: "rollback", Kind: thread.KindTask, SessionID: child.ID, ParentSessionID: parent.ID})
	require.NoError(t, err)
	_, err = storage.SetStatus(t.Context(), st.ID, thread.SetStatusParams{Status: thread.StatusRunning})
	require.NoError(t, err)
	before, err := db.New(conn).GetThread(t.Context(), st.ID)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `CREATE TRIGGER fail_outbox_insert BEFORE INSERT ON task_completion_outbox BEGIN SELECT RAISE(ABORT, 'outbox insert rejected'); END`)
	require.NoError(t, err)
	_, won, err := storage.FinalizeTask(t.Context(), st.ID, thread.FinalizeTaskParams{Status: thread.StatusCompleted, ResultSummary: "done", TerminalAt: 20})
	require.ErrorContains(t, err, "outbox insert rejected")
	require.False(t, won)
	after, err := db.New(conn).GetThread(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, before, after)
	charged, err := sessions.Get(t.Context(), parent.ID)
	require.NoError(t, err)
	require.Zero(t, charged.Cost)
	pending, err := storage.ListPendingTaskCompletions(t.Context())
	require.NoError(t, err)
	require.Empty(t, pending)
	_, err = conn.ExecContext(t.Context(), `DROP TRIGGER fail_outbox_insert`)
	require.NoError(t, err)
	_, won, err = storage.FinalizeTask(t.Context(), st.ID, thread.FinalizeTaskParams{Status: thread.StatusCompleted, ResultSummary: "done", TerminalAt: 20})
	require.NoError(t, err)
	require.True(t, won)
	charged, err = sessions.Get(t.Context(), parent.ID)
	require.NoError(t, err)
	require.Equal(t, 3.25, charged.Cost)
	pending, err = storage.ListPendingTaskCompletions(t.Context())
	require.NoError(t, err)
	require.Len(t, pending, 1)
}

func TestTransactionalStoreReopenResumedTaskPreservesReport(t *testing.T) {
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	storage := NewTransactionalStore(conn, dataDir).(*store)
	st, err := storage.Create(t.Context(), thread.CreateParams{Name: "original", Goal: "original goal", Kind: thread.KindTask, SessionID: "child", ParentSessionID: "parent"})
	require.NoError(t, err)
	first, won, err := storage.FinalizeTask(t.Context(), st.ID, thread.FinalizeTaskParams{Status: thread.StatusFailed, Error: "original error", ResultSummary: "original result", CompletedAt: 7, CompletionDepth: 3, TerminalAt: 20})
	require.NoError(t, err)
	require.True(t, won)
	_, err = storage.SetStatus(t.Context(), st.ID, thread.SetStatusParams{Status: thread.StatusRunning})
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `UPDATE threads SET name = 'changed', goal = 'changed', session_id = 'new-child', parent_session_id = 'new-parent' WHERE id = ?`, st.ID)
	require.NoError(t, err)
	require.NoError(t, db.Release(dataDir))
	conn, err = db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	storage = NewTransactionalStore(conn, dataDir).(*store)
	pending, err := storage.ListPendingTaskCompletions(t.Context())
	require.NoError(t, err)
	require.Len(t, pending, 1)
	got := pending[0]
	require.Equal(t, first.ID, got.ID)
	require.Equal(t, first.Name, got.Name)
	require.Equal(t, first.Goal, got.Goal)
	require.Equal(t, first.SessionID, got.SessionID)
	require.Equal(t, first.ParentSessionID, got.ParentSessionID)
	require.Equal(t, first.Kind, got.Kind)
	require.Equal(t, first.Status, got.Status)
	require.Equal(t, first.Error, got.Error)
	require.Equal(t, first.ResultSummary, got.ResultSummary)
	require.Equal(t, first.CompletedAt, got.CompletedAt)
	require.Equal(t, first.TerminalAt, got.TerminalAt)
	require.Equal(t, first.CompletionDepth, got.CompletionDepth)
	require.True(t, got.CompletionPending)
	second, won, err := storage.FinalizeTask(t.Context(), st.ID, thread.FinalizeTaskParams{Status: thread.StatusCompleted, TerminalAt: first.TerminalAt})
	require.NoError(t, err)
	require.True(t, won)
	require.Equal(t, first.TerminalAt+1, second.TerminalAt)
}

func TestTransactionalStoreRecoveryFinalizesActiveTaskExactlyOnce(t *testing.T) {
	project := t.TempDir()
	conn, err := db.Connect(t.Context(), config.GlobalDBDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(config.GlobalDBDir())) })

	sessions := sessionstore.NewService(db.New(conn), conn, project)
	parent, err := sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	child, err := sessions.CreateTaskSession(t.Context(), uuid.NewString(), parent.ID, "child")
	require.NoError(t, err)
	child.Cost = 3.5
	_, err = sessions.Save(t.Context(), child)
	require.NoError(t, err)

	store := NewTransactionalStore(conn, project).(*store)
	st, err := store.Create(t.Context(), thread.CreateParams{
		Name: "recover-running", Goal: "goal", Kind: thread.KindTask,
		SessionID: child.ID, ParentSessionID: parent.ID,
	})
	require.NoError(t, err)
	_, err = store.SetStatus(t.Context(), st.ID, thread.SetStatusParams{Status: thread.StatusRunning})
	require.NoError(t, err)

	mgr := thread.NewManager(thread.ManagerOptions{Store: store, RepoRoot: project, Context: t.Context()})
	t.Cleanup(func() { require.NoError(t, mgr.Shutdown(context.Background())) })
	require.NoError(t, mgr.Recover(t.Context()))

	got, err := store.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusInterrupted, got.Status)
	require.True(t, got.CompletionPending)
	gotParent, err := sessions.Get(t.Context(), parent.ID)
	require.NoError(t, err)
	require.InDelta(t, 3.5, gotParent.Cost, 1e-9)
	pending, err := store.ListPendingTaskCompletions(t.Context())
	require.NoError(t, err)
	require.Len(t, pending, 1)

	require.NoError(t, mgr.Recover(t.Context()))
	gotParent, err = sessions.Get(t.Context(), parent.ID)
	require.NoError(t, err)
	require.InDelta(t, 3.5, gotParent.Cost, 1e-9, "second recovery must not attribute cost twice")
	pending, err = store.ListPendingTaskCompletions(t.Context())
	require.NoError(t, err)
	require.Len(t, pending, 1, "unacknowledged completion remains replayable without duplication")
}

func TestTransactionalStoreFinalizeTaskRollsBackWithoutParent(t *testing.T) {
	project := t.TempDir()
	conn, err := db.Connect(t.Context(), config.GlobalDBDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(config.GlobalDBDir())) })

	sessions := sessionstore.NewService(db.New(conn), conn, project)
	child, err := sessions.Create(t.Context(), "child")
	require.NoError(t, err)
	child.Cost = 2
	_, err = sessions.Save(t.Context(), child)
	require.NoError(t, err)
	store := NewTransactionalStore(conn, project).(*store)
	st, err := store.Create(t.Context(), thread.CreateParams{
		Name: "missing-parent", Goal: "goal", Kind: thread.KindTask,
		SessionID: child.ID, ParentSessionID: "missing",
	})
	require.NoError(t, err)
	_, err = store.SetStatus(t.Context(), st.ID, thread.SetStatusParams{Status: thread.StatusRunning})
	require.NoError(t, err)

	_, won, err := store.FinalizeTask(t.Context(), st.ID, thread.FinalizeTaskParams{Status: thread.StatusFailed, Error: "boom", TerminalAt: 1})
	require.NoError(t, err)
	require.True(t, won)
	got, getErr := store.Get(t.Context(), st.ID)
	require.NoError(t, getErr)
	require.Equal(t, thread.StatusFailed, got.Status)
	require.True(t, got.CompletionPending)
	// A missing parent cannot be charged, but must not prevent cancellation
	// or failure from becoming durable.
	gotChild, childErr := sessions.Get(t.Context(), child.ID)
	require.NoError(t, childErr)
	require.InDelta(t, 2, gotChild.Cost, 1e-9)
}
