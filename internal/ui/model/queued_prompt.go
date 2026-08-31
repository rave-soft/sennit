package model

import (
	"fmt"
	"strings"

	"github.com/rave-soft/sennit/internal/ui/chat"
	"github.com/rave-soft/sennit/internal/ui/chatlist"
	"github.com/rave-soft/sennit/internal/ui/styles"
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

// show puts a placeholder for text at the bottom of chat. Called when a
// prompt is submitted into a busy session.
func (s *queuedPromptState) show(ch *chatlist.Chat, styles *styles.Styles, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	s.seq++
	item := queuedPrompt{id: fmt.Sprintf("queued-prompt-%d", s.seq), text: text}
	s.items = append(s.items, item)
	ch.AppendMessages(chat.NewQueuedUserMessageItem(styles, item.id, item.text))
}

// deliver drops the placeholder for text, if there is one, because the real
// message has just arrived and the agent took the prompt. Matching is by text:
// a queued prompt has no id to correlate on until it becomes a message.
func (s *queuedPromptState) deliver(ch *chatlist.Chat, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	for i, item := range s.items {
		if strings.TrimSpace(item.text) != text {
			continue
		}
		ch.RemoveMessage(item.id)
		s.items = append(s.items[:i], s.items[i+1:]...)
		return
	}
}

// clear drops every placeholder when the agent queue goes away.
func (s *queuedPromptState) clear(ch *chatlist.Chat) {
	for _, item := range s.items {
		ch.RemoveMessage(item.id)
	}
	s.items = nil
}

// refloat moves placeholders back to the end of chat after another item is
// appended. A queued prompt has not been said yet, so it must remain below the
// agent's continuing work.
func (s *queuedPromptState) refloat(ch *chatlist.Chat, styles *styles.Styles) {
	for _, item := range s.items {
		ch.RemoveMessage(item.id)
		ch.AppendMessages(chat.NewQueuedUserMessageItem(styles, item.id, item.text))
	}
}
