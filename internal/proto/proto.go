package proto

import (
	"github.com/rave-soft/sennit/internal/lsp"
)

// ServerNoticeLevel mirrors the UI's status-line severity taxonomy
// without depending on it, so core code can flag a notice's
// severity without importing internal/ui.
type ServerNoticeLevel string

const (
	ServerNoticeLevelInfo  ServerNoticeLevel = "info"
	ServerNoticeLevelWarn  ServerNoticeLevel = "warn"
	ServerNoticeLevelError ServerNoticeLevel = "error"
)

// ServerNotice carries a human-readable notice for display in the
// UI's status area. It is the payload core code publishes instead of
// reaching into internal/ui/util directly; the UI converts it to its
// own util.InfoMsg on receipt.
type ServerNotice struct {
	Level   ServerNoticeLevel `json:"level"`
	Message string            `json:"message"`
}

// QuestionItem is a single question within a batch.
type QuestionItem struct {
	ID          string           `json:"id"`
	Type        string           `json:"type"`
	Label       string           `json:"label,omitempty"`
	Question    string           `json:"question"`
	Description string           `json:"description,omitempty"`
	Choices     []QuestionChoice `json:"choices,omitempty"`
}

// QuestionChoice is a selectable option.
type QuestionChoice struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// RunComplete is the authoritative end-of-run signal for a session,
// emitted exactly once per top-level agent turn after all message
// updates for the turn have flushed. Clients that need a reliable
// completion contract (notably `sennit run`) should listen for this
// event filtered by RunID (preferred) — or
// by SessionID when no RunID was supplied — and use Text and
// MessageID to reconcile any output they have already streamed from
// earlier message events. Error is non-empty when the run terminated
// with an error; Cancelled is true when terminated due to context
// cancellation.
//
// RunID echoes the value the caller set on AgentMessage.RunID. It is
// the only safe correlator when the caller's prompt was queued
// behind a busy session: another turn's RunComplete for the same
// SessionID may arrive first, and filtering by SessionID alone
// would terminate the caller before its own turn ran.
type RunComplete struct {
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id,omitempty"`
	MessageID string `json:"message_id"`
	Text      string `json:"text,omitempty"`
	Error     string `json:"error,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`
}

// AgentMessage represents a message sent to the agent.
//
// RunID, when non-empty, is echoed back on the [RunComplete] event
// emitted for the resulting turn. Callers that need to correlate a
// specific SendMessage with its terminal event (notably
// `sennit run`, which may attach to a busy session whose currently
// running turn finishes first) should set it to a fresh unique
// value before the request. Server-side propagation flows through
// agent.WithRunID on the request context into the
// SessionAgentCall; it is preserved across the busy-session queue.
// When empty the resulting RunComplete carries an empty RunID and
// callers must fall back to SessionID-only filtering, which
// remains correct only when no other turns are in flight for the
// same session.
type AgentMessage struct {
	SessionID   string       `json:"session_id"`
	RunID       string       `json:"run_id,omitempty"`
	Prompt      string       `json:"prompt"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

// ShellCommandResponse represents the result of a direct shell command.
type ShellCommandResponse struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
}

// LSPClientInfo is the LSP client wire representation. It aliases the
// canonical leaf-package type, including its JSON error handling.
type LSPClientInfo = lsp.ClientInfo
