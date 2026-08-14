package chat

import (
	"strings"
	"testing"

	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/ui/attachments"
	"github.com/rave-soft/braid/internal/ui/styles"
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

	sty := styles.CharmtonePantera()
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
