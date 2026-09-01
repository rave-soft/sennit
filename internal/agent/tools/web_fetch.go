package tools

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/rave-soft/sennit/internal/permission"
)

//go:embed web_fetch.md.tpl
var webFetchDescriptionTmpl []byte

var webFetchDescriptionTpl = template.Must(
	template.New("webFetchDescription").
		Parse(string(webFetchDescriptionTmpl)),
)

// NewWebFetchTool creates a web fetch tool that converts pages to markdown.
// When permissions is nil, the permission check is skipped entirely — used
// by the agentic_fetch sub-agent, whose own top-level call is already
// permission-gated.
func NewWebFetchTool(permissions permission.Requester, workingDir string, client *http.Client, options ...toolAvailabilityOption) fantasy.AgentTool {
	availability := applyToolAvailability(options)
	if client == nil {
		client = newHTTPClient(30 * time.Second)
	}

	return withToolParameterSchema(fantasy.NewAgentTool(
		WebFetchToolName,
		renderToolDescriptionWithAvailability(webFetchDescriptionTpl, availability),
		func(ctx context.Context, params WebFetchParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.URL == "" {
				return invalidParam("url"), nil
			}

			if permissions != nil {
				sessionID := GetSessionFromContext(ctx)
				if sessionID == "" {
					return fantasy.ToolResponse{}, missingSessionID("web_fetch")
				}

				permResp, denied, err := requirePermission(ctx, permissions, permission.CreatePermissionRequest{
					SessionID:   sessionID,
					Path:        workingDir,
					ToolCallID:  call.ID,
					ToolName:    WebFetchToolName,
					Action:      "fetch",
					Description: fmt.Sprintf("Fetch content from URL: %s", params.URL),
					Params:      WebFetchPermissionsParams(params),
				})
				if err != nil {
					return fantasy.ToolResponse{}, err
				}
				if denied {
					return permResp, nil
				}
			}

			content, filePath, err := FetchLargeContent(ctx, client, workingDir, params.URL)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to fetch URL: %s", err)), nil
			}

			var result strings.Builder
			if filePath != "" {
				fmt.Fprintf(&result, "Fetched content from %s (large page)\n\n", params.URL)
				fmt.Fprintf(&result, "Content saved to: %s\n\n", filePath)
				result.WriteString("Use the view and grep tools to analyze this file.")
			} else {
				fmt.Fprintf(&result, "Fetched content from %s:\n\n", params.URL)
				result.WriteString(content)
			}

			return fantasy.NewTextResponse(result.String()), nil
		},
	), map[string]toolParameterSchema{"url": {minLength: intPtr(1)}})
}
