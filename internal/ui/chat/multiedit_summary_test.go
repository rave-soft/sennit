package chat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/message"
	tools "github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// TestMultiEditRenderTool_AllEditsFailedNoDoubleBullet pins Audit 12
// finding 4: when every edit in a multiedit call fails, diffSummary(0, 0)
// is "", and the old code unconditionally formatted "%s · %d/%d edits
// applied" against that empty string, leaving a summary starting with
// " · " that then collided with appendResultSummary's own "· " separator
// — the header rendered two bullets ("·  · 0/2 edits applied") instead of
// one.
func TestMultiEditRenderTool_AllEditsFailedNoDoubleBullet(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	toolCall := message.ToolCall{
		ID:   "tc-me",
		Name: tools.MultiEditToolName,
		Input: `{"file_path":"foo.go","edits":[
			{"old_string":"a","new_string":"b"},
			{"old_string":"c","new_string":"d"}
		]}`,
		Finished: true,
	}
	meta, err := json.Marshal(tools.MultiEditResponseMetadata{
		Additions:    0,
		Removals:     0,
		EditsApplied: 0,
		EditsFailed: []tools.FailedEdit{
			{Index: 0, Error: "old_string not found"},
			{Index: 1, Error: "old_string not found"},
		},
	})
	require.NoError(t, err)
	result := &message.ToolResult{Content: "", Metadata: string(meta)}

	ctx := &MultiEditToolRenderContext{}
	out := ansi.Strip(ctx.RenderTool(&sty, 120, &ToolRenderOpts{
		ToolCall: toolCall,
		Result:   result,
		Status:   ToolStatusSuccess,
	}))

	require.Contains(t, out, "0/2 edits applied")
	require.Equal(t, 1, strings.Count(out, "·"),
		"must render exactly one bullet when the diff summary is empty, not two")
}
