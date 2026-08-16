package chat

import (
	"encoding/xml"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/ui/attachments"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/list"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

// skillInvocation represents the XML structure for a loaded skill.
type skillInvocation struct {
	Name         string `xml:"name"`
	Description  string `xml:"description"`
	Location     string `xml:"location"`
	Instructions string `xml:"instructions"`
}

// UserMessageItem represents a user message in the chat UI.
type UserMessageItem struct {
	*list.Versioned
	*highlightableMessageItem
	*cachedMessageItem
	*focusableMessageItem

	attachments *attachments.Renderer
	message     *message.Message
	sty         *styles.Styles
	// queued marks a prompt the person has submitted but the agent has
	// not taken yet: it is busy, so this is waiting in its queue. The
	// text is theirs and reads exactly like a sent message, so the tag
	// is the only thing separating "said" from "about to say" — see
	// NewQueuedUserMessageItem.
	queued bool
}

// NewUserMessageItem creates a new UserMessageItem.
func NewUserMessageItem(sty *styles.Styles, message *message.Message, attachments *attachments.Renderer) MessageItem {
	v := list.NewVersioned()
	return &UserMessageItem{
		Versioned:                v,
		highlightableMessageItem: defaultHighlighter(sty, v),
		cachedMessageItem:        &cachedMessageItem{},
		focusableMessageItem:     newFocusableMessageItem(v),
		attachments:              attachments,
		message:                  message,
		sty:                      sty,
	}
}

// NewQueuedUserMessageItem creates a placeholder for a prompt that has
// been submitted while the agent was busy and is sitting in its queue
// until the current turn reaches its next step.
//
// It exists because that wait is otherwise invisible: nothing is
// persisted until the agent actually takes the prompt, so the chat used
// to show no trace of a message that had been typed, sent, and accepted.
// Waits of several minutes are ordinary when the turn is deep in a long
// tool call, and a silent one reads as a lost message.
//
// id is the caller's own handle for removing this item once the real
// message lands (see UI.deliverQueuedPrompt); it is not a message id and
// never reaches the database.
func NewQueuedUserMessageItem(sty *styles.Styles, id, text string) MessageItem {
	item, _ := NewUserMessageItem(sty, &message.Message{
		ID:    id,
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: text}},
	}, nil).(*UserMessageItem)
	item.queued = true
	return item
}

// Finished implements list.Item. User messages are immutable once
// submitted, so the entry is always safe to freeze.
func (m *UserMessageItem) Finished() bool {
	return true
}

// AlwaysSpaced implements list.AlwaysSpaced. User text keeps the list's
// normal gap around it even on a short single-line message, so it never
// blends into a dense run of one-line tool calls (see list.List.gapAt).
func (m *UserMessageItem) AlwaysSpaced() bool {
	return true
}

// turnSeparator renders the solid full-width rule that opens every user
// turn — the chat's visual boundary between conversation turns.
func turnSeparator(sty *styles.Styles, width int) string {
	if width <= 0 {
		return ""
	}
	return sty.Section.Line.Render(strings.Repeat(styles.SectionSeparator, width))
}

// originAgentTagLabel is the small caption marking a message an agent
// authored (a delegated goal or a thread_send/task_send follow-up) rather
// than something the person actually typed.
const originAgentTagLabel = "Agent"

// queuedTagLabel marks a submitted prompt the agent has not taken yet.
// It says what is true — the message is waiting, not answered — in the
// same place the agent-authored tag sits.
const queuedTagLabel = "Queued · waiting for the current step to finish"

// withTurnSeparator prepends the turn separator rule (plus a blank line)
// to a user message's rendered body. When the message originated from
// another agent rather than the person, a muted tag line is inserted
// between the separator and the blank line so the turn reads as
// agent-authored at a glance.
func (m *UserMessageItem) withTurnSeparator(content string, width int) string {
	sep := turnSeparator(m.sty, width)
	switch {
	case m.queued:
		sep = sep + "\n" + m.sty.Messages.OriginAgentTag.Render(queuedTagLabel)
	case m.message.Origin == message.OriginAgent:
		sep = sep + "\n" + m.sty.Messages.OriginAgentTag.Render(originAgentTagLabel)
	}
	if content == "" {
		return sep
	}
	return sep + "\n\n" + content
}

// headerLineCount returns the number of leading lines in RawRender's
// output that belong to the turn header (separator rule, optional origin
// tag, blank spacer) rather than the message body. Render uses this to
// decide which lines get plain padding instead of the user border prefix.
func (m *UserMessageItem) headerLineCount() int {
	if m.queued || m.message.Origin == message.OriginAgent {
		return 3
	}
	return 2
}

