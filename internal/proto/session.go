package proto

import "github.com/rave-soft/sennit/internal/session"

// Todo is the session todo wire representation. It is an alias so the
// wire boundary preserves every session status, including statuses
// introduced by newer peers.
type Todo = session.Todo

// Session represents a session in the proto layer.
//
// IsBusy and AttachedClients are vestigial: the transport layer that
// computed them on read is gone, and nothing populates them today. Ask
// the workspace facade instead — AgentIsBusy reports run state. Both
// fields are candidates for the dead-code sweep (plan item K2).
type Session struct {
	ID               string  `json:"id"`
	ParentSessionID  string  `json:"parent_session_id"`
	Title            string  `json:"title"`
	MessageCount     int64   `json:"message_count"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	SummaryMessageID string  `json:"summary_message_id"`
	Cost             float64 `json:"cost"`
	Todos            []Todo  `json:"todos,omitempty"`
	CreatedAt        int64   `json:"created_at"`
	UpdatedAt        int64   `json:"updated_at"`
	IsBusy           bool    `json:"is_busy"`
	AttachedClients  int     `json:"attached_clients"`
}
