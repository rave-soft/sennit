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
