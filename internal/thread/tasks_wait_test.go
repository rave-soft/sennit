package thread_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/thread"
	"github.com/stretchr/testify/require"
)

func TestTaskManagerWaitTerminalBeforeWait(t *testing.T) {
	for _, status := range []thread.Status{thread.StatusCompleted, thread.StatusFailed, thread.StatusCancelled, thread.StatusInterrupted} {
		t.Run(string(status), func(t *testing.T) {
			store := thread.NewStoreForTest(t)
			_, tasks, _ := newTestTaskManager(t, store)
			st, err := store.Create(t.Context(), thread.CreateParams{Name: "terminal", Kind: thread.KindTask})
			require.NoError(t, err)
			_, err = store.SetStatus(t.Context(), st.ID, thread.SetStatusParams{Status: status})
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			require.NoError(t, tasks.Wait(ctx, []string{st.ID}))
		})
	}
}

type waitSnapshotStore struct {
	thread.Store
	mu      sync.Mutex
	armed   bool
	read    chan struct{}
	release chan struct{}
}

func (s *waitSnapshotStore) Get(ctx context.Context, id string) (thread.Thread, error) {
	st, err := s.Store.Get(ctx, id)
	s.mu.Lock()
	armed := s.armed
	s.armed = false
	s.mu.Unlock()
	if armed {
		close(s.read)
		select {
		case <-s.release:
		case <-ctx.Done():
			return thread.Thread{}, ctx.Err()
		}
	}
	return st, err
}

func TestTaskManagerWaitTransitionRace(t *testing.T) {
	store := &waitSnapshotStore{Store: thread.NewStoreForTest(t), read: make(chan struct{}), release: make(chan struct{})}
	_, tasks, _ := newTestTaskManager(t, store)
	st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "wait", ParentSessionID: "parent"})
	require.NoError(t, err)
	store.mu.Lock()
	store.armed = true
	store.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- tasks.Wait(ctx, []string{st.ID}) }()
	<-store.read
	require.NoError(t, tasks.Cancel(t.Context(), st.ID, "race"))
	close(store.release)
	require.NoError(t, <-done)
}

func TestTaskManagerWaitMultipleAndCancellation(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, _ := newTestTaskManager(t, store)
	first, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "first", ParentSessionID: "parent"})
	require.NoError(t, err)
	second, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "second", ParentSessionID: "parent"})
	require.NoError(t, err)
	require.NoError(t, tasks.Cancel(t.Context(), first.ID, "done"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, tasks.Wait(ctx, []string{first.ID, second.ID}), context.Canceled)
	got, err := tasks.Get(t.Context(), second.ID)
	require.NoError(t, err)
	require.True(t, got.Status.Active())
	ctx, cancel = context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- tasks.Wait(ctx, []string{first.ID, second.ID}) }()
	select {
	case err := <-done:
		t.Fatalf("returned before second terminal: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	require.NoError(t, tasks.Cancel(t.Context(), second.ID, "done"))
	require.NoError(t, <-done)
	require.NoError(t, tasks.Wait(t.Context(), nil))
	require.Error(t, tasks.Wait(t.Context(), []string{"missing"}))
}
