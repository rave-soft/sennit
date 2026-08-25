package proto

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

// AgentMessage represents a message sent to the agent.
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
