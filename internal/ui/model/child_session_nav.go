package model

import (
	"encoding/json"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/ui/chat"
)

// viewingChildSession reports whether the UI is currently navigated into a
// sub-agent's session (i.e. the nav stack is non-empty). Used to make child
// sessions read-only and to keep the editor from stealing focus while one
// is being viewed.
func (m *UI) viewingChildSession() bool {
	return len(m.sess.navStack) > 0
}

// childSessionSiblingCount returns the number of sibling delegations in the
// top nav-stack frame, i.e. how many sub-agents alt+left/alt+right can cycle
// through. Zero when not viewing a child session.
func (m *UI) childSessionSiblingCount() int {
	if len(m.sess.navStack) == 0 {
		return 0
	}
	return len(m.sess.navStack[len(m.sess.navStack)-1].siblings)
}

// childSessionBreadcrumbMaxLen is the approximate max length of the prompt
// snippet shown in the child-session breadcrumb before it's truncated.
const childSessionBreadcrumbMaxLen = 40

// childSessionLabel builds a short label for a nested-tool-container item
// (agent / agentic_fetch delegation), taken from the first line of its
// running prompt. Falls back to "subagent" if the item's input can't be
// parsed or carries no prompt.
func childSessionLabel(item chat.ToolMessageItem) string {
	var p struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(item.ToolCall().Input), &p); err != nil {
		return "subagent"
	}
	prompt := strings.TrimSpace(p.Prompt)
	if prompt == "" {
		return "subagent"
	}
	if i := strings.IndexByte(prompt, '\n'); i >= 0 {
		prompt = prompt[:i]
	}
	if r := []rune(prompt); len(r) > childSessionBreadcrumbMaxLen {
		prompt = string(r[:childSessionBreadcrumbMaxLen]) + "…"
	}
	return prompt
}

// enterChildSession pushes a navigation frame for the currently loaded
// session and returns a tea.Cmd that loads the child (sub-agent) session
// identified by messageID/toolCallID. The sibling list is built from the
// nested-tool-container items already loaded in the chat, so cycling with
// alt+left/alt+right doesn't require re-fetching the parent.
//
// Restoring the parent's exact scroll position on the way back is
// deliberately not attempted — loadSession's normal load path already
// leaves a freshly loaded session in a reasonable default scroll state,
// and reconstructing the old viewport would be fragile and not worth the
// complexity for this step.
func (m *UI) enterChildSession(messageID, toolCallID string) tea.Cmd {
	childID := m.com.Workspace.CreateAgentToolSessionID(messageID, toolCallID)

	// m.sess.current still refers to the parent here — loadSession is async and
	// doesn't repoint it synchronously — so this is the last cheap chance
	// to capture the parent's title for the breadcrumb.
	parentTitle := m.sess.current.Title

	siblings := m.chat.NestedToolContainerRefs()
	siblingIndex := 0
	for i, s := range siblings {
		if s.messageID == messageID && s.toolCallID == toolCallID {
			siblingIndex = i
			break
		}
	}

	label := "subagent"
	var agentName, model, effort string
	var delegationStart time.Time
	var delegationDuration time.Duration
	if item, ok := m.chat.MessageItem(toolCallID).(chat.ToolMessageItem); ok {
		label = childSessionLabel(item)
		agentName, model, effort, delegationStart, delegationDuration = delegationInfo(item)
	}

	m.sess.navStack = append(m.sess.navStack, sessionNavFrame{
		parentSessionID:    m.sess.current.ID,
		parentTitle:        parentTitle,
		label:              label,
		siblings:           siblings,
		siblingIndex:       siblingIndex,
		agentName:          agentName,
		model:              model,
		effort:             effort,
		delegationStart:    delegationStart,
		delegationDuration: delegationDuration,
	})

	// Child sessions are read-only: keep focus/keys on the chat list and
	// don't let the editor hold focus while viewing one.
	m.focus = uiFocusMain
	m.editor.textarea.Blur()

	// Orientation ("main › agent1 (2/3)", model/effort, tokens, state) now
	// lives entirely in drawChildSessionPanel, which replaces the editor —
	// this used to also post a status-bar breadcrumb, but InfoTypeInfo is
	// styled identically to InfoTypeSuccess (see quickstyle.go), so it
	// rendered as a full-width green bar under the panel. Redundant with
	// the panel and visually loud for what's just a location cue.

	return m.requestSessionLoad(childID)
}

// exitChildSession pops the top navigation frame and returns a tea.Cmd
// that loads the session it points back to. No-op if the stack is empty
// (e.g. alt+up pressed on a top-level session).
func (m *UI) exitChildSession() tea.Cmd {
	if len(m.sess.navStack) == 0 {
		return nil
	}
	frame := m.sess.navStack[len(m.sess.navStack)-1]
	m.sess.navStack = m.sess.navStack[:len(m.sess.navStack)-1]
	if len(m.sess.navStack) == 0 {
		// Back at a top-level session: restore normal editor focus, since
		// Tab no longer offers a manual way back in.
		m.focus = uiFocusEditor
		m.chat.Blur()
		return tea.Batch(m.requestSessionLoad(frame.parentSessionID), m.editor.textarea.Focus())
	}
	return m.requestSessionLoad(frame.parentSessionID)
}

