package tools

import (
	"context"
	_ "embed"
	"html/template"

	"charm.land/fantasy"
)

const AskParentToolName = "ask_parent"

//go:embed ask_parent.md.tpl
var askParentDescriptionTmpl []byte

var askParentDescriptionTpl = template.Must(
	template.New("askParentDescription").Parse(string(askParentDescriptionTmpl)),
)

type AskParentParams struct {
	Message string `json:"message" description:"The question or update to send to the parent session"`
}

// ParentMessenger is the subset of agent.Coordinator's API the ask_parent
// tool needs — declared here rather than importing internal/agent, which
// would cycle (internal/agent imports this package for its tool
// constructors).
type ParentMessenger interface {
	SendToParent(ctx context.Context, sessionID, message string) error
}

// NewAskParentTool creates the ask_parent tool, letting a delegation's own
// agent send a non-blocking mid-run message to whichever session created
// it (see ParentMessenger.SendToParent). The target is implicit: the
// caller's own session id, resolved from context, not a parameter.
func NewAskParentTool(messenger ParentMessenger) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		AskParentToolName,
		renderToolDescription(askParentDescriptionTpl),
		func(ctx context.Context, params AskParentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				// The model has no way to supply a session ID — it is wired
				// in by the caller — so this is the same infrastructure
				// failure every other tool reports as a Go error, not a
				// text response.
				return fantasy.ToolResponse{}, missingSessionID("asking the parent")
			}
			if params.Message == "" {
				return invalidParam("message"), nil
			}

			if err := messenger.SendToParent(ctx, sessionID, params.Message); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			return fantasy.NewTextResponse("Message sent to parent session."), nil
		},
	)
}
