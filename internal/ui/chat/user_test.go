package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/ui/attachments"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// newTestAttachmentsRenderer builds a bare attachments.Renderer sufficient
// for UserMessageItem construction in tests that don't exercise
// attachments themselves.
func newTestAttachmentsRenderer(sty *styles.Styles) *attachments.Renderer {
	return attachments.NewRenderer(
		sty.Attachments.Normal,
		sty.Attachments.Deleting,
		sty.Attachments.Image,
		sty.Attachments.Text,
		sty.Attachments.Skill,
		sty.Attachments.Remove,
	)
}

// TestUserMessageItemRender_OriginAgentIsMarked is the rendering half of
// the origin-metadata feature: a message.User message tagged
// Origin: message.OriginAgent (a delegated goal or a thread_send/task_send
// follow-up) must render with a visible marker distinguishing it from
// something the person actually typed, while a person-origin message
// (explicit or zero-value) must render byte-identical to before this
// feature existed.
func TestUserMessageItemRender_OriginAgentIsMarked(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	r := newTestAttachmentsRenderer(&sty)
	const width = 60

	agentMsg := &message.Message{
		ID:     "u-agent",
		Role:   message.User,
		Origin: message.OriginAgent,
		Parts:  []message.ContentPart{message.TextContent{Text: "Delegated goal text."}},
	}
	personMsg := &message.Message{
		ID:    "u-person",
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "Delegated goal text."}},
	}

	agentItem := NewUserMessageItem(&sty, agentMsg, r).(*UserMessageItem)
	personItem := NewUserMessageItem(&sty, personMsg, r).(*UserMessageItem)

	agentOut := agentItem.RawRender(width)
	personOut := personItem.RawRender(width)

	require.True(t, strings.Contains(agentOut, originAgentTagLabel),
		"an agent-origin message must render the origin marker")
	require.False(t, strings.Contains(personOut, originAgentTagLabel),
		"a person-origin message must not render the origin marker")

	// A person-origin message (the default/common case) must render
	// exactly as it did before Origin existed: no extra lines, no marker.
	require.Equal(t, 2, personItem.headerLineCount())
	require.Equal(t, 3, agentItem.headerLineCount())
}

// delegationReportMessage builds the message a delegation's completion
// writes into the session that started it.
func delegationReportMessage() *message.Message {
	text := message.DelegationReportPrefix + `
A background task has finished.
id: 36ded998
name: task-36ded998
goal: TASK: keep LSP restarts isolated from stale runtime state
status: completed
child_session: 8fc605e4$$call_yfdq

Result:
ZZQUUX rewrote the restart path and re-ran the suite.`
	return &message.Message{
		ID: "m1", Role: message.User, Origin: message.OriginAgent,
		Parts: []message.ContentPart{message.TextContent{Text: text}},
	}
}

// TestDelegationReportCollapsesToARow is the point of the block: a
// delegation's answer is as long as the work it describes, and it is
// written for the model on its next turn, not for someone scanning their
// own conversation. Collapsed, the row still has to say which delegation
// reported and how it went — a report the reader cannot identify is
// worse than one they have to open.
func TestDelegationReportCollapsesToARow(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	item, _ := NewUserMessageItem(&sty, delegationReportMessage(), nil).(*UserMessageItem)

	out := item.Render(80)
	require.NotContains(t, out, "ZZQUUX", "a collapsed report must not render its body")
	require.NotContains(t, out, "child_session", "nor the machine header the model reads")
	require.Contains(t, out, "keep LSP restarts isolated from stale runtime state",
		"the row says what the delegation was asked to do, with the prompt scaffolding stripped")
	require.Contains(t, out, "completed")
	require.Contains(t, out, collapsedGlyph)
	require.Contains(t, out, expandHint)
}

// TestDelegationReportExpandsAndCloses covers the way in and back out.
func TestDelegationReportExpandsAndCloses(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	item, _ := NewUserMessageItem(&sty, delegationReportMessage(), nil).(*UserMessageItem)

	require.True(t, item.ToggleExpanded())
	out := item.Render(80)
	require.Contains(t, out, "ZZQUUX")
	require.Contains(t, out, "child_session", "nothing is hidden once it is open: a report is evidence")

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	require.Contains(t, lines[len(lines)-1], collapseHint,
		"the way out is the last line, like every other expandable block")

	// Both controls are real; the text between them is not, so a click
	// while reading cannot close what is being read.
	footer := len(lines) - 1
	require.True(t, item.HandleMouseClick(ansi.MouseLeft, MessageLeftPaddingTotal, footer))
	require.True(t, item.HoverableAt(MessageLeftPaddingTotal, item.headerLineCount(), 80))
	require.False(t, item.HandleMouseClick(ansi.MouseLeft, MessageLeftPaddingTotal, footer-1))

	require.False(t, item.ToggleExpanded())
	require.NotContains(t, item.Render(80), "ZZQUUX")
}

// TestOrdinaryUserMessageIsNotCollapsible keeps the block tied to what
// it is for: what the person typed is shown whole, and so is a
// delegation's goal, which is persisted with the same role and origin.
func TestOrdinaryUserMessageIsNotCollapsible(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	typed := &message.Message{
		ID: "m1", Role: message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "ZZQUUX do the thing"}},
	}
	item, _ := NewUserMessageItem(&sty, typed, nil).(*UserMessageItem)
	require.False(t, item.ToggleExpanded())
	require.Contains(t, item.Render(80), "ZZQUUX")

	goal := &message.Message{
		ID: "m2", Role: message.User, Origin: message.OriginAgent,
		Parts: []message.ContentPart{message.TextContent{Text: "ZZQUUX review the diff"}},
	}
	dispatched, _ := NewUserMessageItem(&sty, goal, nil).(*UserMessageItem)
	require.False(t, dispatched.ToggleExpanded())
	require.Contains(t, dispatched.Render(80), "ZZQUUX")
}
