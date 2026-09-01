package thread_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rave-soft/sennit/internal/thread"
	"github.com/stretchr/testify/require"
)

type resolveStore struct {
	thread.Store
	getErr       error
	getByNameErr error
	byNameCalls  int
}

func (s *resolveStore) Get(context.Context, string) (thread.Thread, error) {
	return thread.Thread{}, s.getErr
}

func (s *resolveStore) GetByName(context.Context, string) (thread.Thread, error) {
	s.byNameCalls++
	return thread.Thread{}, s.getByNameErr
}

func TestManagerGetDoesNotFallBackOnInfrastructureError(t *testing.T) {
	want := errors.New("database unavailable")
	store := &resolveStore{getErr: want}
	manager := thread.NewManager(thread.ManagerOptions{Store: store})

	_, err := manager.Get(t.Context(), "thread")
	require.ErrorIs(t, err, want)
	require.Zero(t, store.byNameCalls)
}

func TestManagerGetReturnsFallbackError(t *testing.T) {
	want := errors.New("lookup unavailable")
	store := &resolveStore{getErr: thread.ErrNotFound, getByNameErr: want}
	manager := thread.NewManager(thread.ManagerOptions{Store: store})

	_, err := manager.Get(t.Context(), "thread")
	require.ErrorIs(t, err, want)
	require.Equal(t, 1, store.byNameCalls)
}

func TestManagerGetFallsBackOnlyOnDomainNotFound(t *testing.T) {
	store := &resolveStore{getErr: thread.ErrNotFound, getByNameErr: thread.ErrNotFound}
	manager := thread.NewManager(thread.ManagerOptions{Store: store})

	_, err := manager.Get(t.Context(), "thread")
	require.ErrorIs(t, err, thread.ErrNotFound)
	require.Equal(t, 1, store.byNameCalls)
}
