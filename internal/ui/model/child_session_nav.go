package model

import (
	"encoding/json"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/chat"
	"github.com/rave-soft/sennit/internal/ui/chatlist"
	"github.com/rave-soft/sennit/internal/ui/common"
)

// viewingChildSession reports whether the UI is currently navigated into a
// sub-agent's session (i.e. the nav stack is non-empty). Used to make child
// sessions read-only and to keep the editor from stealing focus while one
// is being viewed.
func (m *UI) viewingChildSession() bool {
	return len(m.sess.navStack) > 0
}

// clearChildSessionNav drops the child-session navigation stack outright,
// for callers that replace the current session rather than navigate out of
// a delegation (new session, the session picker, a deleted-session
// redirect). Left alone, navStack only shrinks via exitChildSession /
// abandonChildSessionEntry, so a session swap that bypassed both stranded
// the UI in "viewing subagent session": sendMessage kept refusing, the
// threads/agents panels stayed hidden, and the breadcrumbs still pointed at
// the abandoned child. Mirrors what those pop paths already do when they
// empty the stack.
func (m *UI) clearChildSessionNav() {
	if len(m.sess.navStack) == 0 {
		return
	}
	m.sess.navStack = nil
	m.focus = uiFocusEditor
	m.chat.Blur()
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
func nestedToolContainerRefs(c *chatlist.Chat) []childSessionRef {
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

// refreshDelegationBlocks re-syncs both handoffs the transcript's
// delegation blocks depend on: which of them the panel is showing (so the
// transcript does not show them too), and which have no child session to
// open yet (so they do not offer a drill-in that would do nothing). Both
// answers change on the same events — a delegation starting, running,
// finishing — which is why they are pushed together.
func (m *UI) refreshDelegationBlocks() {
	m.chat.SetDelegationsHidden(m.panelledDelegations())
	m.chat.SetDelegationsUnopenable(m.unstartedDelegations())
}

// unstartedDelegations names the loaded delegations that cannot be opened
// yet, by the id of the tool call that started each. Nil when they all
// can, which is the ordinary case.
func (m *UI) unstartedDelegations() map[string]bool {
	var ids map[string]bool
	for _, item := range m.chat.NestedToolContainers() {
		id := item.ToolCall().ID
		if m.delegationStarted(id) {
			continue
		}
		if ids == nil {
			ids = make(map[string]bool)
		}
		ids[id] = true
	}
	return ids
}

// delegationStarted reports whether the delegation identified by
// toolCallID has a child session behind it yet, i.e. whether opening it
// would land on a transcript rather than on nothing.
//
// The sub-session row is written when the delegation's run is launched,
// which can be seconds after the model emitted the tool call: the call
// waits its turn behind its siblings, and the delegate's prompt and tools
// still have to be built. In that window the block is already on screen,
// spinner and all, with no session under it. Opening it there pushed a
// nav frame, blurred the editor and then rolled all of it back when the
// load came back not-found — a click that could only ever produce a
// status-bar message.
//
// A delegation whose tool call has come back — or was canceled — is past
// that window: a result exists, so the run that produced it did too. One
// with no result yet is either running or not started, and the only thing
// that tells those apart is whether the task record naming its session
// exists — the same record the panel's agents section reads, which is why
// "has it shown up in the panel yet" is what this comes down to.
func (m *UI) delegationStarted(toolCallID string) bool {
	item, ok := m.chat.MessageItem(toolCallID).(chat.ToolMessageItem)
	if !ok {
		// Not in the loaded window (a sibling cycled to, a panel block):
		// nothing to judge it by here, so let the load answer.
		return true
	}
	if item.Status() == chat.ToolStatusCanceled {
		return true
	}
	if reporter, ok := item.(chat.ToolResultReporter); ok && reporter.HasResult() {
		return true
	}
	return m.delegationHasTaskRecord(toolCallID)
}

// delegationHasTaskRecord reports whether a task record carrying this
// delegation's child session id is known. The record is created before
// the session and only *named* with the session id once that session
// exists (see TaskManager.Create's SetSession), so a parseable id here
// means the session it points at has already been written.
//
// Every uncertain case answers yes: a workspace that lists no tasks at
// all, and a list that has never been fetched, say nothing about this
// delegation, and refusing to open one on that silence would break the
// ordinary case to fix the rare one. The not-found path in updateSession
// stays as the backstop for what slips through.
func (m *UI) delegationHasTaskRecord(toolCallID string) bool {
	if m.com == nil || m.com.Workspace == nil || !m.com.Workspace.SupportsTasks() {
		return true
	}
	if m.agentList.cache.Timestamp.IsZero() {
		return true
	}
	for _, task := range m.agentList.cache.Value {
		if _, id, ok := session.ParseAgentToolSessionID(task.SessionID); ok && id == toolCallID {
			return true
		}
	}
	return false
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
	// A delegation that has not started has nothing to open: no frame, no
	// load, no message. See delegationStarted.
	if !m.delegationStarted(toolCallID) {
		return nil
	}

	childID := session.CreateAgentToolSessionID(messageID, toolCallID)

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

	// Orientation ("main › agent1 (2/3)", model/effort, tokens, state)
	// lives entirely in drawChildSessionPanel, which replaces the editor.
	// Unlike also posting a status-bar breadcrumb, this doesn't render as
	// a full-width green bar under the panel (InfoTypeInfo is styled
	// identically to InfoTypeSuccess, see quickstyle.go) — redundant with
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
// The returned cmd re-focuses the editor when the stack empties, exactly
// as exitChildSession does on the same transition. Setting m.focus alone
// is not enough: entering blurred the textarea, and nothing in Update's
// tail focuses it back, so a rolled-back entry left the editor greyed out
// and refusing input on a chat that was otherwise perfectly usable.
func (m *UI) abandonChildSessionEntry(sessionID string) (bool, tea.Cmd) {
	if len(m.sess.navStack) == 0 {
		return false, nil
	}
	if m.sess.navStack[len(m.sess.navStack)-1].childSessionID != sessionID {
		return false, nil
	}
	m.sess.navStack = m.sess.navStack[:len(m.sess.navStack)-1]
	if len(m.sess.navStack) > 0 {
		return true, nil
	}
	m.focus = uiFocusEditor
	m.chat.Blur()
	return true, m.editor.textarea.Focus()
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
	frame.childSessionID = session.CreateAgentToolSessionID(sibling.messageID, sibling.toolCallID)

	return m.requestSessionLoad(frame.childSessionID)
}

// handleChildSessionUpdate propagates a child agent-tool session's running
// token count and todo list up to the parent delegation's block. Best-
// effort: it's a no-op when the session isn't an agent-tool child session,
// or the parent item can't be found (e.g. scrolled out of the loaded
// window).
func (w *widgets) handleChildSessionUpdate(payload session.Session) {
	_, toolCallID, ok := session.ParseAgentToolSessionID(payload.ID)
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
	_, toolCallID, ok := session.ParseAgentToolSessionID(childSessionID)
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
