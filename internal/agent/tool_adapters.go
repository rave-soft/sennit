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
	coverage := t.service.ReadCoverage(ctx, sessionID, path)
	ranges := make([]tools.FileLineRange, len(coverage.Ranges))
	for i, r := range coverage.Ranges {
		ranges[i] = tools.FileLineRange{Start: r.Start, End: r.End}
	}
	return tools.FileCoverage{Full: coverage.Full, Ranges: ranges}
}

func (t fileTracking) LastReadTime(ctx context.Context, sessionID, path string) time.Time {
	return t.service.LastReadTime(ctx, sessionID, path)
}

// todoSessions adapts the session store's larger API to the tools-owned port.
type todoSessions struct {
	get func(context.Context, string) (session.Session, error)
	set func(context.Context, string, []session.Todo) error
}

func newTodoSessions(service interface {
	Get(context.Context, string) (session.Session, error)
	SetTodos(context.Context, string, []session.Todo) error
},
) tools.TodoSessions {
	if service == nil {
		return nil
	}
	return todoSessions{get: service.Get, set: service.SetTodos}
}

func (s todoSessions) Todos(ctx context.Context, sessionID string) ([]session.Todo, error) {
	current, err := s.get(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return current.Todos, nil
}

func (s todoSessions) SetTodos(ctx context.Context, sessionID string, todos []session.Todo) error {
	return s.set(ctx, sessionID, todos)
}