// RawRender implements [MessageItem].
func (m *UserMessageItem) RawRender(width int) string {
	cappedWidth := cappedMessageWidth(width)

	content, height, ok := m.getCachedRender(cappedWidth)
	// cache hit
	if ok {
		return m.renderHighlighted(content, cappedWidth, height)
	}

	msgContent := strings.TrimSpace(m.message.Content().Text)

	// Check if this is a skill invocation (loaded_skill XML)
	if strings.HasPrefix(msgContent, "<loaded_skill>") {
		content = m.withTurnSeparator(m.renderSkillInvocation(msgContent, cappedWidth), cappedWidth)
		height = lipgloss.Height(content)
		m.setCachedRender(content, cappedWidth, height)
		return m.renderHighlighted(content, cappedWidth, height)
	}

	renderer := common.MarkdownRenderer(m.sty, cappedWidth)
	mu := common.LockMarkdownRenderer(renderer)

	mu.Lock()
	result, err := renderer.Render(msgContent)
	mu.Unlock()

	if err != nil {
		content = msgContent
	} else {
		content = strings.TrimSuffix(result, "\n")
	}

	if len(m.message.BinaryContent()) > 0 {
		attachmentsStr := m.renderAttachments(cappedWidth)
		if content == "" {
			content = attachmentsStr
		} else {
			content = strings.Join([]string{content, "", attachmentsStr}, "\n")
		}
	}

	content = m.withTurnSeparator(content, cappedWidth)
	height = lipgloss.Height(content)
	m.setCachedRender(content, cappedWidth, height)
	return m.renderHighlighted(content, cappedWidth, height)
}

// renderSkillInvocation renders a loaded_skill XML as a special UI element.
func (m *UserMessageItem) renderSkillInvocation(content string, width int) string {
	var skill skillInvocation
	if err := xml.Unmarshal([]byte(content), &skill); err != nil {
		// If parsing fails, just render as markdown
		renderer := common.MarkdownRenderer(m.sty, width)
		mu := common.LockMarkdownRenderer(renderer)

		mu.Lock()
		result, err := renderer.Render(content)
		mu.Unlock()

		if err != nil {
			return content
		}
		return strings.TrimSuffix(result, "\n")
	}

	return toolOutputSkillContent(m.sty, skill.Name, skill.Description)
}

// Render implements MessageItem.
func (m *UserMessageItem) Render(width int) string {
	// Bypass the prefix cache while a highlight range is active so
	// selection drags reflect immediately without invalidating the
	// cache. Highlight changes are intentionally applied "above" the
	// prefix cache.
	useCache := !m.isHighlighted()
	var key uint64
	if m.focused {
		key = 1
	}
	if useCache {
		if cached, ok := m.getCachedPrefixedRender(width, key); ok {
			return cached
		}
	}
	var prefix string
	if m.focused {
		prefix = m.sty.Messages.UserFocused.Render()
	} else {
		prefix = m.sty.Messages.UserBlurred.Render()
	}
	// RawRender opens every user turn with a separator rule and a blank
	// line (see withTurnSeparator); those two lines get plain indentation
	// instead of the user border prefix, so the bar hugs the message text
	// only.
	pad := strings.Repeat(" ", lipgloss.Width(prefix))
	lines := strings.Split(m.RawRender(width), "\n")
	header := m.headerLineCount()
	for i, line := range lines {
		if i < header {
			lines[i] = pad + line
			continue
		}
		lines[i] = prefix + line
	}
	out := strings.Join(lines, "\n")
	if useCache {
		m.setCachedPrefixedRender(out, width, key)
	}
	return out
}

// ID implements MessageItem.
func (m *UserMessageItem) ID() string {
	return m.message.ID
}

// renderAttachments renders attachments.
func (m *UserMessageItem) renderAttachments(width int) string {
	var attachments []message.Attachment
	for _, at := range m.message.BinaryContent() {
		attachments = append(attachments, message.Attachment{
			FileName: at.Path,
			MimeType: at.MIMEType,
		})
	}
	// This message is already posted, so the attachment can't be removed;
	// don't render the remove button.
	return m.attachments.Render(attachments, false, false, width)
}

// HandleKeyEvent implements KeyEventHandler.
func (m *UserMessageItem) HandleKeyEvent(key tea.KeyMsg) (bool, tea.Cmd) {
	if k := key.String(); k == "c" || k == "y" {
		text := m.message.Content().Text
		return true, common.CopyToClipboard(text, "Message copied to clipboard")
	}
	return false, nil
}
