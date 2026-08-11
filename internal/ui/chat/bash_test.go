package chat

import (
	"encoding/json"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/rave-soft/braid/internal/agent/tools"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func TestExpandableBodyHoverHighlightsWholeBlock(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	normal := expandableBodyContent(&sty, "l1\nl2\nl3\nl4\nl5", 40, false, false)
	hovered := expandableBodyContent(&sty, "l1\nl2\nl3\nl4\nl5", 40, false, true)

	require.Equal(t, 5, strings.Count(hovered, "\n")+1)
	require.NotEqual(t, normal, hovered)
	hoverSequence := lipgloss.NewStyle().Background(sty.Tool.ContentLineHover.GetBackground()).Render("x")
	hoverPrefix, _, ok := strings.Cut(hoverSequence, "x")
	require.True(t, ok)
	for _, line := range strings.Split(hovered, "\n") {
		require.Contains(t, line, hoverPrefix)
	}
}

func bashToolCall(t *testing.T) message.ToolCall {
	t.Helper()
	input, err := json.Marshal(tools.BashParams{Command: "npm install"})
	require.NoError(t, err)
	return message.ToolCall{ID: "tc-bash", Name: tools.BashToolName, Input: string(input), Finished: true}
}

// TestBashRenderTool_RunningNoResultIsOneLine covers the window between the
// model finishing streaming a bash command's input (toolCall.Finished flips
// true) and the command's execution actually returning a result — long for
// anything that takes real time to run, like "npm install". Before this
// fix, that window rendered a dangling "Waiting for tool response..." line
// under the header, breaking the always-one-line collapsed guarantee.
func TestBashRenderTool_RunningNoResultIsOneLine(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	item := NewBashToolMessageItem(&sty, bashToolCall(t), nil, false)
	out := item.Render(80)

	require.Equal(t, 1, strings.Count(out, "\n")+1, "expected a single rendered line, got: %q", out)
	require.NotContains(t, out, "Waiting for tool response")
}

// TestBashRenderTool_FinishedShowsOutputPreview covers the normal collapsed
// case: a completed bash call renders its output directly under the header
// (no blank separator line), and short output needs no expand hint.
func TestBashRenderTool_FinishedShowsOutputPreview(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	meta, err := json.Marshal(tools.BashResponseMetadata{StartTime: 0, EndTime: 2100, Output: "added 200 packages"})
	require.NoError(t, err)
	result := &message.ToolResult{ToolCallID: "tc-bash", Content: "added 200 packages", Metadata: string(meta)}

	item := NewBashToolMessageItem(&sty, bashToolCall(t), result, false)
	out := item.Render(80)

	lines := strings.Split(out, "\n")
	require.Len(t, lines, 2, "expected header + one output line, got: %q", out)
	require.Contains(t, lines[1], "added 200 packages")
	require.NotContains(t, out, "Click to expand", "short output has nothing to expand")
}

// TestBashRenderTool_CollapsedCapsAtFourLinesAndToggles covers the
// click-to-expand contract: collapsed output is capped at
// collapsedBodyLines lines followed by a "Click to expand" hint;
// ToggleExpanded reveals the full output with a "Click to collapse" hint,
// and toggling again collapses back.
func TestBashRenderTool_CollapsedCapsAtFourLinesAndToggles(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	output := "l1\nl2\nl3\nl4\nl5\nl6\nl7"
	meta, err := json.Marshal(tools.BashResponseMetadata{StartTime: 0, EndTime: 2100, Output: output})
	require.NoError(t, err)
	result := &message.ToolResult{ToolCallID: "tc-bash", Content: output, Metadata: string(meta)}

	item := NewBashToolMessageItem(&sty, bashToolCall(t), result, false)

	collapsed := item.Render(80)
	lines := strings.Split(collapsed, "\n")
	require.Len(t, lines, 1+collapsedBodyLines+1, "expected header + capped body + hint, got: %q", collapsed)
	require.Contains(t, collapsed, "Click to expand (3 more lines)")
	require.NotContains(t, collapsed, "l5")

	expandable, ok := item.(Expandable)
	require.True(t, ok, "bash items must implement Expandable")
	require.True(t, expandable.ToggleExpanded())

	expanded := item.Render(80)
	require.Contains(t, expanded, "l7", "expanded render must show the full output")
	require.Contains(t, expanded, "Click to collapse")
	require.NotContains(t, expanded, "Click to expand")

	require.False(t, expandable.ToggleExpanded())
	recollapsed := item.Render(80)
	require.Contains(t, recollapsed, "Click to expand (3 more lines)")
	require.NotContains(t, recollapsed, "l5")
}

