package tools

import (
	"cmp"
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"strings"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/filepathext"
	"github.com/rave-soft/sennit/internal/permission"
)

type ReadMCPResourceParams struct {
	MCPName string `json:"mcp_name" description:"The MCP server name"`
	URI     string `json:"uri" description:"The resource URI to read"`
}

type ReadMCPResourcePermissionsParams struct {
	MCPName string `json:"mcp_name"`
	URI     string `json:"uri"`
}

const ReadMCPResourceToolName = "read_mcp_resource"

//go:embed read_mcp_resource.md
var readMCPResourceDescription string

// mcpResourceReader is the subset of *mcp.Registry that
// NewReadMCPResourceTool needs, narrowed to an interface (mirroring
// mcpResourceLister above) so tests can substitute a fake that returns
// arbitrary resource contents - including oversized or binary ones,
// exercising G13's size/MIME handling - without standing up a real,
// connected MCP session.
type mcpResourceReader interface {
	ReadResource(ctx context.Context, cfg mcp.ConfigProvider, name, uri string) ([]*mcp.ResourceContents, error)
}

func NewReadMCPResourceTool(cfg mcpResourceConfig, reg mcpResourceReader, permissions permission.Requester) fantasy.AgentTool {
	return withToolParameterSchema(fantasy.NewAgentTool(
		ReadMCPResourceToolName,
		readMCPResourceDescription,
		func(ctx context.Context, params ReadMCPResourceParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			params.MCPName = strings.TrimSpace(params.MCPName)
			params.URI = strings.TrimSpace(params.URI)
			if params.MCPName == "" {
				return invalidParam("mcp_name"), nil
			}
			if params.URI == "" {
				return invalidParam("uri"), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, missingSessionID("reading MCP resources")
			}

			relPath := filepathext.SmartJoin(cfg.WorkingDir(), cmp.Or(params.URI, "mcp-resource"))
			resp, denied, err := requirePermission(ctx, permissions, permission.CreatePermissionRequest{
				SessionID:   sessionID,
				Path:        relPath,
				ToolCallID:  call.ID,
				ToolName:    ReadMCPResourceToolName,
				Action:      "read",
				Description: fmt.Sprintf("Read MCP resource from %s", params.MCPName),
				Params:      ReadMCPResourcePermissionsParams(params),
			})
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if denied {
				return resp, nil
			}

			contents, err := reg.ReadResource(ctx, cfg, params.MCPName, params.URI)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if len(contents) == 0 {
				return fantasy.NewTextResponse(""), nil
			}

			// A resource read that comes back as exactly one image is
			// handed to the model as a normal image response, the same way
			// the read tool and mcp-tools.go's RunTool do (read.go,
			// mcp-tools.go). Mixed or multi-part results fall through to
			// the text path below, where a binary part that isn't the
			// sole image gets a size/MIME description instead - fantasy's
			// ToolResponse carries at most one image, so there is no
			// sensible way to return more than one here.
			if len(contents) == 1 && contents[0] != nil && len(contents[0].Blob) > 0 &&
				mcp.ResourceContentIsImage(contents[0].MIMEType) {
				if !GetSupportsImagesFromContext(ctx) {
					modelName := GetModelNameFromContext(ctx)
					return fantasy.NewTextErrorResponse(fmt.Sprintf("This model (%s) does not support image data.", modelName)), nil
				}
				if len(contents[0].Blob) > mcp.MaxResourceContentBytes {
					return fantasy.NewTextErrorResponse(fmt.Sprintf(
						"MCP resource %s is too large to return (%d bytes, %s). Maximum size is %d bytes.",
						params.URI, len(contents[0].Blob), contents[0].MIMEType, mcp.MaxResourceContentBytes,
					)), nil
				}
				return fantasy.NewImageResponse(contents[0].Blob, contents[0].MIMEType), nil
			}

			var textParts []string
			for _, content := range contents {
				if content == nil {
					continue
				}
				if content.Text == "" && len(content.Blob) == 0 {
					slog.Debug("MCP resource content missing text/blob", "uri", content.URI)
					continue
				}
				// content.Text is already textual by construction (the
				// server put it in the text field); a populated Blob is
				// only safe to decode as text when its MIME type says so
				// (text/* or a JSON variant). Anything else is binary the
				// model has no use for as a garbled string - G13: this used
				// to be string(content.Blob) unconditionally, so a PDF or
				// image landed in the model's context as invalid UTF-8.
				if content.Text == "" && !mcp.ResourceContentIsText(content.MIMEType) {
					return fantasy.NewTextErrorResponse(fmt.Sprintf(
						"MCP resource %s is binary (%s, %d bytes) and cannot be read as text.",
						cmp.Or(content.URI, params.URI), cmp.Or(content.MIMEType, "unknown MIME type"), len(content.Blob),
					)), nil
				}
				if text := mcp.FormatResourceContentsText(content); text != "" {
					textParts = append(textParts, text)
				}
			}

			if len(textParts) == 0 {
				return fantasy.NewTextResponse(""), nil
			}

			return fantasy.NewTextResponse(strings.Join(textParts, "\n")), nil
		},
	), map[string]toolParameterSchema{"mcp_name": {minLength: intPtr(1)}, "uri": {minLength: intPtr(1)}})
}
