package tools

import (
	"context"
	"fmt"
	"slices"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/permission"
)

// whitelistDockerTools lists tools of the managed Docker MCP gateway that
// run without a permission request. The bypass applies only to the built-in
// gateway (`docker mcp gateway run`): a user-configured server that merely
// shares the name "docker" gets no exemption, so the check goes through
// isWhitelistedDockerTool rather than matching on the tool name alone.
var whitelistDockerTools = []string{
	"mcp-find",
	"mcp-add",
	"mcp-remove",
	"mcp-config-set",
	"code-mode",
}

// GetMCPTools gets all the currently available MCP tools from reg, the
// caller's per-workspace MCP registry.
func GetMCPTools(reg *mcp.Registry, permissions permission.Service, cfg mcp.ConfigProvider, wd string) []*Tool {
	if reg == nil {
		return nil
	}
	var result []*Tool
	for mcpName, tools := range reg.Tools() {
		for _, tool := range tools {
			result = append(result, &Tool{
				mcpName:     mcpName,
				tool:        tool,
				permissions: permissions,
				workingDir:  wd,
				cfg:         cfg,
				reg:         reg,
			})
		}
	}
	return result
}

// Tool is a tool from a MCP.
type Tool struct {
	mcpName         string
	tool            *mcp.Tool
	cfg             mcp.ConfigProvider
	permissions     permission.Service
	workingDir      string
	providerOptions fantasy.ProviderOptions
	reg             *mcp.Registry
}

func (m *Tool) SetProviderOptions(opts fantasy.ProviderOptions) {
	m.providerOptions = opts
}

func (m *Tool) ProviderOptions() fantasy.ProviderOptions {
	return m.providerOptions
}

func (m *Tool) Name() string {
	return fmt.Sprintf("mcp_%s_%s", m.mcpName, m.tool.Name)
}

func (m *Tool) MCP() string {
	return m.mcpName
}

func (m *Tool) MCPToolName() string {
	return m.tool.Name
}

func (m *Tool) Info() fantasy.ToolInfo {
	parameters := make(map[string]any)
	required := make([]string, 0)

	if input, ok := m.tool.InputSchema.(map[string]any); ok {
		if props, ok := input["properties"].(map[string]any); ok {
			parameters = props
		}
		if req, ok := input["required"].([]any); ok {
			// Convert []any -> []string when elements are strings
			for _, v := range req {
				if s, ok := v.(string); ok {
					required = append(required, s)
				}
			}
		} else if reqStr, ok := input["required"].([]string); ok {
			// Handle case where it's already []string
			required = reqStr
		}
	}

	return fantasy.ToolInfo{
		Name:        m.Name(),
		Description: m.tool.Description,
		Parameters:  parameters,
		Required:    required,
	}
}

// isWhitelistedDockerTool reports whether this tool belongs to the managed
// Docker MCP gateway and is on the no-permission whitelist. It fails closed:
// any deviation of the configured server from the built-in gateway command
// means the server is user-controlled and every call goes through the
// normal permission flow.
func (m *Tool) isWhitelistedDockerTool() bool {
	if m.mcpName != config.DockerMCPName || !slices.Contains(whitelistDockerTools, m.tool.Name) {
		return false
	}
	if m.cfg == nil {
		return false
	}
	cfg := m.cfg.Config()
	if cfg == nil || cfg.MCP == nil {
		return false
	}
	mc, ok := cfg.MCP[config.DockerMCPName]
	if !ok {
		return false
	}
	managed := config.DockerMCPConfig()
	return mc.Type == managed.Type && mc.Command == managed.Command && slices.Equal(mc.Args, managed.Args)
}

func (m *Tool) Run(ctx context.Context, params fantasy.ToolCall) (fantasy.ToolResponse, error) {
	sessionID := GetSessionFromContext(ctx)
	if sessionID == "" {
		return fantasy.ToolResponse{}, missingSessionID("running the MCP tool")
	}

	// Skip permission for whitelisted tools of the managed Docker MCP.
	if !m.isWhitelistedDockerTool() {
		permissionDescription := fmt.Sprintf("execute %s with the following parameters:", m.Info().Name)
		resp, denied, err := requirePermission(ctx, m.permissions, permission.CreatePermissionRequest{
			SessionID:   sessionID,
			ToolCallID:  params.ID,
			Path:        m.workingDir,
			ToolName:    m.Info().Name,
			Action:      "execute",
			Description: permissionDescription,
			Params:      params.Input,
		})
		if err != nil {
			return fantasy.ToolResponse{}, err
		}
		if denied {
			return resp, nil
		}
	}

	result, err := m.reg.RunTool(ctx, m.cfg, m.mcpName, m.tool.Name, params.Input)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	// The server ran the tool and the tool reported failure. Returned as
	// an ordinary response, that reached the model as a success and it
	// carried on as though the call had worked.
	if result.IsError {
		content := result.Content
		if content == "" {
			content = fmt.Sprintf("MCP tool %q reported an error with no message", m.tool.Name)
		}
		return fantasy.NewTextErrorResponse(content), nil
	}

	switch result.Type {
	case "image", "media":
		if !GetSupportsImagesFromContext(ctx) {
			modelName := GetModelNameFromContext(ctx)
			return fantasy.NewTextErrorResponse(fmt.Sprintf("This model (%s) does not support image data.", modelName)), nil
		}

		var response fantasy.ToolResponse
		if result.Type == "image" {
			response = fantasy.NewImageResponse(result.Data, result.MediaType)
		} else {
			response = fantasy.NewMediaResponse(result.Data, result.MediaType)
		}
		response.Content = result.Content
		return response, nil
	default:
		return fantasy.NewTextResponse(result.Content), nil
	}
}
