package proto

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
