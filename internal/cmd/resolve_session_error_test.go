package cmd

import (
	"context"
	"errors"
	"testing"

	"github.com/rave-soft/sennit/internal/session"
	"github.com/stretchr/testify/require"
)

type failingSessionLookup struct {
	getErr    error
	listErr   error
	listCalls int
}

func (s *failingSessionLookup) Get(context.Context, string) (session.Session, error) {
	return session.Session{}, s.getErr
}

func (s *failingSessionLookup) List(context.Context) ([]session.Session, error) {
	s.listCalls++
	return nil, s.listErr
}

func TestResolveSessionIDDoesNotListOnDirectError(t *testing.T) {
	want := errors.New("database unavailable")
	lookup := &failingSessionLookup{getErr: want}

	_, err := resolveSessionID(t.Context(), lookup, "prefix")
	require.ErrorIs(t, err, want)
	require.Zero(t, lookup.listCalls)
}

func TestResolveSessionIDFallsBackOnNotFound(t *testing.T) {
	want := errors.New("list unavailable")
	lookup := &failingSessionLookup{getErr: session.ErrNotFound, listErr: want}

	_, err := resolveSessionID(t.Context(), lookup, "prefix")
	require.ErrorIs(t, err, want)
	require.Equal(t, 1, lookup.listCalls)
}
