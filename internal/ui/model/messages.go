package model

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/chat"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/workspace"
)

func sessionMessageItems(sty *styles.Styles, cfg *config.Config, msgs []message.Message) ([]chat.MessageItem, int64) {
	msgPtrs := make([]*message.Message, len(msgs))
	for i := range msgs {
		msgPtrs[i] = &msgs[i]
	}
	toolResultMap := chat.BuildToolResultMap(msgPtrs)
	var lastUserMessageTime int64
	if len(msgPtrs) > 0 {
		lastUserMessageTime = msgPtrs[0].CreatedAt
	}
	items := make([]chat.MessageItem, 0, len(msgs)*2)
	for _, msg := range msgPtrs {
		if msg.Role == message.User {
			lastUserMessageTime = msg.CreatedAt
		}
		items = append(items, chat.ExtractMessageItems(sty, msg, toolResultMap, cfg)...)
		if msg.Role == message.Assistant && msg.FinishPart() != nil && msg.FinishPart().Reason == message.FinishReasonEndTurn {
			items = append(items, chat.NewAssistantInfoItem(sty, msg, cfg, time.Unix(lastUserMessageTime, 0)))
		}
	}
	return items, lastUserMessageTime
}

func (m *UI) applySessionMessageItems(items []chat.MessageItem, lastUserMessageTime int64) tea.Cmd {
	var cmds []tea.Cmd
	m.sess.lastUserMessageTime = lastUserMessageTime
	// If the user switches between sessions while the agent is working we
	// want to make sure the animations are shown. Gate on the agent actually
	// being busy: a session that was killed mid-generation can persist an
	// assistant message with no Finish part, which still reports isSpinning()
	// even though nothing is running. Starting animations for it here would
	// leave a ghost "working" spinner (and a second one alongside any tool
	// spinner) after the session is reloaded.
	//
	// The question has to be about *this* session, not the workspace. Any
	// running thread or background task makes the workspace busy, and a
	// workspace-wide gate therefore let every reloaded session start its
	// ghost spinners — which, with delegations in flight most of the time,
	// is nearly always.
	if m.isCurrentSessionBusy() {
		for _, item := range items {
			if animatable, ok := item.(chat.Animatable); ok {
				if cmd := animatable.StartAnimation(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
		}
	}

	if cmd := m.chat.SetMessages(items...); cmd != nil {
		cmds = append(cmds, cmd)
	}
	// New items just replaced the whole list — sync whether their todos
	// rows are hidden to whether the panel is currently showing this
	// session's todos, same as the pubsub session-update handler does for
	// already-loaded items.
	if m.sess.hasSession() {
		m.chat.SetTodosHidden(hasIncompleteTodos(m.sess.current.Todos))
		m.refreshDelegationBlocks()
	}
	if cmd := m.chat.RestartPausedVisibleAnimations(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	m.chat.SelectLast()
	return tea.Sequence(cmds...)
}

type childLoad struct {
	sessionID string
	container chat.NestedToolContainer
	tools     []chat.ToolMessageItem
}

func loadNestedToolCalls(ctx context.Context, ws workspace.SessionStore, sty *styles.Styles, cfg *config.Config, rootSessionID string, generation uint64, items []chat.MessageItem) error {
	var children []childLoad
	for _, item := range items {
		nestedContainer, ok := item.(chat.NestedToolContainer)
		if !ok {
			continue
		}
		toolItem, ok := item.(chat.ToolMessageItem)
		if !ok {
			continue
		}
		tc := toolItem.ToolCall()
		children = append(children, childLoad{
			sessionID: session.CreateAgentToolSessionID(toolItem.MessageID(), tc.ID),
			container: nestedContainer,
		})
	}

	if len(children) == 0 {
		return nil
	}

	sessionIDs := make([]string, len(children))
	for i, c := range children {
		sessionIDs[i] = c.sessionID
	}

	nestedMsgsMap, err := ws.ListMessagesBySessionIDs(ctx, rootSessionID, generation, sessionIDs)
	if err != nil {
		return err
	}

	var deeperItems []chat.MessageItem
	for i := range children {
		c := &children[i]
		nestedMsgs, ok := nestedMsgsMap[c.sessionID]
		if !ok || len(nestedMsgs) == 0 {
			continue
		}

		nestedMsgPtrs := make([]*message.Message, len(nestedMsgs))
		for i := range nestedMsgs {
			nestedMsgPtrs[i] = &nestedMsgs[i]
		}
		nestedToolResultMap := chat.BuildToolResultMap(nestedMsgPtrs)

		for _, nestedMsg := range nestedMsgPtrs {
			nestedItems := chat.ExtractMessageItems(sty, nestedMsg, nestedToolResultMap, cfg)
			for _, nestedItem := range nestedItems {
				if nestedToolItem, ok := nestedItem.(chat.ToolMessageItem); ok {
					if simplifiable, ok := nestedToolItem.(chat.Compactable); ok {
						simplifiable.SetCompact(true)
					}
					c.tools = append(c.tools, nestedToolItem)

					if _, ok := nestedItem.(chat.NestedToolContainer); ok {
						deeperItems = append(deeperItems, nestedItem)
					}
				}
			}
		}
	}

	if err := loadNestedToolCalls(ctx, ws, sty, cfg, rootSessionID, generation, deeperItems); err != nil {
		return err
	}

	for i := range children {
		children[i].container.SetNestedTools(children[i].tools)
	}
	return nil
}

// appendSessionMessage appends a new message to the current session in the chat
// if the message is a tool result it will update the corresponding tool call message
func (m *UI) appendSessionMessage(msg message.Message) tea.Cmd {
	var cmds []tea.Cmd

	existing := m.chat.MessageItem(msg.ID)
	if existing != nil {
		return nil
	}

	switch msg.Role {
	case message.User:
		// Shell commands are rendered live via shellResultMsg; skip
		// the persisted duplicate.
		hasShellCmd := false
		for _, part := range msg.Parts {
			if _, ok := part.(message.ShellCommand); ok {
				hasShellCmd = true
				break
			}
		}
		if hasShellCmd {
			return nil
		}
		m.sess.lastUserMessageTime = msg.CreatedAt
		// The agent has taken this prompt: drop the placeholder that was
		// standing in for it while it waited in the queue.
		m.queued.deliver(m.chat, msg.Content().Text)
		items := chat.ExtractMessageItems(m.com.Styles, &msg, nil, m.com.Config())
		for _, item := range items {
			if animatable, ok := item.(chat.Animatable); ok {
				if cmd := animatable.StartAnimation(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
		}
		m.chat.AppendMessages(items...)
		if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case message.Assistant:
		items := chat.ExtractMessageItems(m.com.Styles, &msg, nil, m.com.Config())
		for _, item := range items {
			if animatable, ok := item.(chat.Animatable); ok {
				if cmd := animatable.StartAnimation(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
		}
		m.chat.AppendMessages(items...)
		if m.chat.Follow() {
			if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		if msg.FinishPart() != nil && msg.FinishPart().Reason == message.FinishReasonEndTurn {
			infoItem := chat.NewAssistantInfoItem(m.com.Styles, &msg, m.com.Config(), time.Unix(m.sess.lastUserMessageTime, 0))
			m.chat.AppendMessages(infoItem)
			if m.chat.Follow() {
				if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
		}
	case message.System:
		// A notice Sennit wrote about its own doing (a thread merged and
		// cleared away, say). Nothing to animate and nothing to link up —
		// it is one static line.
		items := chat.ExtractMessageItems(m.com.Styles, &msg, nil, m.com.Config())
		if len(items) == 0 {
			return nil
		}
		m.chat.AppendMessages(items...)
		if m.chat.Follow() {
			if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	case message.Tool:
		for _, tr := range msg.ToolResults() {
			toolItem := m.chat.MessageItem(tr.ToolCallID)
			if toolItem == nil {
				// we should have an item!
				continue
			}
			if toolMsgItem, ok := toolItem.(chat.ToolMessageItem); ok {
				toolMsgItem.SetResult(&tr)
				if m.chat.Follow() {
					if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
						cmds = append(cmds, cmd)
					}
				}
			}
		}
		cmds = append(cmds, m.sess.refreshModifiedFiles(m.com, m))
	}
	m.queued.refloat(m.chat, m.com.Styles)
	return tea.Sequence(cmds...)
}

// updateSessionMessage updates an existing message in the current session in
// the chat when an assistant message is updated it may include updated tool
// calls as well that is why we need to handle creating/updating each tool call
// message too.
func (m *UI) updateSessionMessage(msg message.Message) tea.Cmd {
	var cmds []tea.Cmd
	existingItem := m.chat.MessageItem(msg.ID)

	if existingItem != nil {
		if assistantItem, ok := existingItem.(*chat.AssistantMessageItem); ok {
			// SetMessage returns a StartAnimation Cmd when the message
			// transitions back to spinning (e.g. its streamed content was
			// reset for a retry). Propagate it so the spinner re-arms
			// instead of freezing.
			if cmd := assistantItem.SetMessage(&msg); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	}

	shouldRenderAssistant := chat.ShouldRenderAssistantMessage(&msg)
	isEndTurn := msg.FinishPart() != nil && msg.FinishPart().Reason == message.FinishReasonEndTurn
	// If the message of the assistant does not have any response just tool
	// calls we need to remove it, but keep the info item for end-of-turn
	// renders so the footer (model/provider/duration) remains visible when,
	// for example, a hook halts the turn.
	if !shouldRenderAssistant && len(msg.ToolCalls()) > 0 && existingItem != nil {
		m.chat.RemoveMessage(msg.ID)
		if !isEndTurn {
			if infoItem := m.chat.MessageItem(chat.AssistantInfoID(msg.ID)); infoItem != nil {
				m.chat.RemoveMessage(chat.AssistantInfoID(msg.ID))
			}
		}
	}

	if isEndTurn {
		if infoItem := m.chat.MessageItem(chat.AssistantInfoID(msg.ID)); infoItem == nil {
			newInfoItem := chat.NewAssistantInfoItem(m.com.Styles, &msg, m.com.Config(), time.Unix(m.sess.lastUserMessageTime, 0))
			m.chat.AppendMessages(newInfoItem)
		}
	}

	var items []chat.MessageItem
	for _, tc := range msg.ToolCalls() {
		existingToolItem := m.chat.MessageItem(tc.ID)
		if toolItem, ok := existingToolItem.(chat.ToolMessageItem); ok {
			existingToolCall := toolItem.ToolCall()
			// only update if finished state changed or input changed
			// to avoid clearing the cache
			if (tc.Finished && !existingToolCall.Finished) || tc.Input != existingToolCall.Input {
				toolItem.SetToolCall(tc)
			}
		}
		if existingToolItem == nil {
			items = append(items, chat.NewToolMessageItem(m.com.Styles, msg.ID, tc, nil, false, m.com.Config()))
		}
	}

	for _, item := range items {
		if animatable, ok := item.(chat.Animatable); ok {
			if cmd := animatable.StartAnimation(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	}

	m.chat.AppendMessages(items...)
	m.queued.refloat(m.chat, m.com.Styles)
	if m.chat.Follow() {
		if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.chat.SelectLast()
	}

	return tea.Sequence(cmds...)
}

// findNestedToolContainer looks up the top-level tool item in the chat
// whose tool call ID matches toolCallID and that can hold nested
// child-session tool calls (agent / agentic_fetch / custom-agent
// delegations — all three construct an AgentToolMessageItem, see
// chat.NewToolMessageItem). Returns nil if the item is missing or isn't a
// nested-tool container, e.g. a plain (non-delegating) tool call.
func (w *widgets) findNestedToolContainer(toolCallID string) chat.NestedToolContainer {
	item := w.chat.MessageItem(toolCallID)
	if item == nil {
		return nil
	}
	toolMessageItem, ok := item.(chat.ToolMessageItem)
	if !ok || toolMessageItem.ToolCall().ID != toolCallID {
		return nil
	}
	container, ok := item.(chat.NestedToolContainer)
	if !ok {
		return nil
	}
	return container
}
