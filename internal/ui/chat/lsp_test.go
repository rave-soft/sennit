package chat

import (
	"encoding/json"
	"testing"

	"github.com/rave-soft/sennit/internal/message"
	tools "github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// TestDefinitionRenderTool_CompactStopsAtHeader pins two things about the
// simpleToolRenderer table: the row's title ("Find Definition" — swapping
// it for another row's title, or a typo, must fail this) and that Compact
// short-circuits before any summary is computed.
func TestDefinitionRenderTool_CompactStopsAtHeader(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	input, err := json.Marshal(tools.DefinitionParams{Symbol: "myFunc"})
	require.NoError(t, err)
	toolCall := message.ToolCall{ID: "tc-def", Name: tools.DefinitionToolName, Input: string(input), Finished: true}
	result := &message.ToolResult{Content: "func myFunc() {}\nfunc other() {}"}

	out := definitionToolRenderer.RenderTool(&sty, 80, &ToolRenderOpts{
		ToolCall: toolCall,
		Result:   result,
		Status:   ToolStatusSuccess,
		Compact:  true,
	})

	require.Contains(t, out, "Find Definition")
	require.Contains(t, out, "myFunc")
	require.NotContains(t, out, "lines", "compact mode must not reach the summary")
}

// TestDefinitionRenderTool_SummarizesFromMetadata pins the metadata-first
// fallback: with metadata present the summary counts metadata.Content's
// lines, not the raw result content's — dropping the fallback (e.g.
// summarizing opts.Result.Content unconditionally) changes this line count.
func TestDefinitionRenderTool_SummarizesFromMetadata(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	input, err := json.Marshal(tools.DefinitionParams{Symbol: "myFunc"})
	require.NoError(t, err)
	toolCall := message.ToolCall{ID: "tc-def", Name: tools.DefinitionToolName, Input: string(input), Finished: true}
	meta, err := json.Marshal(tools.DefinitionResponseMetadata{Content: "l1\nl2\nl3"})
	require.NoError(t, err)
	result := &message.ToolResult{Content: "the raw content has a single line", Metadata: string(meta)}

	out := definitionToolRenderer.RenderTool(&sty, 80, &ToolRenderOpts{
		ToolCall: toolCall,
		Result:   result,
		Status:   ToolStatusSuccess,
	})

	require.Contains(t, out, "3 lines")
}

// TestLSPRestartRenderTool_NoNameOmitsParam pins that an empty "name" param
// renders no header params at all — dropping the empty-check in
// lspRestartToolRenderer.params would render a stray empty param instead.
func TestLSPRestartRenderTool_NoNameOmitsParam(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	input, err := json.Marshal(tools.LSPRestartParams{})
	require.NoError(t, err)
	toolCall := message.ToolCall{ID: "tc-restart", Name: tools.LSPRestartToolName, Input: string(input), Finished: true}
	result := &message.ToolResult{Content: "restarted"}

	out := lspRestartToolRenderer.RenderTool(&sty, 80, &ToolRenderOpts{
		ToolCall: toolCall,
		Result:   result,
		Status:   ToolStatusSuccess,
	})

	require.Contains(t, out, "Restart LSP")
	require.Contains(t, out, "1 line")
}
