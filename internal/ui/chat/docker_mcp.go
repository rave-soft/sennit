package chat

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/rave-soft/sennit/internal/mcpid"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/stringext"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

// NewDockerMCPToolMessageItem creates a new Docker MCP tool message item.
func NewDockerMCPToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) ToolMessageItem {
	return newBaseToolMessageItem(sty, toolCall, result, &DockerMCPToolRenderContext{}, canceled)
}

// DockerMCPToolRenderContext renders Docker MCP tool messages.
type DockerMCPToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (d *DockerMCPToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	var params map[string]any
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err != nil {
		params = make(map[string]any)
	}

	tool := strings.TrimPrefix(opts.ToolCall.Name, "mcp_"+mcpid.DockerMCPName+"_")

	mainParam := opts.ToolCall.Input
	extraArgs := map[string]string{}
	switch tool {
	case "mcp-find":
		if query, ok := params["query"]; ok {
			if qStr, ok := query.(string); ok {
				mainParam = qStr
			}
		}
		for k, v := range params {
			if k == "query" {
				continue
			}
			data, _ := json.Marshal(v)
			extraArgs[k] = string(data)
		}
	case "mcp-add":
		if name, ok := params["name"]; ok {
			if nStr, ok := name.(string); ok {
				mainParam = nStr
			}
		}
		for k, v := range params {
			if k == "name" {
				continue
			}
			data, _ := json.Marshal(v)
			extraArgs[k] = string(data)
		}
	case "mcp-remove":
		if name, ok := params["name"]; ok {
			if nStr, ok := name.(string); ok {
				mainParam = nStr
			}
		}
		for k, v := range params {
			if k == "name" {
				continue
			}
			data, _ := json.Marshal(v)
			extraArgs[k] = string(data)
		}
	case "mcp-exec":
		if name, ok := params["name"]; ok {
			if nStr, ok := name.(string); ok {
				mainParam = nStr
			}
		}
	case "mcp-config-set":
		if server, ok := params["server"]; ok {
			if sStr, ok := server.(string); ok {
				mainParam = sStr
			}
		}
	}

	var toolParams []string
	toolParams = append(toolParams, mainParam)
	keys := make([]string, 0, len(extraArgs))
	for k := range extraArgs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		toolParams = append(toolParams, k, extraArgs[k])
	}

	if opts.IsPending() {
		// Never nested, whatever opts.Compact says - a Docker MCP call is
		// always rendered at top level - so this goes through the line
		// helper rather than pendingTool.
		return pendingToolLine(sty, d.formatToolName(sty, tool), opts.Anim, false, len(opts.ToolCall.Input))
	}

	header := d.makeHeader(sty, tool, width, opts, toolParams...)
	if opts.Compact {
		return header
	}

	if earlyState, ok := toolEarlyStateContent(sty, opts, width); ok {
		return joinToolParts(header, earlyState)
	}

	if tool == "mcp-find" {
		return joinToolParts(header, d.renderMCPServers(sty, opts, width))
	}

	if !opts.HasResult() {
		return header
	}

	// Image results still get their (already one-line) summary; text
	// results collapse to the same "N lines" summary as every other tool.
	if opts.Result.Data != "" && strings.HasPrefix(opts.Result.MIMEType, "image/") {
		body := sty.Tool.Body.Render(toolOutputImageContent(sty, opts.Result.Data, opts.Result.MIMEType))
		return joinToolParts(header, body)
	}

	return appendResultSummary(sty, header, lineCountSummary(opts.Result.Content))
}

