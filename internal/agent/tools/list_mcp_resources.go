package tools

import (
	"cmp"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"sort"
	"strings"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/filepathext"
	"github.com/rave-soft/sennit/internal/permission"
)

// mcpResourceLister is the subset of *mcp.Registry that
// NewListMCPResourcesTool needs. Narrowing it to an interface lets a test
// substitute a fake that returns context.Canceled without standing up a
// real, connected MCP session.
type mcpResourceLister interface {
	ListResources(ctx context.Context, cfg mcp.ConfigProvider, name string) ([]*mcp.Resource, error)
}

type ListMCPResourcesParams struct {
	MCPName string `json:"mcp_name" description:"The MCP server name"`
}

type ListMCPResourcesPermissionsParams struct {
	MCPName string `json:"mcp_name"`
}

const ListMCPResourcesToolName = "list_mcp_resources"

//go:embed list_mcp_resources.md
var listMCPResourcesDescription string

// mcpResourceConfig is the slice of *config.ConfigStore the MCP resource
// tools (list_mcp_resources, read_mcp_resource) need: the working directory
// for permission-path resolution, plus whatever mcp.Registry's
// ListResources/ReadResource require. Declaring it here rather than
// accepting the concrete *config.ConfigStore keeps this package's
// dependency on config narrow (ISP).
type mcpResourceConfig interface {
	mcp.ConfigProvider
	WorkingDir() string
}

var _ mcpResourceConfig = (*config.ConfigStore)(nil)

func NewListMCPResourcesTool(cfg mcpResourceConfig, reg mcpResourceLister, permissions permission.Requester) fantasy.AgentTool {
	return withToolParameterSchema(fantasy.NewAgentTool(
		ListMCPResourcesToolName,
		listMCPResourcesDescription,
		func(ctx context.Context, params ListMCPResourcesParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			params.MCPName = strings.TrimSpace(params.MCPName)
			if params.MCPName == "" {
				return invalidParam("mcp_name"), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, missingSessionID("listing MCP resources")
			}

			relPath := filepathext.SmartJoin(cfg.WorkingDir(), params.MCPName)
			resp, denied, err := requirePermission(ctx, permissions, permission.CreatePermissionRequest{
				SessionID:   sessionID,
				Path:        relPath,
				ToolCallID:  call.ID,
				ToolName:    ListMCPResourcesToolName,
				Action:      "list",
				Description: fmt.Sprintf("List MCP resources from %s", params.MCPName),
				Params:      ListMCPResourcesPermissionsParams(params),
			})
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if denied {
				return resp, nil
			}

			// Checked before the round trip, not only on its error path: a
			// canceled context does not force ListResources to fail, and a
			// listing that arrives anyway would be recorded as an ordinary
			// tool result for a call the user already abandoned.
			if err := ctx.Err(); err != nil {
				return fantasy.ToolResponse{}, err
			}

			resources, err := reg.ListResources(ctx, cfg, params.MCPName)
			if err != nil {
				// See mcp-tools.go's Run for why cancellation must propagate
				// as a Go error instead of a normal-looking tool result.
				if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return fantasy.ToolResponse{}, fmt.Errorf("list MCP resources: %w", err)
				}
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if len(resources) == 0 {
				return fantasy.NewTextResponse("No resources found"), nil
			}

			lines := make([]string, 0, len(resources))
			for _, resource := range resources {
				if resource == nil {
					continue
				}
				title := cmp.Or(resource.Title, resource.Name, resource.URI)
				line := fmt.Sprintf("- %s", title)
				if resource.URI != "" {
					line = fmt.Sprintf("%s (%s)", line, resource.URI)
				}
				if resource.Description != "" {
					line = fmt.Sprintf("%s: %s", line, resource.Description)
				}
				if resource.MIMEType != "" {
					line = fmt.Sprintf("%s [mime: %s]", line, resource.MIMEType)
				}
				if resource.Size > 0 {
					line = fmt.Sprintf("%s [size: %d]", line, resource.Size)
				}
				lines = append(lines, line)
			}

			sort.Strings(lines)
			return fantasy.NewTextResponse(strings.Join(lines, "\n")), nil
		},
	), map[string]toolParameterSchema{"mcp_name": {minLength: intPtr(1)}})
}
