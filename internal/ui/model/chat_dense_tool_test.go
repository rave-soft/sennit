package model

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/braid/internal/message"
	tools "github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/ui/chat"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// TestChatDenseToolGroup_NoGapBetweenConsecutiveOneLinerTools is the
// regression test for the density fix: a run of finished, collapsed
// tool calls that each render as a single line (Read/Grep/List/...)
// must sit flush against each other in the chat list — no blank row
// between them — while the boundary into assistant text right after
// keeps the list's normal gap. See list.List.gapAt and the
// list.AlwaysSpaced interface it consults.
func TestChatDenseToolGroup_NoGapBetweenConsecutiveOneLinerTools(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.chat.SetSize(80, 40)
	sty := u.com.Styles

	view := chat.NewViewToolMessageItem(sty,
		message.ToolCall{ID: "tc-view", Name: tools.ViewToolName, Input: `{"file_path":"internal/foo.go"}`, Finished: true},
		&message.ToolResult{ToolCallID: "tc-view", Content: strings.Repeat("x\n", 341) + "x"},
		false)
	view.SetMessageID("m-view")

	grep := chat.NewGrepToolMessageItem(sty,
		message.ToolCall{ID: "tc-grep", Name: tools.GrepToolName, Input: `{"pattern":"Provider"}`, Finished: true},
		&message.ToolResult{ToolCallID: "tc-grep", Content: "match", Metadata: `{"number_of_matches":27}`},
		false)
	grep.SetMessageID("m-grep")

	glob := chat.NewGlobToolMessageItem(sty,
		message.ToolCall{ID: "tc-glob", Name: tools.GlobToolName, Input: `{"pattern":"*.go"}`, Finished: true},
		&message.ToolResult{ToolCallID: "tc-glob", Content: "f.go", Metadata: `{"number_of_files":4}`},
		false)
	glob.SetMessageID("m-glob")

	reply := chat.NewAssistantMessageItem(sty, &message.Message{
		ID:   "m-assistant",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "Done."},
		},
	})

	u.chat.SetMessages(view, grep, glob, reply)

	out := u.chat.list.Render()
	lines := strings.Split(out, "\n")

	require.GreaterOrEqual(t, len(lines), 5,
		"expected 3 dense tool lines + 1 gap line + at least 1 reply line: %q", out)

	for i := range 3 {
		require.NotEmptyf(t, strings.TrimSpace(lines[i]), "line %d of the dense tool group must not be blank: %q", i, out)
	}

	require.Empty(t, strings.TrimSpace(lines[3]),
		"the boundary between the tool group and the assistant reply must keep the list's blank-line gap")
	require.NotEmpty(t, strings.TrimSpace(lines[4]), "the assistant reply line itself must not be blank")
}

// TestChatRendersDelegationAsCompactStubWhilePending covers how a running
// delegation appears in the transcript: its own chat render is the pending stub
// plus a single current-activity status line (elapsed, step, latest tool)
// — no task tag, todos, or nested-tool tree. Since the session panel no
// longer carries a delegations section, this line is the whole live view
// of a running delegation, and it stays deliberately one line: the
// transcript is a record, not a dashboard.
func TestChatRendersDelegationAsCompactStubWhilePending(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	item := chat.NewAgentToolMessageItem(&sty,
		message.ToolCall{ID: "tc-1", Name: "agent", Input: `{"prompt":"fix the auth bug"}`, Finished: false}, nil, false, nil)
	item.AddNestedTool(chat.NewToolMessageItem(&sty, "m1",
		message.ToolCall{ID: "c1", Name: "bash", Input: `{"command":"go test"}`, Finished: true}, nil, false, nil))

	out := ansi.Strip(item.Render(120))
	require.NotContains(t, out, "fix the auth bug", "the task/prompt belongs to the panel block, not the transcript")
	require.Contains(t, out, "go test", "the current tool must show in the status line under the stub")
	require.Len(t, strings.Split(strings.TrimRight(out, " \n"), "\n"), 2,
		"pending transcript render is exactly stub + one status line")
}
