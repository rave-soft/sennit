package chat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rave-soft/braid/internal/agent/tools"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

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

// TestBashRenderTool_FinishedIsOneLine covers the normal collapsed case: a
// completed bash call with a result renders as exactly one line.
func TestBashRenderTool_FinishedIsOneLine(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	meta, err := json.Marshal(tools.BashResponseMetadata{StartTime: 0, EndTime: 2100, Output: "added 200 packages"})
	require.NoError(t, err)
	result := &message.ToolResult{ToolCallID: "tc-bash", Content: "added 200 packages", Metadata: string(meta)}

	item := NewBashToolMessageItem(&sty, bashToolCall(t), result, false)
	out := item.Render(80)

	require.Equal(t, 1, strings.Count(out, "\n")+1, "expected a single rendered line, got: %q", out)
	require.NotContains(t, out, "...")
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
// to report) must render with no outcome suffix at all — never a
// placeholder value like "None" standing in for missing data.
func TestBashRenderTool_NoMetadataNoJunkSuffix(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	result := &message.ToolResult{ToolCallID: "tc-bash", Content: "some output"}

	item := NewBashToolMessageItem(&sty, bashToolCall(t), result, false)
	out := item.Render(80)

	require.Equal(t, 1, strings.Count(out, "\n")+1, "expected a single rendered line, got: %q", out)
	require.NotContains(t, out, "·", "no timing data means no outcome suffix at all")
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
