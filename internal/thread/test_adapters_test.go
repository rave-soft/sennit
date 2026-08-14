package thread

import (
	"context"

	"github.com/rave-soft/braid/internal/app"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/permission"
	"github.com/rave-soft/braid/internal/session"
)

// The helpers in this file are the test-only stand-ins for the threadspawn
// implementations of the same roles: an in-package test cannot import
// threadspawn (threadspawn imports thread, so an internal test importing it
// would be an import cycle), but it still needs a Spawner that wraps the
// caller's own already-running App and the domain-view adapters over the
// real session/message services. They return the real production wiring
// (the real services, adapted to the domain view) so the in-package tests
// exercise the same code paths the external ones do.
//
// They deliberately live in a _test.go file — not in testing.go — because
// they name internal/app and internal/session/message, and those must not
// leak into the production build of internal/thread (see the package's
// dependency-boundary requirement).

// NewTestParentAppSpawner returns a Spawner whose every Spawn call returns
// a Handle wrapping a, the caller's own already-running App — the
// test-only counterpart of threadspawn.NewParentAppSpawner.
func NewTestParentAppSpawner(a *app.App) Spawner {
	return &parentAppTestSpawner{app: a}
}

type parentAppTestSpawner struct {
	app *app.App
}

func (s *parentAppTestSpawner) Spawn(ctx context.Context, path string) (Handle, error) {
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
func (h *parentAppTestHandle) Workspace() Workspace {
	return &parentAppTestWorkspace{app: h.app}
}

// parentAppTestWorkspace adapts an *app.App to the domain [Workspace].
type parentAppTestWorkspace struct {
	app *app.App
}

func (w *parentAppTestWorkspace) Coordinator() Coordinator {
	return &testCoordinatorAdapter{inner: w.app.Coordinator()}
}

func (w *parentAppTestWorkspace) Sessions() SessionService {
	if w.app.Sessions() == nil {
		return nil
	}
	return NewTestSessionService(w.app.Sessions())
}

func (w *parentAppTestWorkspace) Messages() MessageService {
	if w.app.Messages() == nil {
		return nil
	}
	return NewTestMessageService(w.app.Messages())
}

func (w *parentAppTestWorkspace) Permissions() permission.Service {
	return w.app.Permissions()
}

func (w *parentAppTestWorkspace) RunCompletions() RunCompletionBroker {
	return &testRunCompletionBrokerAdapter{inner: w.app.RunCompletions()}
}

func (w *parentAppTestWorkspace) SendEvent(msg any) {
	w.app.SendEvent(msg)
}

// NewTestMessageService adapts a real message service to the domain's
// narrow [MessageService] view, as the composition seam does in
// production (see threadspawn.NewMessageService).
func NewTestMessageService(full message.Service) MessageService {
	return &testMessageService{full: full}
}

type testMessageService struct {
	full message.Service
}

func (m *testMessageService) Create(ctx context.Context, sessionID string, role MessageRole, parts []ContentPart) error {
	converted := make([]message.ContentPart, 0, len(parts))
	for _, p := range parts {
		if t, ok := p.(TextContent); ok {
			converted = append(converted, message.TextContent{Text: t.Text})
		}
	}
	_, err := m.full.Create(ctx, sessionID, message.CreateMessageParams{Role: message.MessageRole(role), Parts: converted})
	return err
}

func (m *testMessageService) List(ctx context.Context, sessionID string) ([]Message, error) {
	all, err := m.full.List(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(all))
	for _, msg := range all {
		out = append(out, Message{Role: MessageRole(msg.Role), Text: msg.Content().Text})
	}
	return out, nil
}

// NewTestSessionService adapts a real session service to the domain's
// narrow [SessionService] view, as the composition seam does in
// production (see threadspawn.NewSessionService).
func NewTestSessionService(full session.Service) SessionService {
	return &testSessionService{full: full}
}

type testSessionService struct {
	full session.Service
}

func (s *testSessionService) Create(ctx context.Context, title string) (Session, error) {
	sess, err := s.full.Create(ctx, title)
	if err != nil {
		return Session{}, err
	}
	return Session{ID: sess.ID, Title: sess.Title}, nil
}

func (s *testSessionService) CreateTaskSession(ctx context.Context, toolCallID, parentSessionID, title string) (Session, error) {
	sess, err := s.full.CreateTaskSession(ctx, toolCallID, parentSessionID, title)
	if err != nil {
		return Session{}, err
	}
	return Session{ID: sess.ID, Title: sess.Title}, nil
}
