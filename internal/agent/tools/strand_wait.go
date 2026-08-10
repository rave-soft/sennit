package tools

import (
	"context"
	_ "embed"
	"html/template"
	"time"

	"charm.land/fantasy"
)

const StrandWaitToolName = "strand_wait"

//go:embed strand_wait.md.tpl
var strandWaitDescriptionTmpl []byte

var strandWaitDescriptionTpl = template.Must(
	template.New("strandWaitDescription").Parse(string(strandWaitDescriptionTmpl)),
)

type StrandWaitParams struct {
	IDs            []string `json:"ids,omitempty" description:"Strand IDs or names to wait for (default: all)"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty" description:"Give up after this many seconds (default: no timeout)"`
}

// NewStrandWaitTool creates the strand_wait tool. It blocks the tool call
// until every named strand (or, with no ids, every strand) leaves the
// pending/running/merging states, honoring both the params timeout and the
// tool call's own context cancellation. See [NewStrandCreateTool] for the
// manager nil-safety note.
func NewStrandWaitTool(manager StrandManager) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		StrandWaitToolName,
		renderToolDescription(strandWaitDescriptionTpl),
		func(ctx context.Context, params StrandWaitParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			var timeout time.Duration
			if params.TimeoutSeconds > 0 {
				timeout = time.Duration(params.TimeoutSeconds) * time.Second
			}

			if err := manager.Wait(ctx, params.IDs, timeout); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			return fantasy.NewTextResponse("All named strands have settled."), nil
		},
	)
}
