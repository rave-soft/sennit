package tools

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"strings"

	"charm.land/fantasy"
)

const ThreadListToolName = "thread_list"

//go:embed thread_list.md.tpl
var threadListDescriptionTmpl []byte

var threadListDescriptionTpl = template.Must(
	template.New("threadListDescription").Parse(string(threadListDescriptionTmpl)),
)

type ThreadListParams struct{}

type ThreadListResponseMetadata struct {
	Threads []ThreadInfo `json:"threads"`
}

// NewThreadListTool creates the thread_list tool. See [NewThreadCreateTool]
// for the manager nil-safety note.
func NewThreadListTool(manager ThreadManager) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		ThreadListToolName,
		renderToolDescription(threadListDescriptionTpl),
		func(ctx context.Context, params ThreadListParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			threads, err := manager.List(ctx)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			if len(threads) == 0 {
				return fantasy.WithResponseMetadata(
					fantasy.NewTextResponse("No threads."),
					ThreadListResponseMetadata{},
				), nil
			}

			var sb strings.Builder
			for _, st := range threads {
				summary := st.ResultSummary
				if summary == "" {
					summary = st.Error
				}
				fmt.Fprintf(&sb, "%s\t%s\t%s\t%s\t%s\n", st.ID, st.Name, st.Status, st.Branch, firstLine(summary))
			}

			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(sb.String()),
				ThreadListResponseMetadata{Threads: threads},
			), nil
		},
	)
}

// firstLine returns s up to its first newline, for one-line summaries.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
