package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// pendingRender is the visible text of the pending block for a call whose
// arguments have streamed argsLen bytes so far.
func pendingRender(t *testing.T, argsLen int) string {
	t.Helper()

	sty := styles.SennitDark()
	opts := &ToolRenderOpts{
		ToolCall: message.ToolCall{
			ID:    "tc-1",
			Name:  "write",
			Input: strings.Repeat("x", argsLen),
		},
	}
	require.True(t, opts.IsPending(), "the fixture must be a call still in flight")
	return ansi.Strip(pendingTool(&sty, "Write", opts))
}

// TestPendingToolReportsLongStreamingArguments: a call whose arguments are
// the work - a write of a file composed in one go - can spend minutes being
// generated. The pending block used to be the tool's name and an animation
// and nothing else for all of it, which reads as a hang; the size of what
// has arrived is the one thing that separates a model still writing from
// one that stopped.
func TestPendingToolReportsLongStreamingArguments(t *testing.T) {
	t.Parallel()

	require.Contains(t, pendingRender(t, 45*1024), "45.0 KB")
}

// TestPendingToolStaysQuietForOrdinaryArguments: most calls carry a path or
// a command and land in one breath. Reporting those would put a number on
// every tool in the transcript and say nothing.
func TestPendingToolStaysQuietForOrdinaryArguments(t *testing.T) {
	t.Parallel()

	render := pendingRender(t, 64)
	require.NotContains(t, render, "B")
	require.Contains(t, render, "Write", "the name still renders")

	// The threshold itself is the first size worth mentioning.
	sty := styles.SennitDark()
	require.Empty(t, streamingArgsHint(&sty, argsStreamingThreshold-1))
	require.NotEmpty(t, streamingArgsHint(&sty, argsStreamingThreshold))
}
