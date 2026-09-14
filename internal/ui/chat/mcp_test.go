package chat

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// TestMCPRenderTool_UnparseableNameStillShowsHeader pins half of Audit 12
// finding 5: a tool name this renderer's naive split can't parse at all
// (no config in hand to resolve it properly, and the name doesn't even
// have the minimal "mcp_<x>_<y>" shape) used to render as a bare
// toolErrorContent block — no header, no indication of which tool call
// this even was. It must instead fall back to showing the raw name, like
// any other unrecognized tool in the transcript.
func TestMCPRenderTool_UnparseableNameStillShowsHeader(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	toolCall := message.ToolCall{
		ID:       "tc-mcp-bad",
		Name:     "mcp_onlyoneseg",
		Input:    `{}`,
		Finished: true,
	}
	ctx := &MCPToolRenderContext{}
	out := ansi.Strip(ctx.RenderTool(&sty, 80, &ToolRenderOpts{
		ToolCall: toolCall,
		Status:   ToolStatusSuccess,
	}))

	require.NotContains(t, out, "Invalid tool name")
	require.Contains(t, out, "Onlyoneseg")
}

// TestMCPRenderTool_SplitsWellFormedName is a control: a well-formed
// "mcp_<server>_<tool>" name (no underscore in the server) still renders
// the "Server → Tool" header this renderer has always produced.
func TestMCPRenderTool_SplitsWellFormedName(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	toolCall := message.ToolCall{
		ID:       "tc-mcp-ok",
		Name:     "mcp_context7_query_docs",
		Input:    `{}`,
		Finished: true,
	}
	ctx := &MCPToolRenderContext{}
	out := ansi.Strip(ctx.RenderTool(&sty, 80, &ToolRenderOpts{
		ToolCall: toolCall,
		Status:   ToolStatusSuccess,
	}))

	require.Contains(t, out, "Context7")
}

// TestMCPRenderTool_SplitsAgainstConfiguredServers closes the other half
// of Audit 12 finding 5. The renderer used to pass nil server names and
// split at the first underscore, so "mcp_claude_in_chrome_navigate"
// rendered as "Claude → In Chrome Navigate" while the permission dialog,
// which does read config, rendered the same call as
// "Claude In Chrome → Navigate". Two places disagreed about one call.
func TestMCPRenderTool_SplitsAgainstConfiguredServers(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	toolCall := message.ToolCall{
		ID:       "tc-mcp-underscore",
		Name:     "mcp_claude_in_chrome_navigate",
		Input:    `{}`,
		Finished: true,
	}
	render := func(cfg CustomAgentConfig) string {
		ctx := &MCPToolRenderContext{cfg: cfg}
		return ansi.Strip(ctx.RenderTool(&sty, 120, &ToolRenderOpts{
			ToolCall: toolCall,
			Status:   ToolStatusSuccess,
		}))
	}

	withServer := render(stubAgentConfig{mcpServers: []string{"claude_in_chrome"}})
	require.Contains(t, withServer, "Claude In Chrome")
	require.Contains(t, withServer, "Navigate")

	// Without the server list the naive split still applies: an old
	// session naming a server since renamed must render as something,
	// not error.
	naive := render(nil)
	require.Contains(t, naive, "Claude")
	require.NotContains(t, naive, "Claude In Chrome")
}
