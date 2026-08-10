package tools

import (
	"context"
	_ "embed"
	"html/template"
	"time"

	"charm.land/fantasy"
)

const ThreadWaitToolName = "thread_wait"

//go:embed thread_wait.md.tpl
var threadWaitDescriptionTmpl []byte

var threadWaitDescriptionTpl = template.Must(
	template.New("threadWaitDescription").Parse(string(threadWaitDescriptionTmpl)),
)

type ThreadWaitParams struct {
	IDs            []string `json:"ids,omitempty" description:"Thread IDs or names to wait for (default: all)"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty" description:"Give up after this many seconds (default: no timeout)"`
}

// NewThreadWaitTool creates the thread_wait tool. It blocks the tool call
// until every named thread (or, with no ids, every thread) leaves the
// pending/running/merging states, honoring both the params timeout and the
// tool call's own context cancellation. See [NewThreadCreateTool] for the
// manager nil-safety note.
func NewThreadWaitTool(manager ThreadManager) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		ThreadWaitToolName,
		renderToolDescription(threadWaitDescriptionTpl),
		func(ctx context.Context, params ThreadWaitParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			var timeout time.Duration
			if params.TimeoutSeconds > 0 {
				timeout = time.Duration(params.TimeoutSeconds) * time.Second
			}

			if err := manager.Wait(ctx, params.IDs, timeout); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			return fantasy.NewTextResponse("All named threads have settled."), nil
		},
	)
}
