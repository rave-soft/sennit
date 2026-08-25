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

// validateAgenticFetchParams validates the tool call parameters and
// extracts required context values.
//
// The two return values are deliberately different kinds of failure. A bad
// parameter is the model's to fix and comes back as invalid, so the tool
// can report it as a tool error. Missing context ids are a wiring fault
// this call cannot recover from by rewording, so they come back as a Go
// error — the same split every other delegation tool makes (see
// agent_tool.go and custom_agent_tool.go), and what AGENTS.md asks for.
func validateAgenticFetchParams(ctx context.Context, params tools.AgenticFetchParams) (result agenticFetchValidationResult, invalid error, err error) {
	if params.Prompt == "" {
		return agenticFetchValidationResult{}, errors.New("prompt is required"), nil
	}

	sessionID := tools.GetSessionFromContext(ctx)
	if sessionID == "" {
		return agenticFetchValidationResult{}, nil, errors.New("session id missing from context")
	}

	agentMessageID := tools.GetMessageFromContext(ctx)
	if agentMessageID == "" {
		return agenticFetchValidationResult{}, nil, errors.New("agent message id missing from context")
	}

	return agenticFetchValidationResult{
		SessionID:      sessionID,
		AgentMessageID: agentMessageID,
	}, nil, nil
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
		client = &http.Client{Timeout: 30 * time.Second, Transport: transport}
	}
	return tools.WithToolSchemaConstraints(fantasy.NewParallelAgentTool(
		tools.AgenticFetchToolName,
		agenticFetchToolDescription,
		func(ctx context.Context, params tools.AgenticFetchParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			validation, invalid, err := validateAgenticFetchParams(ctx, params)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if invalid != nil {
				return fantasy.NewTextErrorResponse(invalid.Error()), nil
			}
			childDepth := delegationDepth(ctx)
			return c.launchDelegation(ctx, tools.TaskCreateArgs{
				Goal:            params.Prompt,
				ParentSessionID: validation.SessionID,
				SessionTitle:    "Fetch Analysis",
				SessionID:       c.sessions.CreateAgentToolSessionID(validation.AgentMessageID, call.ID),
				Factory: func(ctx context.Context, childID string) (func(context.Context) (tools.TaskRunResult, error), func(), error) {
					description := "Search the web and analyze results"
					if params.URL != "" {
						description = fmt.Sprintf("Fetch and analyze content from URL: %s", params.URL)
					}
					allowed, err := c.permissions.Request(ctx, permission.CreatePermissionRequest{
						SessionID: validation.SessionID, Path: c.cfg.WorkingDir(), ToolCallID: call.ID,
						ToolName: tools.AgenticFetchToolName, Action: "fetch", Description: description,
						Params: tools.AgenticFetchPermissionsParams(params),
					})
					if err != nil {
						return nil, nil, err
					}
					if !allowed {
						return nil, nil, errors.New("permission denied for agentic fetch")
					}
					tmpDir, err := os.MkdirTemp(c.cfg.Config().Options.DataDirectory, brand.Slug+"-fetch-*")
					if err != nil {
						return nil, nil, fmt.Errorf("create temporary directory: %w", err)
					}
					cleanup := func() { _ = os.RemoveAll(tmpDir) }
					fullPrompt := params.Prompt + "\n\nUse web_search and web_fetch to research this request."
					if params.URL != "" {
						content, filePath, err := tools.FetchLargeContent(ctx, client, tmpDir, params.URL)
						if err != nil {
							return nil, cleanup, fmt.Errorf("fetch URL: %w", err)
						}
						if filePath != "" {
							fullPrompt = fmt.Sprintf("%s\n\nThe web page from %s is saved at %s. Analyze it with read and grep.", params.Prompt, params.URL, filePath)
						} else {
							fullPrompt = fmt.Sprintf("%s\n\nWeb page URL: %s\n\n<webpage_content>\n%s\n</webpage_content>", params.Prompt, params.URL, content)
						}
					}
					promptTemplate, err := prompt.NewPrompt("agentic_fetch", string(agenticFetchPromptTmpl), prompt.WithWorkingDir(tmpDir))
					if err != nil {
						return nil, cleanup, err
					}
					model, err := c.buildAgentModel(ctx, true)
					if err != nil {
						return nil, cleanup, err
					}
					systemPrompt, err := promptTemplate.Build(ctx, model.Model.Provider(), model.Model.Model(), c.cfg)
					if err != nil {
						return nil, cleanup, err
					}
					providerCfg, ok := c.cfg.Config().Providers.Get(model.ModelCfg.Provider)
					if !ok {
						return nil, cleanup, errors.New("model provider not configured")
					}
					searchBackend, err := c.webSearchBackend()
					if err != nil {
						return nil, cleanup, fmt.Errorf("web_search: %w", err)
					}
					availability := tools.ResolveSystemToolAvailability()
					agent := NewSessionAgent(SessionAgentOptions{
						Model: model, SystemPromptPrefix: providerCfg.SystemPromptPrefix, SystemPrompt: systemPrompt,
						DisableAutoSummarize: c.cfg.Config().Options.DisableAutoSummarize,
						Sessions:             c.sessions, Messages: c.messages,
						Tools: []fantasy.AgentTool{
							tools.NewWebFetchTool(nil, tmpDir, client, availability), tools.NewWebSearchTool(nil, tmpDir, client, searchBackend, availability),
							tools.NewGlobTool(tmpDir, c.cfg.Config().Tools.Glob), tools.NewSearchTool(tmpDir, c.cfg.Config().Tools.Grep),
							tools.NewReadTool(c.lspManager, c.permissions, c.filetracker, nil, tmpDir),
						},
					})
					return c.subAgentTaskRun(validation.SessionID, childID, fullPrompt, agent, childDepth), cleanup, nil
				},
			})
		},
	), map[string]tools.ToolSchemaConstraint{"prompt": {MinLength: intPointer(1)}}), nil
}
