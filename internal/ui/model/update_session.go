package model

import (
	"errors"
	"slices"

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
//
// The extracted apply* handlers below come in three shapes, chosen by each
// arm's own control flow, not by convention: a handler returns its own
// commands when its body has a single exit; it takes and returns cmds when
// an early exit inside the body must preserve appends already made to the
// accumulator; it also returns the bool above when the arm itself takes one
// of Update's early-return paths.
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
		cmds = append(cmds, m.applyAgentRunSubmitted(msg)...)
	case loadSessionMsg:
		cmds = m.applyLoadSession(msg, cmds)
	case createSessionMsg:
		return m.applyCreateSession(msg, cmds)
	case requestSessionLoad:
		cmds = append(cmds, m.beginSessionLoad(msg.sessionID))
	case sessionFilesUpdatesMsg:
		cmds = m.applySessionFilesUpdates(msg, cmds)
	case sendMessageMsg:
		cmds = append(cmds, m.sendMessage(msg.Content, msg.Attachments...))
	case pubsub.Event[session.Session]:
		cmds = m.applySessionEvent(msg, cmds)
	case pubsub.Event[message.Message]:
		cmds = m.applyMessageEvent(msg, cmds)
	case pubsub.Event[history.File]:
		cmds = append(cmds, m.sess.refreshModifiedFiles(m.com, m))
	case sendMessageErrorMsg:
		cmds = m.applySendMessageError(msg, cmds)
	case sendPendingQueueMsg:
		cmds = m.applySendPendingQueue(cmds)
	case bangSessionCreatedMsg:
		cmds = m.applyBangSessionCreated(msg, cmds)
	case sessionDescendantCostMsg:
		if m.sess.current != nil && msg.sessionID == m.sess.current.ID {
			m.sess.descendantCost = msg.cost
		}
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

// applyAgentRunSubmitted handles agentRunSubmittedMsg: once a prompt is
// accepted (a run started or was enqueued), it refreshes the busy/queue
// state to confirm the optimistic values sendMessage wrote, and dispatches
// the next queued send if one is waiting. A stale submission — for a load
// generation or session that has since moved on — is dropped.
func (m *UI) applyAgentRunSubmitted(msg agentRunSubmittedMsg) []tea.Cmd {
	if m.sess.loadExpectedID != "" && (msg.sessionID != m.sess.loadExpectedID || msg.loadGeneration != m.sess.loadGen) {
		return nil
	}
	var cmds []tea.Cmd
	// A prompt was just accepted (run started or enqueued): fetch the
	// authoritative busy/queue state to confirm the optimistic values
	// sendMessage wrote.
	m.wsCache.invalidateBusyCaches()
	m.promptQueue.invalidate()
	if cmd := m.dispatchBusyRefresh(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if cmd := m.dispatchPromptQueueRefresh(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	m.editor.pendingSend.finishActive()
	if m.editor.pendingSend.hasQueued() {
		cmds = append(cmds, func() tea.Msg { return sendPendingQueueMsg{uiOwned: uiOwned{owner: m}} })
	}
	return cmds
}

// applyLoadSession handles loadSessionMsg: applies the loaded session (or
// its error) once beginSessionLoad's fetch lands, discarding a stale result
// from a load the user has since superseded (by switching sessions again,
// or by the load generation moving on).
func (m *UI) applyLoadSession(msg loadSessionMsg, cmds []tea.Cmd) []tea.Cmd {
	if msg.gen != m.sess.loadGen || msg.sessionID != m.sess.loadExpectedID {
		return cmds
	}
	if msg.err != nil {
		// On error: discard pending sends and clear stale queue.
		m.editor.pendingSend.discardLoading()
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
		abandoned, refocus := m.abandonChildSessionEntry(msg.sessionID)
		if refocus != nil {
			cmds = append(cmds, refocus)
		}
		if abandoned && errors.Is(msg.err, session.ErrNotFound) {
			return append(cmds, util.ReportWarn("This delegation has not started yet"))
		}
		return append(cmds, util.ReportError(msg.err))
	}
	if m.lay.forceCompactMode {
		m.lay.isCompact = true
	}
	m.setState(uiChat, m.focus)
	m.sess.current = msg.session
	// fileReads has no session ID of its own to key on (see its field
	// doc), so it is only ever correct for whichever session is current.
	// newSession already clears it for a brand new session; a load must
	// clear it too, or a file opened by @ in the session just left carries
	// over here already marked "read" — @ then skips attaching its
	// content, and checkFileFreshness reports it as read when this session
	// never touched it, so the agent can edit a file it was never shown.
	m.sess.fileReads = nil
	m.sess.modelUsed = msg.modelUsed
	// A different session is now current; its delegation total starts
	// at zero rather than carrying over the session just left, and the
	// real figure follows once the fetch below returns.
	m.sess.descendantCost = 0
	cmds = append(cmds, m.refreshDescendantCost(msg.sessionID))
	// The chat is about to be replaced wholesale; placeholders belong
	// to the list that is going away.
	m.queued.clear(m.chat)
	m.sidebar.offset = 0
	m.sess.files = msg.files
	m.sess.filesVersion++
	// Session switch: the memoized busy state and queued prompts
	// belong to the previous session. Drop them and re-fetch
	// off-thread so the queue pill and esc behavior track the new
	// session instead of a stale one.
	m.wsCache.invalidateBusyCaches()
	if msg.modelSwitched {
		// Loading the session moved the instance onto the model it is
		// pinned to. The rebuild has already landed by the time this
		// message exists, so the memoized model only needs re-probing.
		cmds = append(cmds, func() tea.Msg { return agentModelChangedCmd(m) })
	}
	// The old queue belongs to the departed session, so remove it while
	// keeping the replacement fetch stale.
	m.promptQueue.discard()
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
	cmds = append(cmds, m.sess.loadPromptHistory(m.com, m))

	m.editor.pendingSend.finishLoading()
	m.editor.pendingSend.finishActive()
	if m.editor.pendingSend.hasQueued() {
		cmds = append(cmds, func() tea.Msg { return sendPendingQueueMsg{uiOwned: uiOwned{owner: m}} })
	}
	m.updateLayoutAndSize()
	return cmds
}

// applyCreateSession handles createSessionMsg: adopts the newly created
// session, queues the pending send against it, and kicks off its load. It
// reports (cmds, true) — an Update-level early return — once the session
// load has been requested, and (cmds, false) when the message is stale and
// nothing was done.
func (m *UI) applyCreateSession(msg createSessionMsg, cmds []tea.Cmd) ([]tea.Cmd, bool) {
	if !m.editor.pendingSend.acceptsLoadingResult(msg.generation) {
		return cmds, false
	}
	expectedLoadGeneration := m.sess.loadGen + 1
	m.editor.pendingSend.bindQueuedToSession(msg.generation, msg.session.ID, expectedLoadGeneration)
	if m.lay.forceCompactMode {
		m.lay.isCompact = true
	}
	m.sess.current = &msg.session
	// The load requested below re-fetches this once the new session is
	// current; zero it now so the outgoing session's figure doesn't
	// flash on screen for the moment in between.
	m.sess.descendantCost = 0
	m.setState(uiChat, m.focus)
	// Request loading the chat for the new session, then dispatch
	// sendMessage once the session is loaded.
	m.editor.pendingSend.enqueueFront(sendQueueItem{
		content:        msg.content,
		attachments:    msg.attachments,
		generation:     msg.generation,
		sessionID:      msg.session.ID,
		loadGeneration: expectedLoadGeneration,
	})
	cmds = append(cmds, m.requestSessionLoad(msg.session.ID))
	return cmds, true
}

// applySessionFilesUpdates handles sessionFilesUpdatesMsg: refreshes the
// sidebar's file list and restarts LSPs for files that changed, dropping a
// stale reply from a session the user has since switched away from and
// skipping the refresh entirely when nothing actually changed.
func (m *UI) applySessionFilesUpdates(msg sessionFilesUpdatesMsg, cmds []tea.Cmd) []tea.Cmd {
	// Drop a stale reply from a session the user has since switched
	// away from — otherwise it clobbers the sidebar's file list with
	// another session's files.
	if m.sess.current == nil || msg.sessionID != m.sess.current.ID {
		return cmds
	}
	// A reload that found nothing new is not an update. File events
	// arrive for every session in the instance (handleFileEvent leans
	// on the tree-scoped reload rather than filtering them itself),
	// so most reloads land here unchanged — bumping the version would
	// invalidate the sidebar cache and re-walk the LSP paths each
	// time, for the same list.
	if slices.EqualFunc(m.sess.files, msg.sessionFiles, sameSessionFile) {
		return cmds
	}
	m.sess.files = msg.sessionFiles
	m.sess.filesVersion++
	var paths []string
	for _, f := range msg.sessionFiles {
		paths = append(paths, f.LatestVersion.Path)
	}
	return append(cmds, startLSPs(m.com, paths))
}

// applySessionEvent handles pubsub.Event[session.Session]: reacts to the
// current session being deleted (starting a new one) or updated (syncing
// the panel spinner, layout, and todo/delegation display), and forwards an
// update for a different session onto a running delegation's child-session
// tracking.
func (m *UI) applySessionEvent(msg pubsub.Event[session.Session], cmds []tea.Cmd) []tea.Cmd {
	if msg.Type == pubsub.DeletedEvent {
		if m.sess.current != nil && m.sess.current.ID == msg.Payload.ID {
			if cmd := m.newSession(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		} else if m.sess.current != nil && msg.Payload.ParentSessionID != "" {
			// A deleted delegation's cost drops out of the tree total;
			// same broad, over-fetching trigger as the update branch
			// below.
			cmds = append(cmds, m.refreshDescendantCost(m.sess.current.ID))
		}
		return cmds
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
		cmds = append(cmds, m.refreshDescendantCost(msg.Payload.ID))
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
		m.refreshDelegationBlocks()
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
		m.handleChildSessionUpdate(msg.Payload)
		// A non-empty ParentSessionID means this event came from
		// somewhere in a delegation tree, though not necessarily this
		// one — telling whether it belongs to the tree the current
		// session roots would need a lookup of its own, and refreshing
		// on a delegation from an unrelated tree only costs one small
		// indexed query, so the broad trigger is cheaper than being
		// precise about it.
		if m.sess.current != nil && msg.Payload.ParentSessionID != "" {
			cmds = append(cmds, m.refreshDescendantCost(m.sess.current.ID))
		}
	}
	return cmds
}

// applyMessageEvent handles pubsub.Event[message.Message]: appends,
// updates, or removes a chat message for the current session, or routes it
// to a running delegation's child session, and keeps busy/queue state and
// the panel spinner in sync with run boundaries.
func (m *UI) applyMessageEvent(msg pubsub.Event[message.Message], cmds []tea.Cmd) []tea.Cmd {
	// Check if this is a child session message for an agent tool.
	if m.sess.current == nil {
		return cmds
	}
	if msg.Payload.SessionID != m.sess.current.ID {
		// This might be a child session message from an agent tool.
		if cmd := m.handleChildSessionMessage(m.com, msg); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return cmds
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
		m.promptQueue.invalidate()
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
	return cmds
}

// applySendMessageError handles sendMessageErrorMsg: reports the failed
// send, rejects a pending session creation if that is what failed, and
// dispatches the next queued send if one is waiting. A stale error — for a
// load generation or session the user has since moved on from — is
// dropped; the generation/session check above must run first so a stale
// error can never drop a placeholder a newer send is still using.
func (m *UI) applySendMessageError(msg sendMessageErrorMsg, cmds []tea.Cmd) []tea.Cmd {
	if !msg.creating && m.sess.loadExpectedID != "" && (msg.sessionID != m.sess.loadExpectedID || msg.loadGeneration != m.sess.loadGen) {
		return cmds
	}
	m.editor.pendingSend.finishActive()
	if msg.creating && m.editor.pendingSend.matchesGeneration(msg.generation) {
		m.editor.pendingSend.rejectCreation()
	}
	// sendMessage draws a "queued" placeholder before dispatch, on the
	// optimistic assumption the busy turn ahead of it will eventually take
	// it (see queued.show). A failed send never reaches that hand-off, so
	// nothing will ever call queued.deliver for it — left alone, the
	// placeholder sits in chat forever as a prompt that isn't actually
	// queued anywhere. deliver is a safe no-op when this send never showed
	// one (the creating path, or a session that wasn't busy).
	m.queued.deliver(m.chat, msg.content)
	cmds = append(cmds, util.ReportError(msg.Err))
	m.wsCache.agentBusyCache.Set(false)
	if !msg.creating && m.editor.pendingSend.hasQueued() {
		cmds = append(cmds, func() tea.Msg { return sendPendingQueueMsg{uiOwned: uiOwned{owner: m}} })
	}
	return cmds
}

// applySendPendingQueue handles sendPendingQueueMsg: dequeues the next
// pending send and dispatches it (as a bang command or a normal message),
// or re-queues the wakeup when the head of the queue no longer matches the
// current session/load generation. It takes no message, since
// sendPendingQueueMsg carries nothing beyond the owner tag the dispatcher
// already matched on.
func (m *UI) applySendPendingQueue(cmds []tea.Cmd) []tea.Cmd {
	if m.sess.current == nil {
		return cmds
	}
	item, ok := m.editor.pendingSend.dequeue()
	if !ok {
		return cmds
	}
	if item.sessionID != m.sess.current.ID || item.loadGeneration != m.sess.loadGen {
		if m.editor.pendingSend.hasQueued() {
			cmds = append(cmds, func() tea.Msg { return sendPendingQueueMsg{uiOwned: uiOwned{owner: m}} })
		}
		return cmds
	}
	m.editor.pendingSend.beginActive()
	if item.bang {
		cmds = append(cmds, m.runShellCommandInternal(item.content, item.isFirstMessage))
	} else {
		cmds = append(cmds, m.sendMessageNow(item.content, item.attachments...))
	}
	return cmds
}

// applyBangSessionCreated handles bangSessionCreatedMsg: adopts the session
// created for a bang (shell) command, queues the command against it, and
// kicks off its load.
func (m *UI) applyBangSessionCreated(msg bangSessionCreatedMsg, cmds []tea.Cmd) []tea.Cmd {
	if !m.editor.pendingSend.acceptsLoadingResult(msg.generation) {
		return cmds
	}
	expectedLoadGeneration := m.sess.loadGen + 1
	m.editor.pendingSend.bindQueuedToSession(msg.generation, msg.session.ID, expectedLoadGeneration)
	m.editor.pendingSend.enqueueFront(sendQueueItem{
		content:        msg.command,
		generation:     msg.generation,
		sessionID:      msg.session.ID,
		loadGeneration: expectedLoadGeneration,
		bang:           true,
		isFirstMessage: msg.isFirstMessage,
	})
	m.sess.current = &msg.session
	// See applyCreateSession: the load requested below re-fetches this
	// once the new session is current.
	m.sess.descendantCost = 0
	m.setState(uiChat, m.focus)
	return append(cmds, m.requestSessionLoad(msg.session.ID))
}
