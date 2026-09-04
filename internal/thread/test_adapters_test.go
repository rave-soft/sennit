package thread_test

import (
	"context"
	"time"

	"github.com/rave-soft/sennit/internal/thread"

	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/message"
	messagestore "github.com/rave-soft/sennit/internal/message/store"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/question"
	sessionstore "github.com/rave-soft/sennit/internal/session/store"
)

// The helpers in this file are the test-only stand-ins for the threadspawn
// implementations of the same roles: this package's own tests cannot
// import threadspawn (threadspawn imports thread, and this package's own
// tests already import thread — see fakes_test.go's package doc — so
// importing threadspawn too would just give the same adapters a second,
// parallel implementation for no reason), but they still need a Spawner
// that wraps the caller's own already-running App and the domain-view
// adapters over the real session/message services. They return the real
// production wiring (the real services, adapted to the domain view) so
// these tests exercise the same code paths the production composition
// seam does.
//
// NewTestSessionService/NewTestMessageService/NewTestParentAppSpawner are
// exported (unlike the unexported adapter types around them) because
// other packages' tests that want a real, working thread.Manager/
// TaskManager without threadspawn's own dependencies use them too — see
// NewTaskManagerFromManager's doc comment for the same reasoning.

// NewTestParentAppSpawner returns a Spawner whose every Spawn call returns
// a Handle wrapping a, the caller's own already-running App — the
// test-only counterpart of threadspawn.NewParentAppSpawner.
func NewTestParentAppSpawner(a *app.App) thread.Spawner {
	return &parentAppTestSpawner{app: a}
}

type parentAppTestSpawner struct {
	app *app.App
}

func (s *parentAppTestSpawner) Spawn(ctx context.Context, path string) (thread.Handle, error) {
	return &parentAppTestHandle{app: s.app}, nil
}

func (s *parentAppTestSpawner) Release(ctx context.Context, id string) error { return nil }

// parentAppTestHandle wraps a parent App as a [Handle]; its Workspace is a
// thin adapter over the App (the domain's Workspace names the domain's own
// SessionService/MessageService, which the real App's accessors do not
// return, so the raw App no longer satisfies it).
type parentAppTestHandle struct {
	app *app.App
}

func (h *parentAppTestHandle) ID() string { return "" }
func (h *parentAppTestHandle) Workspace() thread.Workspace {
	return &parentAppTestWorkspace{app: h.app}
}

// parentAppTestWorkspace adapts an *app.App to the domain [Workspace].
type parentAppTestWorkspace struct {
	app *app.App
}

func (w *parentAppTestWorkspace) Coordinator() thread.Coordinator {
	return &testCoordinatorAdapter{inner: w.app.Coordinator()}
}

func (w *parentAppTestWorkspace) Sessions() thread.SessionService {
	if w.app.Sessions() == nil {
		return nil
	}
	return NewTestSessionService(w.app.Sessions())
}

func (w *parentAppTestWorkspace) Messages() thread.MessageService {
	if w.app.Messages() == nil {
		return nil
	}
	return NewTestMessageService(w.app.Messages())
}

func (w *parentAppTestWorkspace) Permissions() permission.Service {
	return w.app.Permissions()
}

func (w *parentAppTestWorkspace) Questions() question.Service {
	return w.app.Questions
}

func (w *parentAppTestWorkspace) RunCompletions() thread.RunCompletionBroker {
	return &testRunCompletionBrokerAdapter{inner: w.app.RunCompletions()}
}

func (w *parentAppTestWorkspace) SendEvent(msg any) {
	w.app.SendEvent(msg)
}

// NewTestMessageService adapts a real message service to the domain's
// narrow [MessageService] view, as the composition seam does in
// production (see threadspawn.NewMessageService).
func NewTestMessageService(full messagestore.Service) thread.MessageService {
	return &testMessageService{full: full}
}

type testMessageService struct {
	full messagestore.Service
}

func (m *testMessageService) Create(ctx context.Context, sessionID string, role thread.MessageRole, parts []thread.ContentPart) error {
	converted := make([]message.ContentPart, 0, len(parts))
	for _, p := range parts {
		if t, ok := p.(thread.TextContent); ok {
			converted = append(converted, message.TextContent{Text: t.Text})
		}
	}
	_, err := m.full.Create(ctx, sessionID, message.CreateMessageParams{Role: message.MessageRole(role), Parts: converted})
	return err
}

func (m *testMessageService) List(ctx context.Context, sessionID string) ([]thread.Message, error) {
	all, err := m.full.List(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]thread.Message, 0, len(all))
	for _, msg := range all {
		out = append(out, thread.Message{Role: thread.MessageRole(msg.Role), Text: msg.Content().Text})
	}
	return out, nil
}

func (m *testMessageService) LastActivity(ctx context.Context, sessionID string) (time.Time, error) {
	// app.NewForTest deliberately leaves Messages nil, and most task tests
	// never touch it (see newTestParentApp). "No message service" is the
	// same answer as "nothing has been written yet" for the one caller
	// that asks — the idle sweep, which then measures from the task's own
	// creation instead.
	if m.full == nil {
		return time.Time{}, nil
	}
	return m.full.LastActivity(ctx, sessionID)
}

// NewTestSessionService adapts a real session service to the domain's
// narrow [SessionService] view, as the composition seam does in
// production (see threadspawn.NewSessionService).
func NewTestSessionService(full sessionstore.Service) thread.SessionService {
	return &testSessionService{full: full}
}

type testSessionService struct {
	full sessionstore.Service
}

func (s *testSessionService) Create(ctx context.Context, title string) (thread.Session, error) {
	sess, err := s.full.Create(ctx, title)
	if err != nil {
		return thread.Session{}, err
	}
	return thread.Session{ID: sess.ID, Title: sess.Title}, nil
}

func (s *testSessionService) CreateTaskSession(ctx context.Context, toolCallID, parentSessionID, title string) (thread.Session, error) {
	sess, err := s.full.CreateTaskSession(ctx, toolCallID, parentSessionID, title)
	if err != nil {
		return thread.Session{}, err
	}
	return thread.Session{ID: sess.ID, Title: sess.Title}, nil
}

func (s *testSessionService) CreateSubAgentSession(ctx context.Context, toolCallID, parentSessionID, title, agentID string) (thread.Session, error) {
	sess, err := s.full.CreateSubAgentSession(ctx, toolCallID, parentSessionID, title, agentID)
	if err != nil {
		return thread.Session{}, err
	}
	return thread.Session{ID: sess.ID, Title: sess.Title}, nil
}
