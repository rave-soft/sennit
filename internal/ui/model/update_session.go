package model

import (
	"errors"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/history"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/ui/util"
)

// updateSession handles the session-lifecycle branches of UI.Update: session
// list/load/create/send flows and the session, message, and history-file
// pubsub events. It is called from Update's message-type switch and shares
// that switch's cmds accumulator.
//
// The second return value reports whether a branch below took one of
// Update's early-return paths (return m, tea.Batch(cmds...)): when true,
// the caller must return immediately with the returned cmds, bypassing the
// rest of Update's tail (the focus/placeholder switch, stale-workspace
// refresh, and attachment update) exactly as the original inline case did.
// When false, a branch fell through instead, and the caller must continue
// running that tail with the returned cmds, exactly as falling out of the
// original case body would.
func (m *UI) updateSession(msg tea.Msg, cmds []tea.Cmd) ([]tea.Cmd, bool) {
	switch msg := msg.(type) {
	case sessionsLoadedMsg:
		if cmd := m.applySessionsLoaded(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case busyStateMsg:
		cmds = append(cmds, m.applyBusyState(msg)...)
	case promptQueueMsg:
		cmds = append(cmds, m.applyPromptQueue(msg)...)
	case agentRunSubmittedMsg:
		if m.sess.loadExpectedID != "" && (msg.sessionID != m.sess.loadExpectedID || msg.loadGeneration != m.sess.loadGen) {
			break
		}
		// A prompt was just accepted (run started or enqueued): fetch the
		// authoritative busy/queue state to confirm the optimistic values
		// sendMessage wrote.
		m.wsCache.invalidateBusyCaches()
		m.wsCache.invalidatePromptQueue()
		if cmd := m.dispatchBusyRefresh(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		if cmd := m.dispatchPromptQueueRefresh(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.editor.pendingSendActive = false
		if len(m.editor.pendingSendQueue) > 0 {
			cmds = append(cmds, func() tea.Msg { return sendPendingQueueMsg{} })
		}
	case loadSessionMsg:
		if msg.gen != m.sess.loadGen || msg.sessionID != m.sess.loadExpectedID {
			break
		}
		if msg.err != nil {
			// On error: discard pending sends and clear stale queue.
			m.editor.pendingSendQueue = nil
			m.editor.pendingSendGen = 0
			m.editor.pendingSendLoading = false
			// The nav frame rolls back below, so m.sess.current is (or
			// remains) the parent session — but nothing else resets
			// loadExpectedID. Left pointing at the session that just failed
			// to load, sendMessage's "still waiting on a load" check
			// (loadExpectedID != current.ID) keeps matching forever, so
			// every later prompt is queued for a session that will never
			// answer instead of running against the parent that is actually
			// on screen.
			m.sess.loadExpectedID = ""
			// A delegation the person opened before it had begun. The
			// sub-session row is written at the top of runSubAgent, so
			// between the model emitting the tool call and that call
			// getting its turn to execute — queued behind its siblings,
			// or waiting on the delegate's prompt and tools to finish
			// building — there is a window, sometimes seconds long,
			// where the delegation is on screen and its session does not
			// exist yet. Opening it there produced a raw
			// `session: no such session: "<uuid>$$call_..."` in the
			// status bar and left the UI stuck inside a child view of
			// nothing.
			//
			// Every failed entry rolls back, whatever went wrong; only
			// the not-found case gets the gentler wording, since that is
			// the one that is not really an error at all.
			abandoned := m.abandonChildSessionEntry(msg.sessionID)
			if abandoned && errors.Is(msg.err, session.ErrNotFound) {
				cmds = append(cmds, util.ReportWarn("This delegation has not started yet"))
				break
			}
			cmds = append(cmds, util.ReportError(msg.err))
			break
		}
		if m.lay.forceCompactMode {
			m.lay.isCompact = true
		}
		m.setState(uiChat, m.focus)
		m.sess.current = msg.session
		m.sess.modelUsed = msg.modelUsed
		// The chat is about to be replaced wholesale; placeholders belong
		// to the list that is going away.
		m.clearQueuedPrompts()
		m.sidebar.offset = 0
		m.sess.files = msg.files
		m.sess.filesVersion++
		// Session switch: the memoized busy state and queued prompts
		// belong to the previous session. Drop them and re-fetch
		// off-thread so the queue pill and esc behavior track the new
		// session instead of a stale one.
		m.wsCache.invalidateBusyCaches()
		m.wsCache.invalidatePromptQueue()
		if msg.modelSwitched {
			// Loading the session moved the instance onto the model it is
			// pinned to. The rebuild has already landed by the time this
			// message exists, so the memoized model only needs re-probing.
			cmds = append(cmds, agentModelChangedCmd)
		}
		// invalidatePromptQueue above already zeroed the cache's timestamp
		// (marking it stale) and bumped its generation; only the displayed
		// items belong to the departed session and need clearing too.
		m.wsCache.promptQueueCache.Value = nil
		if cmd := m.dispatchBusyRefresh(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		if cmd := m.dispatchPromptQueueRefresh(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		cmds = append(cmds, startLSPs(m.com, msg.lspFilePaths()))
		if cmd := m.applySessionMessageItems(msg.items, msg.lastUserMessageTime); cmd != nil {
			cmds = append(cmds, cmd)
		}
		if cmd := m.autoExpandTodosIfReasonable(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		if cmd := m.syncPanelSpinner(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		cmds = append(cmds, m.sess.reportCurrentSession(m.com, msg.sessionID))
		if hasInProgressTodo(m.sess.current.Todos) {
			m.updateLayoutAndSize()
		}
		// Reload prompt history for the new session.
		m.editor.historyReset()
		cmds = append(cmds, m.sess.loadPromptHistory(m.com))

		m.editor.pendingSendLoading = false
		m.editor.pendingSendActive = false
		if len(m.editor.pendingSendQueue) > 0 {
			cmds = append(cmds, func() tea.Msg { return sendPendingQueueMsg{} })
		}
		m.updateLayoutAndSize()

	case createSessionMsg:
		if !m.editor.pendingSendLoading || msg.generation != m.editor.pendingSendGen {
			return cmds, false
		}
		expectedLoadGeneration := m.sess.loadGen + 1
		for i := range m.editor.pendingSendQueue {
			if m.editor.pendingSendQueue[i].generation == msg.generation {
				m.editor.pendingSendQueue[i].sessionID = msg.session.ID
				m.editor.pendingSendQueue[i].loadGeneration = expectedLoadGeneration
			}
		}
		if m.lay.forceCompactMode {
			m.lay.isCompact = true
		}
		m.sess.current = &msg.session
		m.setState(uiChat, m.focus)
		// Request loading the chat for the new session, then dispatch
		// sendMessage once the session is loaded.
		m.editor.pendingSendQueue = append([]sendQueueItem{{
			content:        msg.content,
			attachments:    msg.attachments,
			generation:     msg.generation,
			sessionID:      msg.session.ID,
			loadGeneration: expectedLoadGeneration,
		}}, m.editor.pendingSendQueue...)
		cmds = append(cmds, m.requestSessionLoad(msg.session.ID))
		return cmds, true

	case requestSessionLoad:
		cmds = append(cmds, m.beginSessionLoad(msg.sessionID))

	case sessionFilesUpdatesMsg:
		// Drop a stale reply from a session the user has since switched
		// away from — otherwise it clobbers the sidebar's file list with
		// another session's files.
		if m.sess.current == nil || msg.sessionID != m.sess.current.ID {
			break
		}
		m.sess.files = msg.sessionFiles
		m.sess.filesVersion++
		var paths []string
		for _, f := range msg.sessionFiles {
			paths = append(paths, f.LatestVersion.Path)
		}
		cmds = append(cmds, startLSPs(m.com, paths))

	case sendMessageMsg:
		cmds = append(cmds, m.sendMessage(msg.Content, msg.Attachments...))

	case pubsub.Event[session.Session]:
		if msg.Type == pubsub.DeletedEvent {
			if m.sess.current != nil && m.sess.current.ID == msg.Payload.ID {
				if cmd := m.newSession(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
			break
		}
		if m.sess.current != nil && msg.Payload.ID == m.sess.current.ID {
			prevTodosLen := len(m.sess.current.Todos)
			// mainRect.Dy() as of the last layout pass — main and panel
			// together reconstruct it, since generateLayout splits mainRect
			// into exactly those two rects. Only used here to detect
			// whether the panel's footprint changed, not as an actual
			// layout budget, so approximating off the last computed layout
			// (rather than recomputing mainRect from scratch) is fine.
			available := m.lay.layout.main.Dy() + m.lay.layout.panel.Dy()
			prevPanelHeight := m.sessionPanelHeight(available)
			m.sess.current = &msg.Payload
			// syncPanelSpinner is idempotent and self-guarding — no need
			// to pre-compute the in-progress edge here.
			if cmd := m.syncPanelSpinner(); cmd != nil {
				cmds = append(cmds, cmd)
			}
			// The session panel reserves vertical space that the chat area
			// must yield. Recompute the layout whenever that footprint
			// changes (todos appearing, the list growing, etc.) so the
			// panel renders on first paint rather than waiting for a
			// toggle. drawSessionPanel always paints from live state, so
			// no extra re-render is needed when the footprint is
			// unchanged (e.g. the in-progress spinner just ticks on the
			// next frame).
			if m.sessionPanelHeight(available) != prevPanelHeight {
				m.updateLayoutAndSize()
			}
			// While the panel is showing this session's todos, the chat
			// transcript's own todos tool call(s) draw nothing at all
			// rather than duplicating the list or announcing it in a
			// one-line stub; once every todo is completed and the panel
			// disappears, the transcript becomes the permanent record
			// again.
			m.chat.SetTodosHidden(hasIncompleteTodos(m.sess.current.Todos))
			// And the same handoff for delegations: while the panel's
			// agents section has them, they have no row in the transcript.
			m.chat.SetDelegationsHidden(m.panelledDelegations())
			// A brand new list (0 -> N todos) always opens the panel,
			// unconditionally — distinct from autoExpandTodosIfReasonable
			// below, which is a gentler one-shot-per-session, tall-enough-
			// terminal nicety for the "resumed a session that already had
			// an active list" case. This only fires on the transition
			// itself: a later update to the same list (items added,
			// statuses changed) must respect a user's manual collapse.
			if prevTodosLen == 0 && len(m.sess.current.Todos) > 0 {
				m.panel.expanded = true
			}
			m.autoExpandTodosIfReasonable()
		} else {
			// Not the current session — it may be a running delegation's
			// child session, updated as its own turns complete. Surface
			// its running token count on the parent's status line.
			m.handleChildSessionUpdate(m.com, msg.Payload)
		}
	case pubsub.Event[message.Message]:
		// Check if this is a child session message for an agent tool.
		if m.sess.current == nil {
			break
		}
		if msg.Payload.SessionID != m.sess.current.ID {
			// This might be a child session message from an agent tool.
			if cmd := m.handleChildSessionMessage(m.com, msg); cmd != nil {
				cmds = append(cmds, cmd)
			}
			break
		}
		m.sess.recordAssistantModel(msg.Payload)
		switch msg.Type {
		case pubsub.CreatedEvent:
			cmds = append(cmds, m.appendSessionMessage(msg.Payload))
			// A new message is a run boundary — a user prompt starting
			// a turn or the agent replying/dequeueing. Drop the
			// memoized busy state and re-fetch it and the queue
			// off-thread. Per-chunk UpdatedEvents deliberately do NOT
			// trigger this: during streaming that would put workspace
			// probes on every token.
			m.wsCache.invalidateBusyCaches()
			m.wsCache.invalidatePromptQueue()
			if cmd := m.dispatchBusyRefresh(); cmd != nil {
				cmds = append(cmds, cmd)
			}
			if cmd := m.dispatchPromptQueueRefresh(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		case pubsub.UpdatedEvent:
			cmds = append(cmds, m.updateSessionMessage(msg.Payload))
		case pubsub.DeletedEvent:
			m.chat.RemoveMessage(msg.Payload.ID)
		}
		// Reconcile the spinner with the new message's implications
		// (a turn starting or ending changes what's live).
		if cmd := m.syncPanelSpinner(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case pubsub.Event[history.File]:
		cmds = append(cmds, m.sess.handleFileEvent(m.com, msg.Payload))

	case sendMessageErrorMsg:
		if !msg.creating && m.sess.loadExpectedID != "" && (msg.sessionID != m.sess.loadExpectedID || msg.loadGeneration != m.sess.loadGen) {
			break
		}
		m.editor.pendingSendActive = false
		if msg.creating && msg.generation == m.editor.pendingSendGen {
			m.editor.pendingSendLoading = false
			m.editor.pendingSendQueue = nil
		}
		cmds = append(cmds, util.ReportError(msg.Err))
		m.wsCache.agentBusyCache.Set(false)
		if !msg.creating && len(m.editor.pendingSendQueue) > 0 {
			cmds = append(cmds, func() tea.Msg { return sendPendingQueueMsg{} })
		}

	case sendPendingQueueMsg:
		if m.editor.pendingSendActive || len(m.editor.pendingSendQueue) == 0 || m.sess.current == nil {
			break
		}
		item := m.editor.pendingSendQueue[0]
		m.editor.pendingSendQueue = m.editor.pendingSendQueue[1:]
		if item.sessionID != m.sess.current.ID || item.loadGeneration != m.sess.loadGen {
			if len(m.editor.pendingSendQueue) > 0 {
				cmds = append(cmds, func() tea.Msg { return sendPendingQueueMsg{} })
			}
			break
		}
		m.editor.pendingSendActive = true
		if item.bang {
			cmds = append(cmds, m.runShellCommandInternal(item.content, item.isFirstMessage))
		} else {
			cmds = append(cmds, m.sendMessageNow(item.content, item.attachments...))
		}

	case bangSessionCreatedMsg:
		if !m.editor.pendingSendLoading || msg.generation != m.editor.pendingSendGen {
			break
		}
		expectedLoadGeneration := m.sess.loadGen + 1
		for i := range m.editor.pendingSendQueue {
			if m.editor.pendingSendQueue[i].generation == msg.generation {
				m.editor.pendingSendQueue[i].sessionID = msg.session.ID
				m.editor.pendingSendQueue[i].loadGeneration = expectedLoadGeneration
			}
		}
		m.editor.pendingSendQueue = append([]sendQueueItem{{
			content:        msg.command,
			generation:     msg.generation,
			sessionID:      msg.session.ID,
			loadGeneration: expectedLoadGeneration,
			bang:           true,
			isFirstMessage: msg.isFirstMessage,
		}}, m.editor.pendingSendQueue...)
		m.sess.current = &msg.session
		m.setState(uiChat, m.focus)
		cmds = append(cmds, m.requestSessionLoad(msg.session.ID))
	}
	return cmds, false
}

// applySessionsLoaded opens the sessions dialog with the fetched list once
// it lands. A generation mismatch means a newer openSessionsDialog call
// superseded this fetch (e.g. the user pressed the key again before it
// landed), so the stale result is dropped instead of popping the dialog
// open unexpectedly.
func (m *UI) applySessionsLoaded(msg sessionsLoadedMsg) tea.Cmd {
	if msg.gen != m.sess.dialogGen {
		return nil
	}
	m.sess.dialogLoading = false
	if msg.err != nil {
		return util.ReportError(msg.err)
	}
	if m.dialog.ContainsDialog(dialog.SessionsID) {
		return nil
	}
	m.dialog.OpenDialog(dialog.NewSessions(m.com, msg.sessions, msg.selectedSessionID))
	return nil
}
