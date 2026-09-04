package threadspawn

import (
	"context"
	"sync"
	"time"

	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/message"
	messagestore "github.com/rave-soft/sennit/internal/message/store"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/question"
	sessionstore "github.com/rave-soft/sennit/internal/session/store"
	"github.com/rave-soft/sennit/internal/thread"
	"github.com/rave-soft/sennit/internal/workspace"
)

// NewAppWorkspaceAdapter presents a as a thread workspace. Adapters are
// deliberately owned by their parent spawner or handle, rather than a
// process-wide cache: retaining an adapter globally would retain its App and
// all of its services after that workspace is released.
func NewAppWorkspaceAdapter(a *app.App, frontend ...workspace.Workspace) *AppWorkspaceAdapter {
	adapter := &AppWorkspaceAdapter{App: a}
	if len(frontend) != 0 {
		adapter.frontend = frontend[0]
	}
	return adapter
}

// AppWorkspaceAdapter presents an *app.App as the thread domain's narrow
// thread.Workspace: it delegates every method straight through except
// Sessions/Messages, which it wraps in the domain-view adapters below. It
// exists because the domain's Workspace names the domain's own
// SessionService/MessageService types (not the real session/message
// service types), which *app.App no longer satisfies structurally once it
// does — so the spawner handles hand out this adapter instead of the raw
// App. Everything else the domain calls (coordinator, permissions,
// RunCompletions, SendEvent) is the identical underlying service, so
// lifecycle behavior is unchanged.
type AppWorkspaceAdapter struct {
	App      *app.App
	frontend workspace.Workspace

	mu sync.Mutex
	rc thread.RunCompletionBroker
}

// Coordinator wraps a.App's current coordinator on every call rather than
// caching it: a.App.Coordinator() is a single RWMutex read, cheap enough
// that caching buys nothing, and caching cost correctness. An App that has
// not run InitCoderAgent yet (an unconfigured project) reports a nil
// coordinator until the user finishes setup and it runs for the first
// time (see ui/model's initAgentAndReportModel); initCoderAgent can also
// rebuild the coordinator later (a model change, or the interactive
// rebuild InitCoderAgentNonInteractive's doc describes) and replace the
// field with a new instance. A cached wrapper would freeze on whichever
// coordinator was live at the first call — nil before setup, or a
// coordinator a later rebuild has since closed — and every later
// registerParent/DeliverTaskCompletion call would target that dead
// reference, losing a task's completion with no error anywhere. Wrapping
// fresh each time means every caller always reaches whichever coordinator
// is actually live right now.
func (a *AppWorkspaceAdapter) Coordinator() thread.Coordinator {
	return NewCoordinatorAdapter(a.App.Coordinator())
}

func (a *AppWorkspaceAdapter) Sessions() thread.SessionService {
	return &SessionAdapter{full: a.App.Sessions()}
}

func (a *AppWorkspaceAdapter) Messages() thread.MessageService {
	return &MessageAdapter{full: a.App.Messages()}
}

func (a *AppWorkspaceAdapter) Permissions() permission.Service {
	return a.App.Permissions()
}

// Questions returns a.App's question service directly, the same as
// Permissions above: App.Questions is already an exported field of the
// domain's own question.Service type, so no adapter wrapping is needed.
func (a *AppWorkspaceAdapter) Questions() question.Service {
	return a.App.Questions
}

func (a *AppWorkspaceAdapter) RunCompletions() thread.RunCompletionBroker {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rc == nil {
		a.rc = NewRunCompletionBroker(a.App.RunCompletions())
	}
	return a.rc
}

func (a *AppWorkspaceAdapter) SendEvent(msg any) {
	a.App.SendEvent(msg)
}

// FrontendWorkspace returns the frontend-facing workspace supplied at the
// composition root, if this handle is attachable by a frontend.
func (a *AppWorkspaceAdapter) FrontendWorkspace() workspace.Workspace {
	return a.frontend
}

// sessionAdapter wraps a workspace's real session service so it satisfies
// the thread domain's narrow thread.SessionService: the domain must not
// name the concrete session.Session row (which would drag internal/session,
// and with it internal/db, into internal/thread), so the full row is
// converted to the domain view at this seam.
type SessionAdapter struct {
	full sessionstore.Service
}

func (a *SessionAdapter) Create(ctx context.Context, title string) (thread.Session, error) {
	s, err := a.full.Create(ctx, title)
	if err != nil {
		return thread.Session{}, err
	}
	return thread.Session{ID: s.ID, Title: s.Title}, nil
}

func (a *SessionAdapter) CreateTaskSession(ctx context.Context, toolCallID, parentSessionID, title string) (thread.Session, error) {
	s, err := a.full.CreateTaskSession(ctx, toolCallID, parentSessionID, title)
	if err != nil {
		return thread.Session{}, err
	}
	return thread.Session{ID: s.ID, Title: s.Title}, nil
}

func (a *SessionAdapter) CreateSubAgentSession(ctx context.Context, toolCallID, parentSessionID, title, agentID string) (thread.Session, error) {
	s, err := a.full.CreateSubAgentSession(ctx, toolCallID, parentSessionID, title, agentID)
	if err != nil {
		return thread.Session{}, err
	}
	return thread.Session{ID: s.ID, Title: s.Title}, nil
}

// messageAdapter wraps a workspace's real message service so it satisfies
// the thread domain's narrow thread.MessageService, for the same
// seam-reason as [sessionAdapter] (the domain must not import
// internal/message): the domain's Create/role/part spellings are converted
// onto the real service's at this seam, and its List results are reduced
// to the Role/Text fields the domain reads.
type MessageAdapter struct {
	full messagestore.Service
}

func (a *MessageAdapter) Create(ctx context.Context, sessionID string, role thread.MessageRole, parts []thread.ContentPart) error {
	converted := make([]message.ContentPart, 0, len(parts))
	for _, p := range parts {
		if t, ok := p.(thread.TextContent); ok {
			converted = append(converted, message.TextContent{Text: t.Text})
			continue
		}
		// A part kind the domain does not name cannot be converted; this
		// cannot happen in production, which only ever builds TextContent.
	}
	_, err := a.full.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.MessageRole(role),
		Parts: converted,
	})
	return err
}

func (a *MessageAdapter) List(ctx context.Context, sessionID string) ([]thread.Message, error) {
	all, err := a.full.List(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]thread.Message, 0, len(all))
	for _, m := range all {
		out = append(out, thread.Message{
			Role: thread.MessageRole(m.Role),
			Text: m.Content().Text,
		})
	}
	return out, nil
}

func (a *MessageAdapter) LastActivity(ctx context.Context, sessionID string) (time.Time, error) {
	return a.full.LastActivity(ctx, sessionID)
}

// NewMessageService adapts a real message service to the thread domain's
// narrow [thread.MessageService].
func NewMessageService(full messagestore.Service) thread.MessageService {
	return &MessageAdapter{full: full}
}
