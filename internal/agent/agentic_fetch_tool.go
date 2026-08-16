package agent

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"charm.land/fantasy"

	"github.com/rave-soft/sennit/internal/agent/prompt"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/permission"
)

//go:embed templates/agentic_fetch.md
var agenticFetchToolDescription string

// agenticFetchValidationResult holds the validated parameters from the tool call context.
type agenticFetchValidationResult struct {
	SessionID      string
	AgentMessageID string
}

// validateAgenticFetchParams validates the tool call parameters and extracts required context values.
func validateAgenticFetchParams(ctx context.Context, params tools.AgenticFetchParams) (agenticFetchValidationResult, error) {
	if params.Prompt == "" {
		return agenticFetchValidationResult{}, errors.New("prompt is required")
	}

	sessionID := tools.GetSessionFromContext(ctx)
	if sessionID == "" {
		return agenticFetchValidationResult{}, errors.New("session id missing from context")
	}

	agentMessageID := tools.GetMessageFromContext(ctx)
	if agentMessageID == "" {
		return agenticFetchValidationResult{}, errors.New("agent message id missing from context")
	}

	return agenticFetchValidationResult{
		SessionID:      sessionID,
		AgentMessageID: agentMessageID,
	}, nil
}

//go:embed templates/agentic_fetch_prompt.md.tpl
var agenticFetchPromptTmpl []byte

//nolint:unparam // matches the (tool, error) signature of the other buildTools helpers
func (c *coordinator) agenticFetchTool(_ context.Context, client *http.Client) (fantasy.AgentTool, error) {
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConns = 100
		transport.MaxIdleConnsPerHost = 10
		transport.IdleConnTimeout = 90 * time.Second

		client = &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		}
	}

	return fantasy.NewParallelAgentTool(
		tools.AgenticFetchToolName,
		agenticFetchToolDescription,
		func(ctx context.Context, params tools.AgenticFetchParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			validationResult, err := validateAgenticFetchParams(ctx, params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			// Determine description based on mode.
			var description string
			if params.URL != "" {
				description = fmt.Sprintf("Fetch and analyze content from URL: %s", params.URL)
			} else {
				description = "Search the web and analyze results"
			}

			p, err := c.permissions.Request(
				ctx,
				permission.CreatePermissionRequest{
					SessionID:   validationResult.SessionID,
					Path:        c.cfg.WorkingDir(),
					ToolCallID:  call.ID,
					ToolName:    tools.AgenticFetchToolName,
					Action:      "fetch",
					Description: description,
					Params:      tools.AgenticFetchPermissionsParams(params),
				},
			)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !p {
				return tools.NewPermissionDeniedResponse(), nil
			}

			tmpDir, err := os.MkdirTemp(c.cfg.Config().Options.DataDirectory, brand.Slug+"-fetch-*")
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to create temporary directory: %s", err)), nil
			}
			defer func() { _ = os.RemoveAll(tmpDir) }() // best-effort temp dir cleanup

			var fullPrompt string

			if params.URL != "" {
				// URL mode: fetch the URL content first.
				content, filePath, err := tools.FetchLargeContent(ctx, client, tmpDir, params.URL)
				if err != nil {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to fetch URL: %s", err)), nil
				}

				if filePath != "" {
					fullPrompt = fmt.Sprintf("%s\n\nThe web page from %s has been saved to: %s\n\nUse the read and grep tools to analyze this file and extract the requested information.", params.Prompt, params.URL, filePath)
				} else {
					fullPrompt = fmt.Sprintf("%s\n\nWeb page URL: %s\n\n<webpage_content>\n%s\n</webpage_content>", params.Prompt, params.URL, content)
				}
			} else {
				// Search mode: let the sub-agent search and fetch as needed.
				fullPrompt = fmt.Sprintf("%s\n\nUse the web_search tool to find relevant information. Break down the question into smaller, focused searches if needed. After searching, use web_fetch to get detailed content from the most relevant results.", params.Prompt)
			}

			promptOpts := []prompt.Option{
				prompt.WithWorkingDir(tmpDir),
			}

			promptTemplate, err := prompt.NewPrompt("agentic_fetch", string(agenticFetchPromptTmpl), promptOpts...)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("error creating prompt: %s", err)
			}

			model, err := c.buildAgentModel(ctx, true)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("error building models: %s", err)
			}

			systemPrompt, err := promptTemplate.Build(ctx, model.Model.Provider(), model.Model.Model(), c.cfg)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("error building system prompt: %s", err)
			}

			providerCfg, ok := c.cfg.Config().Providers.Get(model.ModelCfg.Provider)
			if !ok {
				return fantasy.ToolResponse{}, errors.New("model provider not configured")
			}

			searchBackend, err := c.webSearchBackend()
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("web_search: %w", err)
			}

			// nil permissions: this sub-agent's parent agentic_fetch call is
			// already permission-gated above, so its own fetch/search tools
			// run unauthenticated.
			webFetchTool := tools.NewWebFetchTool(nil, tmpDir, client)
			webSearchTool := tools.NewWebSearchTool(nil, tmpDir, client, searchBackend)
			fetchTools := []fantasy.AgentTool{
				webFetchTool,
				webSearchTool,
				tools.NewGlobTool(tmpDir, c.cfg.Config().Tools.Glob),
				tools.NewSearchTool(tmpDir, c.cfg.Config().Tools.Grep),
				tools.NewReadTool(c.lspManager, c.permissions, c.filetracker, nil, tmpDir),
			}

			// Sub-agent tools run without hook interception. The top-level
			// `agentic_fetch` call itself is already wrapped from the coder's
			// side; firing hooks again for every inner tool call would run
			// the user's hooks N times per delegated turn.

			agent := NewSessionAgent(SessionAgentOptions{
				Model:                model,
				SystemPromptPrefix:   providerCfg.SystemPromptPrefix,
				SystemPrompt:         systemPrompt,
				DisableAutoSummarize: c.cfg.Config().Options.DisableAutoSummarize,
				Sessions:             c.sessions,
				Messages:             c.messages,
				Tools:                fetchTools,
			})

			// The child session is NOT auto-approved: the fetch/search/glob/
			// grep tools above don't touch permissions at all, but
			// NewReadTool does when asked to read a path outside tmpDir.
			// The top-level `agentic_fetch` call already required
			// permission (above); auto-approving the child session on top
			// of that used to let the sub-agent read arbitrary files
			// anywhere on disk without ever prompting the user. Leaving
			// SessionSetup unset routes those read requests through the
			// normal per-session permission flow, same as any other
			// agent-as-tool sub-agent (see coordinator.buildTools).
			return c.runSubAgent(ctx, subAgentParams{
				Agent:          agent,
				SessionID:      validationResult.SessionID,
				AgentMessageID: validationResult.AgentMessageID,
				ToolCallID:     call.ID,
				Prompt:         fullPrompt,
				SessionTitle:   "Fetch Analysis",
			})
		},
	), nil
}