// TestBashRenderTool_ErrorAddsSingleTailLine covers the one exception to
// the one-line rule: an error result appends a single error-tag line, not
// a truncation marker.
func TestBashRenderTool_ErrorAddsSingleTailLine(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	result := &message.ToolResult{ToolCallID: "tc-bash", Content: "npm: command not found", IsError: true}

	item := NewBashToolMessageItem(&sty, bashToolCall(t), result, false)
	out := item.Render(80)

	// joinToolParts separates header and body with a blank line, so this
	// is header + blank + error, not header + error directly.
	lines := strings.Split(out, "\n")
	require.Len(t, lines, 3, "expected header, blank, error line, got: %q", out)
	require.Contains(t, lines[2], "npm: command not found")
}

// TestBashRenderTool_AwaitingPermissionStillShowsStatus ensures the
// running-state fix is scoped to ToolStatusRunning only — awaiting
// permission is a distinct, meaningful state, and still renders below the
// header.
func TestBashRenderTool_AwaitingPermissionStillShowsStatus(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	item := NewBashToolMessageItem(&sty, bashToolCall(t), nil, false)
	item.SetStatus(ToolStatusAwaitingPermission)

	out := item.Render(80)
	require.Contains(t, out, "Requesting permission")
}

// TestBashRenderTool_MultilineCommandIsOneLine is the regression test for
// a multi-line command (e.g. `python3 -c "<multi-line script>"`) breaking
// the one-line collapsed guarantee: the embedded "\n"s must never survive
// into the rendered header as real line breaks — the truncation ellipsis
// must land inline, not stranded alone on a second line.
func TestBashRenderTool_MultilineCommandIsOneLine(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	script := "import json\nwith open('/tmp/x.json') as f:\n    data = json.load(f)\nprint(data)\n"
	input, err := json.Marshal(tools.BashParams{Command: `python3 -c "` + script + `"`})
	require.NoError(t, err)
	tc := message.ToolCall{ID: "tc-bash", Name: tools.BashToolName, Input: string(input), Finished: true}

	for _, width := range []int{20, 40, 80, 120} {
		item := NewBashToolMessageItem(&sty, tc, nil, false)
		out := item.Render(width)
		require.Equal(t, 1, strings.Count(out, "\n")+1,
			"width %d: multi-line command must still render as one line, got: %q", width, out)
	}
}

// TestBashRenderTool_NoMetadataNoJunkSuffix covers the "None" regression:
// a result with no/unparsable metadata (so bashDurationSummary has nothing
// to report) must render its header with no outcome suffix at all — never a
// placeholder value like "None" standing in for missing data.
func TestBashRenderTool_NoMetadataNoJunkSuffix(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	result := &message.ToolResult{ToolCallID: "tc-bash", Content: "some output"}

	item := NewBashToolMessageItem(&sty, bashToolCall(t), result, false)
	out := item.Render(80)

	lines := strings.Split(out, "\n")
	require.Len(t, lines, 2, "expected header + one output line, got: %q", out)
	require.NotContains(t, lines[0], "·", "no timing data means no outcome suffix at all")
	require.NotContains(t, strings.ToLower(out), "none")
}

// TestBashRenderTool_BackgroundJunkDescriptionFallsBackToCommand covers a
// model filling the optional "description" field with a literal
// placeholder like "None" instead of leaving it empty: the background job
// header must fall back to the command text, never display "None" as if
// it were a real description.
func TestBashRenderTool_BackgroundJunkDescriptionFallsBackToCommand(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	input, err := json.Marshal(tools.BashParams{Command: "long_running_script.sh", RunInBackground: true})
	require.NoError(t, err)
	tc := message.ToolCall{ID: "tc-bash", Name: tools.BashToolName, Input: string(input), Finished: true}

	meta, err := json.Marshal(tools.BashResponseMetadata{Background: true, ShellID: "sh-1", Description: "None"})
	require.NoError(t, err)
	result := &message.ToolResult{ToolCallID: "tc-bash", Content: "started", Metadata: string(meta)}

	item := NewBashToolMessageItem(&sty, tc, result, false)
	out := item.Render(80)

	require.Contains(t, out, "long_running_script.sh", "must fall back to the command text")
	require.NotContains(t, strings.ToLower(out), "none")
}
