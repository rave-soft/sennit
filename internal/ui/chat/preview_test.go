package chat

import (
	"strings"
	"testing"

	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// TestToolMessageItem_CollapsedByDefaultIsOneLine covers the core UX fix:
// a finished tool call must render as a single line by default — no body,
// no wall of content — across every major tool class (file, search,
// network). Regression target: "I hit init and a wall of text scrolls by". Bash
// and Write are the deliberate exceptions: they show a capped content
// preview under their header (see TestToolMessageItem_ExpandableBodyPreview).
func TestToolMessageItem_CollapsedByDefaultIsOneLine(t *testing.T) {
	t.Parallel()

	sty := styles.BraidDark()

	longFile := strings.Repeat("line of code\n", 300)

	tests := []struct {
		name   string
		tc     message.ToolCall
		result *message.ToolResult
	}{
		{
			name: "view",
			tc:   message.ToolCall{ID: "1", Name: "view", Input: `{"file_path":"internal/foo.go"}`, Finished: true},
			result: &message.ToolResult{
				ToolCallID: "1",
				Metadata:   `{"content":` + toJSONString(longFile) + `}`,
			},
		},
		{
			name: "grep",
			tc:   message.ToolCall{ID: "4", Name: "grep", Input: `{"pattern":"Provider"}`, Finished: true},
			result: &message.ToolResult{
				ToolCallID: "4",
				Content:    longFile,
				Metadata:   `{"number_of_matches":27}`,
			},
		},
		{
			name:   "fetch",
			tc:     message.ToolCall{ID: "5", Name: "fetch", Input: `{"url":"https://example.com"}`, Finished: true},
			result: &message.ToolResult{ToolCallID: "5", Content: longFile},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			item := NewToolMessageItem(&sty, "m1", tt.tc, tt.result, false, nil)
			out := item.Render(120)
			lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
			require.Lenf(t, lines, 1, "collapsed %s render must be one line, got:\n%s", tt.name, out)
		})
	}
}

// TestToolMessageItem_ExpandableBodyPreview covers the exceptions to the
// one-line rule: a finished bash call shows its output — and a finished
// write call the written content — under the header, capped at
// collapsedBodyLines lines plus a "Click to expand" hint when more was
// cut off (full toggle behavior is covered in bash_test.go).
func TestToolMessageItem_ExpandableBodyPreview(t *testing.T) {
	t.Parallel()

	sty := styles.BraidDark()
	longFile := strings.Repeat("line of code\n", 300)

	tests := []struct {
		name   string
		tc     message.ToolCall
		result *message.ToolResult
	}{
		{
			name: "bash",
			tc:   message.ToolCall{ID: "3", Name: "bash", Input: `{"command":"go test ./..."}`, Finished: true},
			result: &message.ToolResult{
				ToolCallID: "3",
				Metadata:   `{"output":` + toJSONString(longFile) + `,"start_time":0,"end_time":2100}`,
			},
		},
		{
			name:   "write",
			tc:     message.ToolCall{ID: "2", Name: "write", Input: `{"file_path":"docs/x.md","content":` + toJSONString(longFile) + `}`, Finished: true},
			result: &message.ToolResult{ToolCallID: "2", Content: "wrote file"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			item := NewToolMessageItem(&sty, "m1", tt.tc, tt.result, false, nil)
			require.Implements(t, (*Expandable)(nil), item)
			out := item.Render(120)
			lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
			require.Lenf(t, lines, 1+collapsedBodyLines+1,
				"collapsed %s render must be header + capped body + hint, got:\n%s", tt.name, out)
			require.Contains(t, out, "Click to expand")
		})
	}
}

// TestToolMessageItem_NoExpand covers plain tool items staying collapsed: a
// non-delegation, non-bash tool item must not implement chat.Expandable at
// all, and rendering at any width stays one line regardless — there is no
// state to toggle into that would ever show file content in chat.
func TestToolMessageItem_NoExpand(t *testing.T) {
	t.Parallel()

	sty := styles.BraidDark()
	longFile := strings.Repeat("line of code\n", 300)

	tc := message.ToolCall{ID: "1", Name: "view", Input: `{"file_path":"internal/foo.go"}`, Finished: true}
	result := &message.ToolResult{ToolCallID: "1", Metadata: `{"content":` + toJSONString(longFile) + `}`}
	item := NewToolMessageItem(&sty, "m1", tc, result, false, nil)

	_, ok := item.(Expandable)
	require.False(t, ok, "a plain tool item must not implement Expandable — there is nothing to expand")

	out := item.Render(120)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	require.Len(t, lines, 1, "a plain tool item always renders as one line")
}

// TestToolMessageItem_ErrorShowsStderrTail covers the error case: a failed
// bash call must still surface a short error/stderr summary inline even
// though the body is collapsed by default.
func TestToolMessageItem_ErrorShowsStderrTail(t *testing.T) {
	t.Parallel()

	sty := styles.BraidDark()
	tc := message.ToolCall{ID: "1", Name: "bash", Input: `{"command":"go build ./..."}`, Finished: true}
	result := &message.ToolResult{
		ToolCallID: "1",
		IsError:    true,
		Content:    "exit status 1\nsome_file.go:10: undefined: foo",
	}
	item := NewToolMessageItem(&sty, "m1", tc, result, false, nil)
	out := item.Render(120)
	require.Contains(t, out, "undefined: foo")
}

// toJSONString quotes s as a JSON string literal for building metadata
// blobs in tests without pulling in encoding/json boilerplate per case.
func toJSONString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
