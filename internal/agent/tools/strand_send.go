package tools

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"

	"charm.land/fantasy"
)

const StrandSendToolName = "strand_send"

//go:embed strand_send.md.tpl
var strandSendDescriptionTmpl []byte

var strandSendDescriptionTpl = template.Must(
	template.New("strandSendDescription").Parse(string(strandSendDescriptionTmpl)),
)

type StrandSendParams struct {
	ID      string `json:"id" description:"The strand's ID or name"`
	Message string `json:"message" description:"The instruction to send to the strand's agent"`
}

// NewStrandSendTool creates the strand_send tool. See [NewStrandCreateTool]
// for the manager nil-safety note.
func NewStrandSendTool(manager StrandManager) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		StrandSendToolName,
		renderToolDescription(strandSendDescriptionTpl),
		func(ctx context.Context, params StrandSendParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.ID == "" {
				return fantasy.NewTextErrorResponse("id is required"), nil
			}
			if params.Message == "" {
				return fantasy.NewTextErrorResponse("message is required"), nil
			}

			if err := manager.Send(ctx, params.ID, params.Message); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			return fantasy.NewTextResponse(fmt.Sprintf("Sent message to strand %q", params.ID)), nil
		},
	)
}
