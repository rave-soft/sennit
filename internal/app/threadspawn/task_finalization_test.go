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

	require.NoError(t, store.MarkTaskCompletionDelivered(t.Context(), st.ID))
	pending, err = store.ListPendingTaskCompletions(t.Context())
	require.NoError(t, err)
	require.Empty(t, pending)
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