// FindMCPResponse represents the response from mcp-find.
type FindMCPResponse struct {
	Servers []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"servers"`
}

func (d *DockerMCPToolRenderContext) renderMCPServers(sty *styles.Styles, opts *ToolRenderOpts, width int) string {
	if !opts.HasResult() || opts.Result.Content == "" {
		return ""
	}

	var result FindMCPResponse
	if err := json.Unmarshal([]byte(opts.Result.Content), &result); err != nil {
		return toolOutputPlainContent(sty, opts.Result.Content, width-toolBodyLeftPaddingTotal)
	}

	if len(result.Servers) == 0 {
		return sty.Tool.ResultEmpty.Render("No MCP servers found.")
	}

	bodyWidth := min(120, width) - toolBodyLeftPaddingTotal
	rows := [][]string{}
	moreServers := ""
	for i, server := range result.Servers {
		if i > 9 {
			moreServers = sty.Tool.ResultTruncation.Render(fmt.Sprintf("... and %d more", len(result.Servers)-10))
			break
		}
		rows = append(rows, []string{sty.Tool.ResultItemName.Render(server.Name), sty.Tool.ResultItemDesc.Render(server.Description)})
	}
	serverTable := table.New().
		Wrap(false).
		BorderTop(false).
		BorderBottom(false).
		BorderRight(false).
		BorderLeft(false).
		BorderColumn(false).
		BorderRow(false).
		StyleFunc(func(row, col int) lipgloss.Style {
			if row == table.HeaderRow {
				return lipgloss.NewStyle()
			}
			switch col {
			case 0:
				return lipgloss.NewStyle().PaddingRight(1)
			}
			return lipgloss.NewStyle()
		}).Rows(rows...).Width(bodyWidth)
	if moreServers != "" {
		return sty.Tool.Body.Render(serverTable.Render() + "\n" + moreServers)
	}
	return sty.Tool.Body.Render(serverTable.Render())
}

func (d *DockerMCPToolRenderContext) makeHeader(sty *styles.Styles, tool string, width int, opts *ToolRenderOpts, params ...string) string {
	if opts.Compact {
		return d.makeCompactHeader(sty, tool, width, params...)
	}

	icon := toolIcon(sty, opts.Status)
	if opts.IsPending() {
		icon = sty.Tool.IconPending.Render()
	}
	prefix := fmt.Sprintf("%s %s ", icon, d.formatToolName(sty, tool))
	return prefix + toolParamList(sty, params, width-lipgloss.Width(prefix))
}

func (d *DockerMCPToolRenderContext) formatToolName(sty *styles.Styles, tool string) string {
	mainTool := "Docker MCP"
	var action string
	actionStyle := sty.Tool.MCPToolName
	switch tool {
	case "mcp-exec":
		action = "Exec"
	case "mcp-config-set":
		action = "Config Set"
	case "mcp-find":
		action = "Find"
	case "mcp-add":
		action = "Add"
		actionStyle = sty.Tool.ActionCreate
	case "mcp-remove":
		action = "Remove"
		actionStyle = sty.Tool.ActionDestroy
	case "code-mode":
		action = "Code Mode"
	default:
		action = strings.ReplaceAll(tool, "-", " ")
		action = strings.ReplaceAll(action, "_", " ")
		action = stringext.Capitalize(action)
	}

	toolNameStyled := sty.Tool.MCPName.Render(mainTool)
	arrow := sty.Tool.MCPArrow.String()
	return fmt.Sprintf("%s %s %s", toolNameStyled, arrow, actionStyle.Render(action))
}

func (d *DockerMCPToolRenderContext) makeCompactHeader(sty *styles.Styles, tool string, width int, params ...string) string {
	var action string
	switch tool {
	case "mcp-exec":
		action = "exec"
	case "mcp-config-set":
		action = "config-set"
	case "mcp-find":
		action = "find"
	case "mcp-add":
		action = "add"
	case "mcp-remove":
		action = "remove"
	case "code-mode":
		action = "code-mode"
	default:
		action = strings.ReplaceAll(tool, "-", " ")
		action = strings.ReplaceAll(action, "_", " ")
	}

	name := fmt.Sprintf("Docker MCP: %s", action)
	return toolHeader(sty, ToolStatusSuccess, name, width, &ToolRenderOpts{Compact: true}, params...)
}

// IsDockerMCPTool returns true if the tool name is a Docker MCP tool.
func IsDockerMCPTool(name string) bool {
	return strings.HasPrefix(name, "mcp_"+mcpid.DockerMCPName+"_")
}
