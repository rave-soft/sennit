package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func noticeTestMessage(text string) *message.Message {
	return &message.Message{
		ID:    "m-notice",
		Role:  message.System,
		Parts: []message.ContentPart{message.TextContent{Text: text}},
	}
}

// TestExtractMessageItems_SystemRoleBecomesANotice: system-role messages
// used to fall through and render as nothing at all, which is why the
// merge-and-removal note needed a home. They now show up as one muted
// line.
func TestExtractMessageItems_SystemRoleBecomesANotice(t *testing.T) {
	t.Parallel()

	sty := styles.BraidDark()
	styPtr := &sty
	items := ExtractMessageItems(styPtr, noticeTestMessage(`Thread "tidy-up" merged into main and removed.`), nil, nil)
	require.Len(t, items, 1)
	require.IsType(t, &NoticeItem{}, items[0])

	out := ansi.Strip(items[0].Render(80))
	require.Contains(t, out, "tidy-up")
	require.Contains(t, out, "merged into main and removed")
}

// TestExtractMessageItems_EmptySystemMessageRendersNothing: an empty
// notice would paint a blank line in the middle of the conversation for
// no reason.
func TestExtractMessageItems_EmptySystemMessageRendersNothing(t *testing.T) {
	t.Parallel()

	sty := styles.BraidDark()
	styPtr := &sty
	require.Empty(t, ExtractMessageItems(styPtr, noticeTestMessage("   "), nil, nil))
}

// TestNoticeItem_StaysOnOneLine: a notice is an aside. Left to wrap, a
// long one would take several lines in the middle of the transcript and
// start competing with what was actually said.
func TestNoticeItem_StaysOnOneLine(t *testing.T) {
	t.Parallel()

	sty := styles.BraidDark()
	styPtr := &sty
	long := "Thread " + strings.Repeat("very-long-name-", 20) + " merged into main and removed."
	item := NewNoticeItem(styPtr, noticeTestMessage(long))

	out := ansi.Strip(item.Render(60))
	require.Equal(t, 1, len(strings.Split(out, "\n")))
	require.LessOrEqual(t, ansi.StringWidth(out), 60)
	require.Contains(t, out, "…", "an over-long notice must be truncated, not wrapped")
}
