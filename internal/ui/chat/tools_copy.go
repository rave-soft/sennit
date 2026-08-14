package chat

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rave-soft/braid/internal/fsext"
	tools "github.com/rave-soft/braid/internal/proto"
)

// formatToolForCopy formats the tool call for clipboard copying.
func (t *baseToolMessageItem) formatToolForCopy() string {
	var parts []string

	toolName := prettifyToolName(t.toolCall.Name)
	parts = append(parts, fmt.Sprintf("## %s Tool Call", toolName))

	if t.toolCall.Input != "" {
		params := t.formatParametersForCopy()
		if params != "" {
			parts = append(parts, "### Parameters:")
			parts = append(parts, params)
		}
	}

	if t.result != nil && t.result.ToolCallID != "" {
		if t.result.IsError {
			parts = append(parts, "### Error:")
			parts = append(parts, t.result.Content)
		} else {
			parts = append(parts, "### Result:")
			content := t.formatResultForCopy()
			if content != "" {
				parts = append(parts, content)
			}
		}
	} else if t.status == ToolStatusCanceled {
		parts = append(parts, "### Status:")
		parts = append(parts, "Cancelled")
	} else {
		parts = append(parts, "### Status:")
		parts = append(parts, "Pending...")
	}

	return strings.Join(parts, "\n\n")
}

// formatParametersForCopy formats tool parameters for clipboard copying.
func (t *baseToolMessageItem) formatParametersForCopy() string {
	switch t.toolCall.Name {
	case tools.BashToolName:
		var params tools.BashParams
		if json.Unmarshal([]byte(t.toolCall.Input), &params) == nil {
			cmd := strings.ReplaceAll(params.Command, "\n", " ")
			cmd = strings.ReplaceAll(cmd, "\t", "    ")
			return fmt.Sprintf("**Command:** %s", cmd)
		}
	case tools.ViewToolName:
		var params tools.ViewParams
		if json.Unmarshal([]byte(t.toolCall.Input), &params) == nil {
			var parts []string
			parts = append(parts, fmt.Sprintf("**File:** %s", fsext.PrettyPath(params.FilePath)))
			if params.Limit > 0 {
				parts = append(parts, fmt.Sprintf("**Limit:** %d", params.Limit))
			}
			if params.Offset > 0 {
				parts = append(parts, fmt.Sprintf("**Offset:** %d", params.Offset))
			}
			return strings.Join(parts, "\n")
		}
	case tools.EditToolName:
		var params tools.EditParams
		if json.Unmarshal([]byte(t.toolCall.Input), &params) == nil {
			return fmt.Sprintf("**File:** %s", fsext.PrettyPath(params.FilePath))
		}
	case tools.MultiEditToolName:
		var params tools.MultiEditParams
		if json.Unmarshal([]byte(t.toolCall.Input), &params) == nil {
			var parts []string
			parts = append(parts, fmt.Sprintf("**File:** %s", fsext.PrettyPath(params.FilePath)))
			parts = append(parts, fmt.Sprintf("**Edits:** %d", len(params.Edits)))
			return strings.Join(parts, "\n")
		}
	case tools.WriteToolName:
		var params tools.WriteParams
		if json.Unmarshal([]byte(t.toolCall.Input), &params) == nil {
			return fmt.Sprintf("**File:** %s", fsext.PrettyPath(params.FilePath))
		}
	case tools.FetchToolName:
		var params tools.FetchParams
		if json.Unmarshal([]byte(t.toolCall.Input), &params) == nil {
			var parts []string
			parts = append(parts, fmt.Sprintf("**URL:** %s", params.URL))
			if params.Format != "" {
				parts = append(parts, fmt.Sprintf("**Format:** %s", params.Format))
			}
			if params.Timeout > 0 {
				parts = append(parts, fmt.Sprintf("**Timeout:** %ds", params.Timeout))
			}
			return strings.Join(parts, "\n")
		}
	case tools.AgenticFetchToolName:
		var params tools.AgenticFetchParams
		if json.Unmarshal([]byte(t.toolCall.Input), &params) == nil {
			var parts []string
			if params.URL != "" {
				parts = append(parts, fmt.Sprintf("**URL:** %s", params.URL))
			}
			if params.Prompt != "" {
				parts = append(parts, fmt.Sprintf("**Prompt:** %s", params.Prompt))
			}
			return strings.Join(parts, "\n")
		}
	case tools.WebFetchToolName:
		var params tools.WebFetchParams
		if json.Unmarshal([]byte(t.toolCall.Input), &params) == nil {
			return fmt.Sprintf("**URL:** %s", params.URL)
		}
	case tools.GrepToolName, tools.RipgrepToolName:
		var params tools.RipgrepParams
		if json.Unmarshal([]byte(t.toolCall.Input), &params) == nil {
			var parts []string
			parts = append(parts, fmt.Sprintf("**Pattern:** %s", params.Pattern))
			if params.Path != "" {
				parts = append(parts, fmt.Sprintf("**Path:** %s", params.Path))
			}
			if params.Include != "" {
				parts = append(parts, fmt.Sprintf("**Include:** %s", params.Include))
			}
			if params.LiteralText {
				parts = append(parts, "**Literal:** true")
			}
			return strings.Join(parts, "\n")
		}
	case tools.GlobToolName:
		var params tools.GlobParams
		if json.Unmarshal([]byte(t.toolCall.Input), &params) == nil {
			var parts []string
			parts = append(parts, fmt.Sprintf("**Pattern:** %s", params.Pattern))
			if params.Path != "" {
				parts = append(parts, fmt.Sprintf("**Path:** %s", params.Path))
			}
			return strings.Join(parts, "\n")
		}
	case tools.LSToolName:
		var params tools.LSParams
		if json.Unmarshal([]byte(t.toolCall.Input), &params) == nil {
			path := params.Path
			if path == "" {
				path = "."
			}
			return fmt.Sprintf("**Path:** %s", fsext.PrettyPath(path))
		}
	case tools.DownloadToolName:
		var params tools.DownloadParams
		if json.Unmarshal([]byte(t.toolCall.Input), &params) == nil {
			var parts []string
			parts = append(parts, fmt.Sprintf("**URL:** %s", params.URL))
			parts = append(parts, fmt.Sprintf("**File Path:** %s", fsext.PrettyPath(params.FilePath)))
			if params.Timeout > 0 {
				parts = append(parts, fmt.Sprintf("**Timeout:** %s", (time.Duration(params.Timeout)*time.Second).String()))
			}
			return strings.Join(parts, "\n")
		}
	case tools.DiagnosticsToolName:
		return "**Project:** diagnostics"
	case tools.AgentToolName:
		var params tools.AgentParams
		if json.Unmarshal([]byte(t.toolCall.Input), &params) == nil {
			return fmt.Sprintf("**Task:**\n%s", params.Prompt)
		}
	}

	var params map[string]any
	if json.Unmarshal([]byte(t.toolCall.Input), &params) == nil {
		var parts []string
		for key, value := range params {
			displayKey := strings.ReplaceAll(key, "_", " ")
			if len(displayKey) > 0 {
				displayKey = strings.ToUpper(displayKey[:1]) + displayKey[1:]
			}
			parts = append(parts, fmt.Sprintf("**%s:** %v", displayKey, value))
		}
		return strings.Join(parts, "\n")
	}

	return ""
}

