package thread

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type reservationCancelStore struct {
	Store
	afterCreate func(Thread)
}

func (s *reservationCancelStore) Create(ctx context.Context, args CreateParams) (Thread, error) {
	row, err := s.Store.Create(ctx, args)
	if err == nil {
		s.afterCreate(row)
	}
	return row, err
}

func TestTaskCancellationImmediatelyAfterReservation(t *testing.T) {
	store := &reservationCancelStore{Store: NewStoreForTest(t)}
	manager := NewManager(ManagerOptions{Store: store, Context: t.Context()})
	t.Cleanup(func() { require.NoError(t, manager.Shutdown(context.Background())) })
	tasks := NewTaskManager(store, nil, nil, manager.lc, manager.ctx)
	var reserved Thread
	store.afterCreate = func(row Thread) {
		reserved = row
		require.NoError(t, tasks.Cancel(t.Context(), row.ID, "cancel at reservation"))
	}
	tasks.prepareIsolation = func(context.Context, *TaskCreateArgs) error {
		t.Fatal("cancelled reservation reached preparation")
		return nil
	}
	_, err := tasks.Create(t.Context(), TaskCreateArgs{Goal: "cancel early", ParentSessionID: "parent", Isolation: "worktree"})
	require.ErrorIs(t, err, context.Canceled)
	row, err := store.Get(t.Context(), reserved.ID)
	require.NoError(t, err)
	require.Equal(t, StatusCancelled, row.Status)
}

func TestTaskPreparationDoesNotHoldAdmissionOrBlockCancellation(t *testing.T) {
	store := NewStoreForTest(t)
	manager := NewManager(ManagerOptions{Store: store, Context: t.Context()})
	t.Cleanup(func() { require.NoError(t, manager.Shutdown(context.Background())) })
	tasks := NewTaskManager(store, nil, nil, manager.lc, manager.ctx)
	entered := make(chan struct{})
	unblock := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(unblock) }) })
	tasks.prepareIsolation = func(ctx context.Context, args *TaskCreateArgs) error {
		if args.Goal == "blocked" {
			close(entered)
			<-unblock
			return ctx.Err()
		}
		return errors.New("independent preparation failed")
	}
	finished := make(chan error, 1)
	go func() {
		_, err := tasks.Create(t.Context(), TaskCreateArgs{Goal: "blocked", ParentSessionID: "parent", Isolation: "worktree"})
		finished <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("preparation did not start")
	}
	second := make(chan error, 1)
	go func() {
		_, err := tasks.Create(t.Context(), TaskCreateArgs{Goal: "independent", ParentSessionID: "parent", Isolation: "worktree"})
		second <- err
	}()
	select {
	case err := <-second:
		require.ErrorContains(t, err, "independent preparation failed")
	case <-time.After(5 * time.Second):
		t.Fatal("preparation held admission lock")
	}
	rows, err := tasks.List(t.Context())
	require.NoError(t, err)
	var blocked Thread
	for _, row := range rows {
		if row.Goal == "blocked" {
			blocked = row
		}
	}
	require.NotEmpty(t, blocked.ID)
	cancelled := make(chan error, 1)
	go func() { cancelled <- tasks.Cancel(t.Context(), blocked.ID, "stop preparation") }()
	select {
	case err := <-cancelled:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("cancel waited for preparation lock")
	}
	once.Do(func() { close(unblock) })
	select {
	case err := <-finished:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled preparation did not finish")
	}
	row, err := store.Get(t.Context(), blocked.ID)
	require.NoError(t, err)
	require.Equal(t, StatusCancelled, row.Status)
}
