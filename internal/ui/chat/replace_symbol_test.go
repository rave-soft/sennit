package chat

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/message"
	tools "github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// TestReplaceSymbolRenderTool_MetadataUnmarshalErrorFallsBackToLineCount
// pins the precedence of the metadata guard: `err == nil && (old != "" ||
// new != "")`. Without the parens around the OR, `meta.NewContent != ""`
// escapes the err == nil check, so a metadata blob that fails to
// unmarshal but partially populates the struct (Go's json.Unmarshal fills
// fields before erroring on trailing garbage) would still be treated as
// valid and rendered as a diff summary, even though it's broken data.
func TestReplaceSymbolRenderTool_MetadataUnmarshalErrorFallsBackToLineCount(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	toolCall := message.ToolCall{
		ID:       "tc-rs",
		Name:     tools.ReplaceSymbolToolName,
		Input:    `{"symbol":"Foo","file_path":"foo.go"}`,
		Finished: true,
	}
	// A type-mismatched trailing field: json.Unmarshal decodes fields in
	// JSON key order and only fails on "action" (a string field fed a
	// number), so old_content/new_content are already populated by the
	// time the error is recorded — meta.NewContent ends up non-empty
	// even though err != nil.
	metadata := `{"old_content":"old body","new_content":"replacement body","action":123}`
	result := &message.ToolResult{Content: "one line only", Metadata: metadata}

	ctx := &ReplaceSymbolToolRenderContext{}
	out := ctx.RenderTool(&sty, 80, &ToolRenderOpts{
		ToolCall: toolCall,
		Result:   result,
		Status:   ToolStatusSuccess,
	})

	require.Contains(t, out, "1 line",
		"an unmarshal error must fall back to the raw-content line count summary")
	require.NotContains(t, out, "+1 −1", "must not render a diff summary derived from broken metadata")
}

// TestReplaceSymbolRenderTool_TitleFollowsAction pins Audit 12 finding 3:
// the tool's Action param (replace/add_before/add_after/delete) used to be
// ignored by the renderer, which always titled the header "Replace
// Symbol" — a deletion showed up in the transcript looking exactly like
// an edit.
func TestReplaceSymbolRenderTool_TitleFollowsAction(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	ctx := &ReplaceSymbolToolRenderContext{}

	for action, wantTitle := range map[string]string{
		"":           "Replace Symbol", // default per the param's own description
		"replace":    "Replace Symbol",
		"add_before": "Insert Before Symbol",
		"add_after":  "Insert After Symbol",
		"delete":     "Delete Symbol",
	} {
		toolCall := message.ToolCall{
			ID:       "tc-rs-" + action,
			Name:     tools.ReplaceSymbolToolName,
			Input:    `{"symbol":"Foo","file_path":"foo.go","action":"` + action + `"}`,
			Finished: true,
		}
		out := ansi.Strip(ctx.RenderTool(&sty, 80, &ToolRenderOpts{
			ToolCall: toolCall,
			Status:   ToolStatusRunning,
		}))
		require.Contains(t, out, wantTitle, "action=%q", action)
	}
}