// formatResultForCopy formats tool results for clipboard copying.
func (t *baseToolMessageItem) formatResultForCopy() string {
	if t.result == nil {
		return ""
	}

	if t.result.Data != "" {
		if strings.HasPrefix(t.result.MIMEType, "image/") {
			return fmt.Sprintf("[Image: %s]", t.result.MIMEType)
		}
		return fmt.Sprintf("[Media: %s]", t.result.MIMEType)
	}

	switch t.toolCall.Name {
	case tools.BashToolName:
		return t.formatBashResultForCopy()
	case tools.ViewToolName:
		return t.formatViewResultForCopy()
	case tools.EditToolName:
		return t.formatEditResultForCopy()
	case tools.MultiEditToolName:
		return t.formatMultiEditResultForCopy()
	case tools.WriteToolName:
		return t.formatWriteResultForCopy()
	case tools.FetchToolName:
		return t.formatFetchResultForCopy()
	case tools.AgenticFetchToolName:
		return t.formatAgenticFetchResultForCopy()
	case tools.WebFetchToolName:
		return t.formatWebFetchResultForCopy()
	case tools.AgentToolName:
		return t.formatAgentResultForCopy()
	case tools.DownloadToolName, tools.GrepToolName, tools.RipgrepToolName, tools.GlobToolName, tools.LSToolName, tools.DiagnosticsToolName, tools.TodosToolName:
		return fmt.Sprintf("```\n%s\n```", t.result.Content)
	default:
		return t.result.Content
	}
}

// prettifyToolName returns a human-readable name for tool names.
func prettifyToolName(name string) string {
	switch name {
	case tools.AgentToolName:
		return "Agent"
	case tools.BashToolName:
		return "Bash"
	case tools.JobOutputToolName:
		return "Job: Output"
	case tools.JobKillToolName:
		return "Job: Kill"
	case tools.DownloadToolName:
		return "Download"
	case tools.EditToolName:
		return "Edit"
	case tools.MultiEditToolName:
		return "Multi-Edit"
	case tools.FetchToolName:
		return "Fetch"
	case tools.AgenticFetchToolName:
		return "Agentic Fetch"
	case tools.WebFetchToolName:
		return "Fetch"
	case tools.WebSearchToolName:
		return "Search"
	case tools.GlobToolName:
		return "Glob"
	case tools.GrepToolName:
		return "Grep"
	case tools.RipgrepToolName:
		return "Ripgrep"
	case tools.LSToolName:
		return "List"
	case tools.TodosToolName:
		return "To-Do"
	case tools.ViewToolName:
		return "Read"
	case tools.WriteToolName:
		return "Write"
	default:
		return humanizedToolName(name)
	}
}
