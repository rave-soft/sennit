package agent

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/filetracker"
	"github.com/rave-soft/sennit/internal/history"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/stretchr/testify/require"
)

type fakeFileHistoryStore struct {
	file                        history.File
	lookupErr                   error
	lookupPath, lookupSessionID string
	created                     []history.File
}

func (s *fakeFileHistoryStore) CreateVersion(_ context.Context, sessionID, path, content string) (history.File, error) {
	file := history.File{SessionID: sessionID, Path: path, Content: content}
	s.created = append(s.created, file)
	return file, nil
}

func (s *fakeFileHistoryStore) GetByPathAndSession(_ context.Context, path, sessionID string) (history.File, error) {
	s.lookupPath, s.lookupSessionID = path, sessionID
	if s.lookupErr != nil {
		return history.File{}, s.lookupErr
	}
	return s.file, nil
}

func TestFileHistoryAdapterTranslatesNoRowsAndForwardsArguments(t *testing.T) {
	t.Parallel()
	store := &fakeFileHistoryStore{lookupErr: sql.ErrNoRows}
	adapter := newFileHistory(store)

	content, found, err := adapter.LatestContent(t.Context(), "session-id", "/workspace/file")
	require.NoError(t, err)
	require.False(t, found)
	require.Empty(t, content)
	require.Equal(t, "/workspace/file", store.lookupPath)
	require.Equal(t, "session-id", store.lookupSessionID)

	require.NoError(t, adapter.CreateVersion(t.Context(), "session-id", "/workspace/file", "content"))
	require.Equal(t, []history.File{{SessionID: "session-id", Path: "/workspace/file", Content: "content"}}, store.created)
}

func TestFileHistoryAdapterPreservesInfrastructureError(t *testing.T) {
	t.Parallel()
	lookupErr := errors.New("history unavailable")
	adapter := newFileHistory(&fakeFileHistoryStore{lookupErr: lookupErr})

	_, found, err := adapter.LatestContent(t.Context(), "session-id", "/workspace/file")
	require.ErrorIs(t, err, lookupErr)
	require.False(t, found)
}

type fakeTrackingStore struct {
	coverageRead bool
	lastRead     time.Time
}

func (s *fakeTrackingStore) RecordRead(context.Context, string, string)                  {}
func (s *fakeTrackingStore) RecordPartialRead(context.Context, string, string, int, int) {}
func (s *fakeTrackingStore) RecordEdit(context.Context, string, string, int, int, int)   {}
func (s *fakeTrackingStore) ReadCoverage(context.Context, string, string) filetracker.Coverage {
	s.coverageRead = true
	return filetracker.Coverage{Ranges: []filetracker.LineRange{{Start: 2, End: 4}}}
}

func (s *fakeTrackingStore) LastReadTime(context.Context, string, string) time.Time {
	return s.lastRead
}

func (s *fakeTrackingStore) ListReadFiles(context.Context, string) ([]string, error) { return nil, nil }

func TestFileTrackingAdapterConvertsCoverage(t *testing.T) {
	t.Parallel()
	store := &fakeTrackingStore{lastRead: time.Unix(42, 0)}
	adapter := newFileTracking(store)

	coverage := adapter.ReadCoverage(t.Context(), "session-id", "/workspace/file")
	require.True(t, store.coverageRead)
	require.Equal(t, []tools.FileLineRange{{Start: 2, End: 4}}, coverage.Ranges)
	require.Equal(t, store.lastRead, adapter.LastReadTime(t.Context(), "session-id", "/workspace/file"))
}

type fakeTodoStore struct {
	session session.Session
	setID   string
}

func (s *fakeTodoStore) Get(context.Context, string) (session.Session, error) { return s.session, nil }

func (s *fakeTodoStore) SetTodos(_ context.Context, id string, todos []session.Todo) error {
	s.setID, s.session.Todos = id, todos
	return nil
}

func TestTodoSessionsAdapterForwardsNarrowOperations(t *testing.T) {
	t.Parallel()
	store := &fakeTodoStore{session: session.Session{Todos: []session.Todo{{Content: "test"}}}}
	adapter := newTodoSessions(store)

	todos, err := adapter.Todos(t.Context(), "session-id")
	require.NoError(t, err)
	require.Equal(t, store.session.Todos, todos)
	require.NoError(t, adapter.SetTodos(t.Context(), "session-id", nil))
	require.Equal(t, "session-id", store.setID)
}
