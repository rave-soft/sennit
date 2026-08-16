package proto

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/brand"
)

const (
	AgentToolName            = "agent"
	JobOutputToolName        = "job_output"
	JobKillToolName          = "job_kill"
	WebFetchToolName         = "web_fetch"
	WebSearchToolName        = "web_search"
	TodosToolName            = "todos"
	QuestionToolName         = "question"
	ReferencesToolName       = "lsp_references"
	DefinitionToolName       = "lsp_definition"
	RenameToolName           = "lsp_rename"
	ReplaceSymbolToolName    = "lsp_replace_symbol"
	CallHierarchyToolName    = "lsp_call_hierarchy"
	SymbolsToolName          = "lsp_symbols"
	LSPRestartToolName       = "lsp_restart"
	DiagnosticsToolName      = "lsp_diagnostics"
	SennitInfoToolName       = brand.ToolInfo
	SennitLogsToolName       = brand.ToolLogs
	ListMCPResourcesToolName = "list_mcp_resources"
	ReadMCPResourceToolName  = "read_mcp_resource"
	ThreadCreateToolName     = "thread_create"
	ThreadListToolName       = "thread_list"
	ThreadStatusToolName     = "thread_status"
	ThreadSendToolName       = "thread_send"
	ThreadWaitToolName       = "thread_wait"
	ThreadMergeToolName      = "thread_merge"
	ThreadRemoveToolName     = "thread_remove"
	TaskListToolName         = "task_list"
	TaskResultToolName       = "task_result"
	TaskCancelToolName       = "task_cancel"
	TaskSendToolName         = "task_send"
	TaskOutputToolName       = "task_output"
	AskParentToolName        = "ask_parent"
	BashNoOutput             = "no output"
)

type AgentParams struct {
	Prompt     string `json:"prompt"`
	Background bool   `json:"background,omitempty"`
}

type AgentBackgroundResponseMetadata struct {
	TaskID    string `json:"task_id"`
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
}

type AgenticFetchParams struct {
	URL    string `json:"url,omitempty"`
	Prompt string `json:"prompt"`
}

type WebFetchParams struct {
	URL string `json:"url"`
}

type WebSearchParams struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results,omitempty"`
}

type TodoItem struct {
	Content    string `json:"content"`
	Status     string `json:"status"`
	ActiveForm string `json:"active_form"`
}

type TodosParams struct {
	Todos []TodoItem `json:"todos"`
}

type TodosResponseMetadata struct {
	IsNew         bool     `json:"is_new"`
	Todos         []Todo   `json:"todos"`
	JustCompleted []string `json:"just_completed,omitempty"`
	JustStarted   string   `json:"just_started,omitempty"`
	Completed     int      `json:"completed"`
	Total         int      `json:"total"`
}

type JobOutputParams struct {
	ShellID string `json:"shell_id"`
	Wait    bool   `json:"wait"`
}

type JobOutputResponseMetadata struct {
	ShellID          string `json:"shell_id"`
	Command          string `json:"command"`
	Description      string `json:"description"`
	Done             bool   `json:"done"`
	WorkingDirectory string `json:"working_directory"`
}

type JobKillParams struct {
	ShellID string `json:"shell_id"`
}

type JobKillResponseMetadata struct {
	ShellID     string `json:"shell_id"`
	Command     string `json:"command"`
	Description string `json:"description"`
}

type ReplaceSymbolParams struct {
	Symbol      string `json:"symbol"`
	FilePath    string `json:"file_path"`
	Replacement string `json:"replacement,omitempty"`
	Action      string `json:"action,omitempty"`
}

type ReplaceSymbolResponseMetadata struct {
	FilePath   string `json:"file_path"`
	OldContent string `json:"old_content"`
	NewContent string `json:"new_content"`
	Action     string `json:"action"`
}

type RenameParams struct {
	Symbol  string `json:"symbol"`
	NewName string `json:"new_name"`
	Path    string `json:"path,omitempty"`
}

type ReferencesParams struct {
	Symbol string `json:"symbol"`
	Path   string `json:"path,omitempty"`
}

type QuestionParams struct {
	Questions          []QuestionItem `json:"questions"`
	ConfirmTitle       string         `json:"confirm_title,omitempty"`
	ConfirmDescription string         `json:"confirm_description,omitempty"`
}

func (p *QuestionParams) UnmarshalJSON(data []byte) error {
	type alias QuestionParams
	aux := struct {
		Questions json.RawMessage `json:"questions"`
		*alias
	}{alias: (*alias)(p)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if len(aux.Questions) == 0 {
		return nil
	}
	if err := json.Unmarshal(aux.Questions, &p.Questions); err != nil {
		var encoded string
		if stringErr := json.Unmarshal(aux.Questions, &encoded); stringErr != nil {
			return err
		}
		if stringErr := json.Unmarshal([]byte(strings.TrimSpace(encoded)), &p.Questions); stringErr != nil {
			return fmt.Errorf("questions must be an array: %w", stringErr)
		}
	}
	return nil
}

type LSPRestartParams struct {
	Name string `json:"name,omitempty"`
}

type SymbolsParams struct {
	FilePath string `json:"file_path"`
}

type CallHierarchyParams struct {
	Symbol    string `json:"symbol"`
	Direction string `json:"direction"`
	Path      string `json:"path,omitempty"`
}

type DefinitionParams struct {
	Symbol string `json:"symbol"`
	Path   string `json:"path,omitempty"`
}

type DefinitionResponseMetadata struct {
	FilePath string `json:"file_path"`
	Line     int    `json:"line"`
	Content  string `json:"content"`
}

type ReplaceSymbolPermissionsParams = tools.ReplaceSymbolPermissionsParams
