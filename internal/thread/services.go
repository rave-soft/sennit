package thread

import "context"

// SessionService is the slice of the workspace's session service a
// delegation's lifecycle calls: creating the child session a thread or
// task runs in, and (for a task) its nested variant.
//
// It is declared here, on the consumer side, for the same reason as
// [Workspace]: internal/thread must not import the packages that provide
// these services (internal/session in production), so the real service is
// handed in wrapped through the composition seam — see
// internal/app/threadspawn.
type SessionService interface {
	// Create creates a top-level session titled title.
	Create(ctx context.Context, title string) (Session, error)
	// CreateTaskSession creates an anonymous session nested under parentSessionID.
	CreateTaskSession(ctx context.Context, toolCallID, parentSessionID, title string) (Session, error)
	// CreateSubAgentSession additionally stamps a named agent id; an empty id
	// has the same semantics as CreateTaskSession.
	CreateSubAgentSession(ctx context.Context, toolCallID, parentSessionID, title, agentID string) (Session, error)
}

// Session is the domain's view of a session as this package consumes it:
// only the identity fields a delegation record needs, deliberately not
// the full persistence row.
type Session struct {
	// ID is the session's stable identifier; it is what gets recorded on
	// the delegation row (see [Store.SetSession]).
	ID string
	// Title is the session's display title (the delegation's goal).
	Title string
}

// MessageService is the slice of the workspace's message service this
// package calls: recording a completion note into a session's history and
// reading a task session's history back for [TaskManager.Output].
//
// Same consumer-side story as [SessionService]: internal/thread must not
// import internal/message (which pulls in internal/db), so the real
// service is wrapped through the composition seam.
type MessageService interface {
	// Create persists a message with role role and the given parts into
	// the session identified by sessionID.
	Create(ctx context.Context, sessionID string, role MessageRole, parts []ContentPart) error
	// List returns the session's messages in order.
	List(ctx context.Context, sessionID string) ([]Message, error)
}

// MessageRole is the domain's view of a message's authorship category.
// The constants below carry the same wire values the real message service
// uses, so the conversion at the composition seam is lossless.
type MessageRole string

const (
	// RoleSystem marks a record-only entry (e.g. a thread-removal note)
	// that is dropped when the prompt is built.
	RoleSystem MessageRole = "system"
	// RoleUser marks a turn typed by the person or relayed from one.
	RoleUser MessageRole = "user"
	// RoleAssistant marks the model's reply to a user turn.
	RoleAssistant MessageRole = "assistant"
)

// ContentPart is one piece of a message's content. Only the part kind
// this package builds (text) is named here; the composition seam maps it
// onto the real message package's part types.
type ContentPart interface {
	isContentPart()
}

// TextContent is a plain text content part.
type TextContent struct {
	Text string
}

func (TextContent) isContentPart() {}

// Message is the domain's view of a message as this package consumes it:
// the fields [TaskManager.Output] reads, deliberately not the full
// persistence row.
type Message struct {
	// Role is who the message is attributed to.
	Role MessageRole
	// Text is the message's plain-text content (its first text part).
	Text string
}
