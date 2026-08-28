package tools

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"charm.land/fantasy"

	"github.com/rave-soft/sennit/internal/permission"
)

//go:embed web_search.md.tpl
var webSearchDescriptionTmpl []byte

var webSearchDescriptionTpl = template.Must(
	template.New("webSearchDescription").
		Parse(string(webSearchDescriptionTmpl)),
)

// webSearchDescriptionData feeds the web_search description template with
// the selected backend, so the description matches what the tool actually
// returns (snippets only vs. snippets plus page content).
type webSearchDescriptionData struct {
	toolDescriptionData
	// Provider is the human-readable backend name ("DuckDuckGo",
	// "Tavily").
	Provider string
	// IncludesContent reports whether results carry page content, making
	// a follow-up web_fetch per result unnecessary.
	IncludesContent bool
}

func renderWebSearchDescription(backend SearchBackend, availability toolAvailability) string {
	data := webSearchDescriptionData{
		toolDescriptionData: toolDescriptionData{GhAvailable: availability.ghAvailable},
	}
	switch backend.(type) {
	case *duckDuckGoBackend:
		data.Provider = "DuckDuckGo"
	case *tavilyBackend:
		data.Provider = "Tavily"
		data.IncludesContent = true
	default:
		data.Provider = "the configured search provider"
	}
	return renderTemplate(webSearchDescriptionTpl, data)
}

// defaultSearchHTTPClient builds the HTTP client used when no client is
// supplied to NewWebSearchTool/NewSearchBackend.
func defaultSearchHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 100
	transport.MaxIdleConnsPerHost = 10
	transport.IdleConnTimeout = 90 * time.Second

	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
	}
}

// NewWebSearchTool creates a web search tool. When permissions is nil, the
// permission check is skipped entirely — used by the agentic_fetch
// sub-agent, whose own top-level call is already permission-gated. backend
// selects the search implementation (DuckDuckGo, Tavily, ...); a nil
// backend defaults to DuckDuckGo, built from client (or a fresh client if
// client is also nil).
func NewWebSearchTool(permissions permission.Requester, workingDir string, client *http.Client, backend SearchBackend, options ...toolAvailabilityOption) fantasy.AgentTool {
	availability := applyToolAvailability(options)
	if backend == nil {
		if client == nil {
			client = defaultSearchHTTPClient()
		}
		backend = &duckDuckGoBackend{client: client}
	}

	return withToolParameterSchema(fantasy.NewParallelAgentTool(
		WebSearchToolName,
		renderWebSearchDescription(backend, availability),
		func(ctx context.Context, params WebSearchParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Query == "" {
				return invalidParam("query"), nil
			}

			maxResults := params.MaxResults
			if maxResults < 0 || maxResults > 20 {
				return fantasy.NewTextErrorResponse("max_results must be between 0 and 20"), nil
			}
			if maxResults == 0 {
				maxResults = 10
			}

			if permissions != nil {
				sessionID := GetSessionFromContext(ctx)
				if sessionID == "" {
					return fantasy.ToolResponse{}, missingSessionID("web_search")
				}

				permResp, denied, err := requirePermission(ctx, permissions, permission.CreatePermissionRequest{
					SessionID:   sessionID,
					Path:        workingDir,
					ToolCallID:  call.ID,
					ToolName:    WebSearchToolName,
					Action:      "search",
					Description: fmt.Sprintf("Search the web for: %s", params.Query),
					Params:      WebSearchPermissionsParams(params),
				})
				if err != nil {
					return fantasy.ToolResponse{}, err
				}
				if denied {
					return permResp, nil
				}
			}

			results, err := backend.Search(ctx, params.Query, maxResults)
			slog.Debug("Web search completed", "query", params.Query, "results", len(results), "err", err)
			if err != nil {
				return fantasy.NewTextErrorResponse("Failed to search: " + err.Error()), nil
			}

			return fantasy.NewTextResponse(formatSearchResults(results)), nil
		},
	), map[string]toolParameterSchema{"query": {minLength: intPtr(1)}, "max_results": intSchemaBounds(20)})
}

func intPtr(value int) *int { return &value }
