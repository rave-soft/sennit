package agent

import (
	"context"
	_ "embed"
	"errors"

	"github.com/rave-soft/sennit/internal/agent/tools"
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
