package model

import (
	"testing"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/stretchr/testify/require"
)

// TestQueuedPromptIsVisibleUntilTheAgentTakesIt: a prompt submitted into a
// busy session is not persisted anywhere until the running turn folds it
// in, which can be minutes later. Until then the placeholder is the only
// evidence the message was sent at all.
func TestQueuedPromptIsVisibleUntilTheAgentTakesIt(t *testing.T) {
	u := newCursorTestUI(t)

	u.showQueuedPrompt("also update the docs")
	require.Equal(t, 1, u.chat.Len())
	require.NotNil(t, u.chat.MessageItem("queued-prompt-1"))

	// The agent took it: the real message arrives and the placeholder has
	// nothing left to stand in for.
	u.appendSessionMessage(message.Message{
		ID:    "real-message",
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "also update the docs"}},
	})
	require.Nil(t, u.chat.MessageItem("queued-prompt-1"))
	require.NotNil(t, u.chat.MessageItem("real-message"))
	require.Empty(t, u.queued.items)
}

// TestQueuedPromptStaysBelowTheAgentsWork: the turn keeps producing
// replies and tool calls while the prompt waits. A placeholder left where
// it was first appended would sit above them, reading as a message that
// had already been answered.
func TestQueuedPromptStaysBelowTheAgentsWork(t *testing.T) {
	u := newCursorTestUI(t)

	u.showQueuedPrompt("also update the docs")
	u.appendSessionMessage(message.Message{
		ID:    "assistant-1",
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "working on it"}},
	})

	items := u.chat.MessageItems()
	last := items[len(items)-1]
	require.Equal(t, "queued-prompt-1", last.ID())
}

// TestIdenticalQueuedPromptsAreSeparateEntries: sending the same text
// twice queues two prompts, and taking one must not clear the other.
func TestIdenticalQueuedPromptsAreSeparateEntries(t *testing.T) {
	u := newCursorTestUI(t)

	u.showQueuedPrompt("go on")
	u.showQueuedPrompt("go on")
	require.Equal(t, 2, u.chat.Len())

	u.deliverQueuedPrompt("go on")
	require.Len(t, u.queued.items, 1)
	require.Nil(t, u.chat.MessageItem("queued-prompt-1"))
	require.NotNil(t, u.chat.MessageItem("queued-prompt-2"))
}

// TestClearQueuedPromptsDropsThemAll covers escape emptying the agent's
// queue: the placeholders describe prompts that will now never run.
func TestClearQueuedPromptsDropsThemAll(t *testing.T) {
	u := newCursorTestUI(t)

	u.showQueuedPrompt("one")
	u.showQueuedPrompt("two")
	u.clearQueuedPrompts()

	require.Zero(t, u.chat.Len())
	require.Empty(t, u.queued.items)
}
