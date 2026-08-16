package model

import (
	"fmt"
	"strings"

	"github.com/rave-soft/sennit/internal/ui/chat"
)

// queuedPromptState tracks the prompts this client has submitted while the
// agent was busy and which the agent has therefore queued rather than run.
//
// Nothing about a queued prompt is persisted until the running turn folds
// it into its next step (see agent/turn.go's drainQueueForStep), which can
// be minutes away when that turn is inside a long tool call. These
// placeholders are the chat's account of that gap: they are local to this
// client, they carry no message id, and each one is replaced by the real
// message the moment the agent takes it.
type queuedPromptState struct {
	// seq numbers placeholder ids so two identical prompts are still two
	// distinct list entries.
	seq int
	// items is in submission order, which is also the order the agent
	// will take them in.
	items []queuedPrompt
}

type queuedPrompt struct {
	id   string
	text string
}

// showQueuedPrompt puts a placeholder for text at the bottom of the chat.
// Called when a prompt is submitted into a busy session.
func (m *UI) showQueuedPrompt(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	m.queued.seq++
	item := queuedPrompt{id: fmt.Sprintf("queued-prompt-%d", m.queued.seq), text: text}
	m.queued.items = append(m.queued.items, item)
	m.chat.AppendMessages(chat.NewQueuedUserMessageItem(m.com.Styles, item.id, item.text))
}

// deliverQueuedPrompt drops the placeholder for text, if there is one,
// because the real message has just arrived — the agent took the prompt
// and persisted it. Matching is by text: a queued prompt has no id to
// correlate on until it becomes a message, and the person's own words are
// what the placeholder was showing.
func (m *UI) deliverQueuedPrompt(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	for i, item := range m.queued.items {
		if strings.TrimSpace(item.text) != text {
			continue
		}
		m.chat.RemoveMessage(item.id)
		m.queued.items = append(m.queued.items[:i], m.queued.items[i+1:]...)
		return
	}
}

// clearQueuedPrompts drops every placeholder. Used where the queue itself
// goes away — escape clearing it, or a session switch replacing the chat.
func (m *UI) clearQueuedPrompts() {
	for _, item := range m.queued.items {
		m.chat.RemoveMessage(item.id)
	}
	m.queued.items = nil
}

// refloatQueuedPrompts moves the placeholders back to the end of the chat
// after something else was appended.
//
// A queued prompt has not been said yet, so it must not sit above the
// agent's continuing work as though it had been: the turn keeps producing
// tool calls and replies while the prompt waits, and every one of them
// would otherwise be appended after it. Keeping the placeholders pinned at
// the bottom is what makes "still waiting" readable.
func (m *UI) refloatQueuedPrompts() {
	if len(m.queued.items) == 0 {
		return
	}
	for _, item := range m.queued.items {
		m.chat.RemoveMessage(item.id)
		m.chat.AppendMessages(chat.NewQueuedUserMessageItem(m.com.Styles, item.id, item.text))
	}
}