// cycleChildSession moves the sibling index of the current navigation
// frame by delta (wrapping) and returns a tea.Cmd that loads the newly
// selected sibling's child session. It updates the existing top frame in
// place rather than pushing a new one — the delegations are still
// siblings under the same parent. No-op if there's no active frame or
// fewer than two siblings to cycle through.
func (m *UI) cycleChildSession(delta int) tea.Cmd {
	if len(m.sess.navStack) == 0 {
		return nil
	}
	frame := &m.sess.navStack[len(m.sess.navStack)-1]
	n := len(frame.siblings)
	if n < 2 {
		return nil
	}
	frame.siblingIndex = ((frame.siblingIndex+delta)%n + n) % n
	sibling := frame.siblings[frame.siblingIndex]

	// The sibling's own tool-call item generally isn't in m.chat here —
	// m.chat currently holds the child session we're navigating away from,
	// not the parent — so this lookup routinely misses and falls back to
	// the generic "subagent" label. frame.parentTitle was captured at
	// enterChildSession time and doesn't have that problem.
	label := "subagent"
	var agentName, model, effort string
	var delegationStart time.Time
	var delegationDuration time.Duration
	if item, ok := m.chat.MessageItem(sibling.toolCallID).(chat.ToolMessageItem); ok {
		label = childSessionLabel(item)
		agentName, model, effort, delegationStart, delegationDuration = delegationInfo(item)
	}
	frame.label = label
	frame.agentName, frame.model, frame.effort = agentName, model, effort
	frame.delegationStart, frame.delegationDuration = delegationStart, delegationDuration

	return m.requestSessionLoad(m.com.Workspace.CreateAgentToolSessionID(sibling.messageID, sibling.toolCallID))
}

// handleChildSessionUpdate propagates a child agent-tool session's running
// token count and todo list up to the parent delegation's block. Best-
// effort: it's a no-op when the session isn't an agent-tool child session,
// or the parent item can't be found (e.g. scrolled out of the loaded
// window).
func (m *UI) handleChildSessionUpdate(payload session.Session) {
	_, toolCallID, ok := m.com.Workspace.ParseAgentToolSessionID(payload.ID)
	if !ok {
		return
	}
	container := m.findNestedToolContainer(toolCallID)
	if container == nil {
		return
	}
	if tracker, ok := container.(chat.ChildSessionTokenTracker); ok {
		tracker.SetChildSessionTokens(payload.PromptTokens, payload.CompletionTokens)
	}
	if tracker, ok := container.(chat.ChildSessionTodoTracker); ok {
		tracker.SetChildSessionTodos(payload.Todos)
	}
}

// handleChildSessionMessage handles messages from child sessions (agent tools).
func (m *UI) handleChildSessionMessage(event pubsub.Event[message.Message]) tea.Cmd {
	var cmds []tea.Cmd

	// Only process messages with tool calls or results.
	if len(event.Payload.ToolCalls()) == 0 && len(event.Payload.ToolResults()) == 0 {
		return nil
	}

	// Check if this is an agent tool session and parse it.
	childSessionID := event.Payload.SessionID
	_, toolCallID, ok := m.com.Workspace.ParseAgentToolSessionID(childSessionID)
	if !ok {
		return nil
	}

	agentItem := m.findNestedToolContainer(toolCallID)
	if agentItem == nil {
		return nil
	}

	// Get existing nested tools.
	nestedTools := agentItem.NestedTools()

	// Update or create nested tool calls.
	for _, tc := range event.Payload.ToolCalls() {
		found := false
		for _, existingTool := range nestedTools {
			if existingTool.ToolCall().ID == tc.ID {
				existingTool.SetToolCall(tc)
				found = true
				break
			}
		}
		if !found {
			// Create a new nested tool item.
			nestedItem := chat.NewToolMessageItem(m.com.Styles, event.Payload.ID, tc, nil, false, m.com.Config())
			if simplifiable, ok := nestedItem.(chat.Compactable); ok {
				simplifiable.SetCompact(true)
			}
			if animatable, ok := nestedItem.(chat.Animatable); ok {
				if cmd := animatable.StartAnimation(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
			nestedTools = append(nestedTools, nestedItem)
		}
	}

	// Update nested tool results.
	for _, tr := range event.Payload.ToolResults() {
		for _, nestedTool := range nestedTools {
			if nestedTool.ToolCall().ID == tr.ToolCallID {
				nestedTool.SetResult(&tr)
				break
			}
		}
	}

	// Update the agent item with the new nested tools.
	agentItem.SetNestedTools(nestedTools)

	// Update the chat so it updates the index map for animations to work as expected
	m.chat.UpdateNestedToolIDs(toolCallID)

	if m.chat.Follow() {
		if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.chat.SelectLast()
	}

	return tea.Sequence(cmds...)
}
