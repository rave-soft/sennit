package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/permission"
)

// mcpToolRunner is the subset of *mcp.Registry that Tool.Run needs to
// invoke the underlying MCP tool. Narrowing it to an interface lets a
// test substitute a fake that returns context.Canceled without standing
// up a real, connected MCP session.
type mcpToolRunner interface {
	RunTool(ctx context.Context, cfg mcp.ConfigProvider, name, toolName string, input string) (mcp.ToolResult, error)
}

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
func GetMCPTools(reg *mcp.Registry, permissions permission.Requester, cfg mcp.ConfigProvider, wd string) []*Tool {
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
	permissions     permission.Requester
	workingDir      string
	providerOptions fantasy.ProviderOptions
	reg             mcpToolRunner
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

func schemaRequired(value any) []string {
	switch required := value.(type) {
	case []string:
		return append([]string(nil), required...)
	case []any:
		result := make([]string, 0, len(required))
		for _, value := range required {
			if name, ok := value.(string); ok {
				result = append(result, name)
			}
		}
		return result
	default:
		return []string{}
	}
}

func (m *Tool) Info() fantasy.ToolInfo {
	// Registration validates schemas before publication. Clone again here so a
	// caller cannot mutate an MCP SDK-owned schema through ToolInfo.
	inputSchema, err := mcp.CloneToolSchema(m.tool.InputSchema)
	if err != nil {
		// An invalid tool is never published; retain a safe legacy shape if a
		// caller races a mutable third-party SDK object after registration.
		inputSchema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	parameters, _ := inputSchema["properties"].(map[string]any)
	if parameters == nil {
		parameters = map[string]any{}
	}
	return fantasy.ToolInfo{
		Name:        m.Name(),
		Description: m.tool.Description,
		Parameters:  parameters,
		Required:    schemaRequired(inputSchema["required"]),
		InputSchema: inputSchema,
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
		// mcp.ErrLostOwnership means this attempt merely lost a race against
		// a concurrent reconnect/teardown/auth flow (e.g. a lazy renewal of
		// a dropped stdio server overlapping a config edit) - ctx.Err() is
		// nil, the server is fine, and the model can just retry the call
		// against the freshly (re)established session. It falls through to
		// the same text-response branch as the check below would give it
		// anyway (it is neither Canceled nor DeadlineExceeded), but this
		// makes that guarantee explicit rather than incidental, since a
		// future change to the check below could otherwise start
		// misclassifying it.
		if errors.Is(err, mcp.ErrLostOwnership) {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		// Cancellation (Esc on a queued tool call, a hook timeout, ...) is
		// not something the model can react to — it means the turn itself
		// is over, not that this call failed and can be retried — so it
		// must abort the tool-call batch as a Go error rather than land in
		// the transcript as an ordinary "context canceled" text result.
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return fantasy.ToolResponse{}, fmt.Errorf("run MCP tool %s: %w", m.Name(), err)
		}
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
