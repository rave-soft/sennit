package appws

import (
	"context"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/session"
)

// -- Sessions --

func (w *AppWorkspace) CreateSession(ctx context.Context, title string) (session.Session, error) {
	return w.app.Sessions().Create(ctx, title)
}

func (w *AppWorkspace) GetSession(ctx context.Context, sessionID string) (session.Session, error) {
	return w.app.Sessions().Get(ctx, sessionID)
}

func (w *AppWorkspace) ListSessions(ctx context.Context) ([]session.Session, error) {
	return w.app.Sessions().List(ctx)
}

func (w *AppWorkspace) GetLastSession(ctx context.Context) (session.Session, error) {
	return w.app.Sessions().GetLast(ctx)
}

func (w *AppWorkspace) RenameSession(ctx context.Context, sessionID string, title string) error {
	return w.app.Sessions().Rename(ctx, sessionID, title)
}

func (w *AppWorkspace) DeleteSession(ctx context.Context, sessionID string) error {
	return w.app.Sessions().Delete(ctx, sessionID)
}

// SetCurrentSession reports the active session to herdr so the pane
// can persist a resumable reference. Multi-client presence tracking
// is irrelevant in single-client local mode, but herdr still needs
// to know which session is live to support agent resume.
func (w *AppWorkspace) SetCurrentSession(ctx context.Context, sessionID string) error {
	return w.SetCurrentSessionGeneration(ctx, sessionID, 0)
}

func (w *AppWorkspace) SetCurrentSessionGeneration(_ context.Context, sessionID string, _ uint64) error {
	w.app.ReportCurrentSession(sessionID)
	return nil
}

func (w *AppWorkspace) SessionDescendantCost(ctx context.Context, sessionID string) (float64, error) {
	return w.app.Sessions().DescendantCost(ctx, sessionID)
}

// -- Messages --

func (w *AppWorkspace) ListMessages(ctx context.Context, sessionID string) ([]message.Message, error) {
	// Drain any debounced updates so the caller observes the latest
	// in-memory state. message.Service buffers streaming deltas and a
	// cold List would otherwise miss them at session-switch time.
	if err := w.app.Messages().FlushAll(ctx); err != nil {
		return nil, err
	}
	return w.app.Messages().List(ctx, sessionID)
}

func (w *AppWorkspace) ListUserMessages(ctx context.Context, sessionID string) ([]message.Message, error) {
	return w.app.Messages().ListUserMessages(ctx, sessionID)
}

func (w *AppWorkspace) ListAllUserMessages(ctx context.Context) ([]message.Message, error) {
	return w.app.Messages().ListAllUserMessages(ctx)
}

func (w *AppWorkspace) ListMessagesBySessionIDs(ctx context.Context, rootSessionID string, _ uint64, sessionIDs []string) (map[string][]message.Message, error) {
	validated, err := w.app.Sessions().ValidateSessionIDsInTree(ctx, rootSessionID, sessionIDs)
	if err != nil {
		return nil, err
	}
	if err := w.app.Messages().FlushAll(ctx); err != nil {
		return nil, err
	}
	return w.app.Messages().ListBySessionIDs(ctx, validated)
}
