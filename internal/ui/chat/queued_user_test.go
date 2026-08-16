package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// TestQueuedUserMessageItemIsMarked: the text is the person's own and
// reads exactly like a sent message, so without the tag there is nothing
// to tell "waiting in the agent's queue" from "already said".
func TestQueuedUserMessageItemIsMarked(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	queued := ansi.Strip(NewQueuedUserMessageItem(&sty, "queued-prompt-1", "also update the docs").Render(60))

	require.Contains(t, queued, "also update the docs")
	require.Contains(t, queued, "Queued")

	sent := ansi.Strip(NewUserMessageItem(&sty, &message.Message{
		ID:    "u1",
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "also update the docs"}},
	}, nil).Render(60))
	require.NotContains(t, sent, "Queued")

	// The tag is a header line above the body, exactly like the
	// agent-authored tag: the text moves down by one line and no more.
	require.Equal(t, textLine(t, sent)+1, textLine(t, queued))
}

// textLine reports which rendered line the message body starts on.
func textLine(t *testing.T, rendered string) int {
	t.Helper()
	for i, line := range strings.Split(rendered, "\n") {
		if strings.Contains(line, "also update the docs") {
			return i
		}
	}
	t.Fatalf("message text missing from render:\n%s", rendered)
	return -1
}
