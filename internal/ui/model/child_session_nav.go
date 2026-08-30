package model

import (
	"encoding/json"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/chat"
	"github.com/rave-soft/sennit/internal/ui/common"
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
func (s *sessionState) childSessionSiblingCount() int {
	if len(s.navStack) == 0 {
		return 0
	}
	return len(s.navStack[len(s.navStack)-1].siblings)
}

// childSessionBreadcrumbMaxLen is the approximate max length of the prompt
// snippet shown in the child-session breadcrumb before it's truncated.
const childSessionBreadcrumbMaxLen = 40

// defaultChildSessionLabel names a delegation whose prompt snippet
// couldn't be resolved.
const defaultChildSessionLabel = "subagent"

// childSessionLabel builds a short label for a nested-tool-container item
// (agent / agentic_fetch delegation), taken from the first line of its
// running prompt. Falls back to "subagent" if the item's input can't be
// parsed or carries no prompt.
func childSessionLabel(item chat.ToolMessageItem) string {
	var p struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(item.ToolCall().Input), &p); err != nil {
		return defaultChildSessionLabel
	}
	prompt := strings.TrimSpace(p.Prompt)
	if prompt == "" {
		return defaultChildSessionLabel
	}
	if i := strings.IndexByte(prompt, '\n'); i >= 0 {
		prompt = prompt[:i]
	}
	if r := []rune(prompt); len(r) > childSessionBreadcrumbMaxLen {
		prompt = string(r[:childSessionBreadcrumbMaxLen]) + "…"
	}
	return prompt
}

// captureDelegationRef snapshots everything the child-session panel needs
// to describe a delegation — its ids, prompt-snippet label, agent name,
// model/effort override and timing — while its chat item is still loaded.
// nestedToolContainerRefs describes every delegation the chat is currently
// showing. Walking the list is the chat's job and describing a delegation
// is navigation's, which is why the two are separate calls: the walk
// returns items, and the mapping to a ref happens here.
func nestedToolContainerRefs(c *Chat) []childSessionRef {
	items := c.NestedToolContainers()
	refs := make([]childSessionRef, 0, len(items))
	for _, item := range items {
		refs = append(refs, captureDelegationRef(item))
	}
	return refs
}

func captureDelegationRef(item chat.ToolMessageItem) childSessionRef {
	ref := childSessionRef{
		messageID:  item.MessageID(),
		toolCallID: item.ToolCall().ID,
		label:      childSessionLabel(item),
	}
	ref.agentName, ref.model, ref.effort, ref.delegationStart, ref.delegationDuration = delegationInfo(item)
	return ref
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

	siblings := nestedToolContainerRefs(m.chat)
	siblingIndex := 0
	for i, s := range siblings {
		if s.messageID == messageID && s.toolCallID == toolCallID {
			siblingIndex = i
			break
		}
	}

	// The delegation being entered is one of the siblings just captured,
	// so its display data comes from there. The direct lookup is only a
	// fallback for a caller that entered something not in the list.
	var ref childSessionRef
	if siblingIndex < len(siblings) && siblings[siblingIndex].toolCallID == toolCallID {
		ref = siblings[siblingIndex]
	} else if item, ok := m.chat.MessageItem(toolCallID).(chat.ToolMessageItem); ok {
		ref = captureDelegationRef(item)
	}

	frame := sessionNavFrame{
		parentSessionID: m.sess.current.ID,
		parentTitle:     parentTitle,
		siblings:        siblings,
		siblingIndex:    siblingIndex,
		childSessionID:  childID,
	}
	frame.adoptRef(ref)
	m.sess.navStack = append(m.sess.navStack, frame)

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

// abandonChildSessionEntry undoes an entry into sessionID that turned out
// to have nothing behind it, and reports whether it did.
//
// enterChildSession pushes its frame and moves focus before the load it
// asks for has resolved — it has to, because the load is asynchronous and
// the frame is what the arriving result is matched against. So a load
// that fails leaves the UI *inside* a child session that does not exist:
// the editor stays blurred and replaced by the delegation panel, the chat
// stays read-only, and the content on screen is still the parent's. The
// only way out was alt+up, which reads as the app having hung.
//
// Unlike exitChildSession this asks for no load on the way back. Nothing
// was ever replaced — the failed load returned before it could — so the
// parent is already what is on screen, and re-fetching it would throw
// away the person's scroll position to redraw what is already there.
func (m *UI) abandonChildSessionEntry(sessionID string) bool {
	if len(m.sess.navStack) == 0 {
		return false
	}
	if m.sess.navStack[len(m.sess.navStack)-1].childSessionID != sessionID {
		return false
	}
	m.sess.navStack = m.sess.navStack[:len(m.sess.navStack)-1]
	if len(m.sess.navStack) == 0 {
		m.focus = uiFocusEditor
		m.chat.Blur()
	}
	return true
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

	// From the ref, not from m.chat: it holds the child session we're
	// navigating away from, not the parent, so the sibling's own tool-call
	// item isn't there to look up. The ref was captured from the parent
	// chat when the frame was pushed (see NestedToolContainerRefs), the
	// same way frame.parentTitle was.
	frame.adoptRef(sibling)
	frame.childSessionID = m.com.Workspace.CreateAgentToolSessionID(sibling.messageID, sibling.toolCallID)

	return m.requestSessionLoad(frame.childSessionID)
}

// handleChildSessionUpdate propagates a child agent-tool session's running
// token count and todo list up to the parent delegation's block. Best-
// effort: it's a no-op when the session isn't an agent-tool child session,
// or the parent item can't be found (e.g. scrolled out of the loaded
// window).
func (w *widgets) handleChildSessionUpdate(com *common.Common, payload session.Session) {
	_, toolCallID, ok := com.Workspace.ParseAgentToolSessionID(payload.ID)
	if !ok {
		return
	}
	container := w.findNestedToolContainer(toolCallID)
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
func (w *widgets) handleChildSessionMessage(com *common.Common, event pubsub.Event[message.Message]) tea.Cmd {
	var cmds []tea.Cmd

	// Only process messages with tool calls or results.
	if len(event.Payload.ToolCalls()) == 0 && len(event.Payload.ToolResults()) == 0 {
		return nil
	}

	// Check if this is an agent tool session and parse it.
	childSessionID := event.Payload.SessionID
	_, toolCallID, ok := com.Workspace.ParseAgentToolSessionID(childSessionID)
	if !ok {
		return nil
	}

	agentItem := w.findNestedToolContainer(toolCallID)
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
			nestedItem := chat.NewToolMessageItem(com.Styles, event.Payload.ID, tc, nil, false, com.Config())
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
	w.chat.UpdateNestedToolIDs(toolCallID)

	if w.chat.Follow() {
		if cmd := w.chat.ScrollToBottomAndAnimate(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		w.chat.SelectLast()
	}

	return tea.Sequence(cmds...)
}
