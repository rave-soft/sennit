package tools

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"strings"

	"charm.land/fantasy"
)

const StrandListToolName = "strand_list"

//go:embed strand_list.md.tpl
var strandListDescriptionTmpl []byte

var strandListDescriptionTpl = template.Must(
	template.New("strandListDescription").Parse(string(strandListDescriptionTmpl)),
)

type StrandListParams struct{}

type StrandListResponseMetadata struct {
	Strands []StrandInfo `json:"strands"`
}

// NewStrandListTool creates the strand_list tool. See [NewStrandCreateTool]
// for the manager nil-safety note.
func NewStrandListTool(manager StrandManager) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		StrandListToolName,
		renderToolDescription(strandListDescriptionTpl),
		func(ctx context.Context, params StrandListParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			strands, err := manager.List(ctx)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			if len(strands) == 0 {
				return fantasy.WithResponseMetadata(
					fantasy.NewTextResponse("No strands."),
					StrandListResponseMetadata{},
				), nil
			}

			var sb strings.Builder
			for _, st := range strands {
				summary := st.ResultSummary
				if summary == "" {
					summary = st.Error
				}
				fmt.Fprintf(&sb, "%s\t%s\t%s\t%s\t%s\n", st.ID, st.Name, st.Status, st.Branch, firstLine(summary))
			}

			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(sb.String()),
				StrandListResponseMetadata{Strands: strands},
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
