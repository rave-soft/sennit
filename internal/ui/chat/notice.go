package chat

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/ui/list"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

// NoticeItem renders a system-role message: a one-line, muted record of
// something Sennit did on its own — a thread merging and being cleared
// away, say. It is not part of the conversation. The model never sees a
// system-role message (they are dropped when the prompt is built), and it
// is not styled like anything either party said; it reads as a margin
// note, because that is what it is.
type NoticeItem struct {
	*list.Versioned
	*cachedMessageItem

	id   string
	text string
	sty  *styles.Styles
}

// NewNoticeItem creates a NoticeItem for a system-role message.
func NewNoticeItem(sty *styles.Styles, msg *message.Message) MessageItem {
	return &NoticeItem{
		Versioned:         list.NewVersioned(),
		cachedMessageItem: &cachedMessageItem{},
		id:                msg.ID,
		text:              strings.TrimSpace(msg.Content().Text),
		sty:               sty,
	}
}

// ID implements MessageItem.
func (n *NoticeItem) ID() string { return n.id }

// Finished implements list.Item. A notice records something that already
// happened and never changes afterwards, so it is safe to freeze.
func (n *NoticeItem) Finished() bool { return true }

// RawRender implements MessageItem.
func (n *NoticeItem) RawRender(width int) string {
	innerWidth := max(0, width-MessageLeftPaddingTotal)
	content, _, ok := n.getCachedRender(innerWidth)
	if !ok {
		content = n.renderContent(innerWidth)
		n.setCachedRender(content, innerWidth, lipgloss.Height(content))
	}
	return content
}

// Render implements MessageItem.
func (n *NoticeItem) Render(width int) string {
	if cached, ok := n.getCachedPrefixedRender(width, 0); ok {
		return cached
	}
	prefix := n.sty.Messages.SectionHeader.Render()
	lines := strings.Split(n.RawRender(width), "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	out := strings.Join(lines, "\n")
	n.setCachedPrefixedRender(out, width, 0)
	return out
}

func (n *NoticeItem) renderContent(width int) string {
	if n.text == "" || width <= 0 {
		return ""
	}
	// One line, always: a notice that wraps into a paragraph starts
	// competing with the conversation for attention.
	return n.sty.Messages.Notice.Render(ansi.Truncate(noticeBullet+n.text, width, "…"))
}

// noticeBullet marks the line as a record rather than speech.
const noticeBullet = "· "
