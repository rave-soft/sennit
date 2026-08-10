package tools

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"

	"charm.land/fantasy"
)

const ThreadSendToolName = "thread_send"

//go:embed thread_send.md.tpl
var threadSendDescriptionTmpl []byte

var threadSendDescriptionTpl = template.Must(
	template.New("threadSendDescription").Parse(string(threadSendDescriptionTmpl)),
)

type ThreadSendParams struct {
	ID      string `json:"id" description:"The thread's ID or name"`
	Message string `json:"message" description:"The instruction to send to the thread's agent"`
}

// NewThreadSendTool creates the thread_send tool. See [NewThreadCreateTool]
// for the manager nil-safety note.
func NewThreadSendTool(manager ThreadManager) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		ThreadSendToolName,
		renderToolDescription(threadSendDescriptionTpl),
		func(ctx context.Context, params ThreadSendParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.ID == "" {
				return fantasy.NewTextErrorResponse("id is required"), nil
			}
			if params.Message == "" {
				return fantasy.NewTextErrorResponse("message is required"), nil
			}

			if err := manager.Send(ctx, params.ID, params.Message); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			return fantasy.NewTextResponse(fmt.Sprintf("Sent message to thread %q", params.ID)), nil
		},
	)
}
