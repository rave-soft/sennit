package agent

import (
	"context"
	"time"

	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/filetracker"
	"github.com/rave-soft/sennit/internal/session"
)

// fileTracking adapts persisted file-read state to the tools-owned port.
type fileTracking struct{ service filetracker.Service }

func newFileTracking(service filetracker.Service) tools.FileTracking {
	if service == nil {
		return nil
	}
	return fileTracking{service: service}
}

func (t fileTracking) RecordRead(ctx context.Context, sessionID, path string) {
	t.service.RecordRead(ctx, sessionID, path)
}

func (t fileTracking) RecordPartialRead(ctx context.Context, sessionID, path string, start, end int) {
	t.service.RecordPartialRead(ctx, sessionID, path, start, end)
}

func (t fileTracking) RecordEdit(ctx context.Context, sessionID, path string, start, end, newEnd int) {
	t.service.RecordEdit(ctx, sessionID, path, start, end, newEnd)
}

func (t fileTracking) ReadCoverage(ctx context.Context, sessionID, path string) tools.FileCoverage {
	// filetracker.Coverage and tools.FileCoverage are the same type (both
	// alias internal/filetracker/coverage.Coverage), so no conversion is
	// needed here.
	return t.service.ReadCoverage(ctx, sessionID, path)
}

func (t fileTracking) LastReadTime(ctx context.Context, sessionID, path string) time.Time {
	return t.service.LastReadTime(ctx, sessionID, path)
}

// todoSessionService is the slice of the session store this adapter needs.
// sessionstore.Service satisfies it structurally, so nothing here pins the
// concrete store type — but its Get returns a whole session.Session where
// tools.TodoSessions wants just the todo list, so a real (if small)
// adaptation is still needed and this type can't just be handed through.
type todoSessionService interface {
	Get(context.Context, string) (session.Session, error)
	SetTodos(context.Context, string, []session.Todo) error
}

// todoSessions adapts the session store's larger API to the tools-owned port.
type todoSessions struct{ service todoSessionService }

func newTodoSessions(service todoSessionService) tools.TodoSessions {
	if service == nil {
		return nil
	}
	return todoSessions{service: service}
}

func (s todoSessions) Todos(ctx context.Context, sessionID string) ([]session.Todo, error) {
	current, err := s.service.Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return current.Todos, nil
}

func (s todoSessions) SetTodos(ctx context.Context, sessionID string, todos []session.Todo) error {
	return s.service.SetTodos(ctx, sessionID, todos)
}
