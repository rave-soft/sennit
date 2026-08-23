package chat

import (
	"encoding/json"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/rave-soft/sennit/internal/hooks"
	"github.com/rave-soft/sennit/internal/message"
	tools "github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// TestRawRenderWidth_NarrowTerminalNeverGoesNegative covers a terminal
// narrower than MessageLeftPaddingTotal (or, for capped tools, narrower
// than the padding cappedMessageWidth subtracts). Before the max(0, ...)
// guard in RawRender, toolItemWidth went negative and propagated into
// styles.Width() and ansi.Truncate() calls downstream — this only needs
// to not panic.
func TestRawRenderWidth_NarrowTerminalNeverGoesNegative(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	meta, err := json.Marshal(tools.BashResponseMetadata{StartTime: 0, EndTime: 2100, Output: "some output"})
	require.NoError(t, err)
	result := &message.ToolResult{ToolCallID: "tc-bash", Content: "some output", Metadata: string(meta)}

	// hasCappedWidth == true: goes through cappedMessageWidth.
	bashItem := NewBashToolMessageItem(&sty, bashToolCall(t), result, false)
	// hasCappedWidth == false: goes through the plain width-padding branch.
	editItem := NewEditToolMessageItem(&sty, editToolCall(t), nil, false)

	for _, w := range []int{-5, -1, 0, 1, 2} {
		w := w
		require.NotPanics(t, func() { bashItem.Render(w) }, "bash Render(%d) panicked", w)
		require.NotPanics(t, func() { editItem.Render(w) }, "edit Render(%d) panicked", w)
	}
}

// TestRawRenderWidth_BodyAndHookIndicatorShareWidth pins RawRender's
// contract: the width it computes is handed unchanged to both RenderTool
// and the hook indicator, so they never disagree. Before the fix,
// RenderTool implementations re-capped an already-capped width
// (effectively width-4 vs. the hook indicator's width-2), so the tool
// body ended up MessageLeftPaddingTotal columns narrower than the
// indicator sitting right above it.
//
// expandableBodyContent renders each body line through
// sty.Tool.ContentLine.Width(width), which pads/truncates to exactly
// `width` columns — so measuring the rendered body line's width directly
// pins the value RenderTool actually received, independent of what
// RawRender itself computed.
func TestRawRenderWidth_BodyAndHookIndicatorShareWidth(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	// Small enough that the hook name's truncation budget stays under
	// toolOutputHookIndicator's own 30-column cap - otherwise a 2-column
	// difference in the width passed in wouldn't change the hook line at
	// all, and that half of this test would pass for the wrong reason.
	const totalWidth = 24

	// A hook name much longer than any plausible column budget so it gets
	// truncated to fill the budget exactly - this makes the hook line's
	// width sensitive to the exact value passed in.
	hookMeta := struct {
		tools.BashResponseMetadata
		Hook hooks.HookMetadata `json:"hook"`
	}{
		BashResponseMetadata: tools.BashResponseMetadata{Output: "ok"},
		Hook: hooks.HookMetadata{
			HookCount: 1,
			Decision:  "allow",
			Hooks:     []hooks.HookInfo{{Name: strings.Repeat("x", 100), Decision: "allow"}},
		},
	}
	metaJSON, err := json.Marshal(hookMeta)
	require.NoError(t, err)

	result := &message.ToolResult{ToolCallID: "tc-bash", Content: "ok", Metadata: string(metaJSON)}
	item := NewBashToolMessageItem(&sty, bashToolCall(t), result, false)

	out := item.RawRender(totalWidth)
	lines := strings.Split(out, "\n")

	expectedWidth := cappedMessageWidth(totalWidth)

	var hookLine, bodyLine string
	for _, ln := range lines {
		switch {
		case strings.Contains(ln, "Hook"):
			hookLine = ln
		case strings.Contains(ln, "ok"):
			bodyLine = ln
		}
	}
	require.NotEmpty(t, hookLine, "expected a hook indicator line in: %q", out)
	require.NotEmpty(t, bodyLine, "expected a body content line in: %q", out)

	require.Equal(t, expectedWidth, lipgloss.Width(bodyLine),
		"tool body width must equal RawRender's single computed width, not a re-derived (narrower) one")
	require.LessOrEqual(t, lipgloss.Width(hookLine), expectedWidth,
		"hook indicator must fit within the same width as the body")
}

func editToolCall(t *testing.T) message.ToolCall {
	t.Helper()
	input, err := json.Marshal(tools.EditParams{FilePath: "foo.go", OldString: "a", NewString: "b"})
	require.NoError(t, err)
	return message.ToolCall{ID: "tc-edit", Name: tools.EditToolName, Input: string(input), Finished: true}
}
