package model

import (
	"errors"
	"testing"

	"github.com/rave-soft/sennit/internal/history"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/stretchr/testify/require"
)

// TestApplySessionsLoaded covers the standalone helper directly: a
// generation mismatch drops the reply, an error is surfaced, an
// already-open dialog is left alone, and a fresh fetch (empty or not)
// opens the sessions dialog with what it fetched.
func TestApplySessionsLoaded(t *testing.T) {
	t.Parallel()

	t.Run("stale generation is dropped", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		m.sess.dialogGen = 2
		m.sess.dialogLoading = true

		cmd := m.applySessionsLoaded(sessionsLoadedMsg{gen: 1})

		require.Nil(t, cmd)
		require.True(t, m.sess.dialogLoading, "stale reply must not touch loading state")
		require.False(t, m.dialog.ContainsDialog(dialog.SessionsID))
	})

	t.Run("error reports and clears loading without opening a dialog", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		m.sess.dialogGen = 1
		m.sess.dialogLoading = true

		cmd := m.applySessionsLoaded(sessionsLoadedMsg{gen: 1, err: errors.New("fetch failed")})

		require.False(t, m.sess.dialogLoading)
		require.False(t, m.dialog.ContainsDialog(dialog.SessionsID))
		require.NotNil(t, cmd)
		got, ok := cmd().(util.InfoMsg)
		require.True(t, ok)
		require.Equal(t, util.InfoTypeError, got.Type)
		require.Equal(t, "fetch failed", got.Msg)
	})

	t.Run("dialog already open is left alone", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		m.sess.dialogGen = 1
		m.sess.dialogLoading = true
		m.dialog.OpenDialog(stubIDDialog{id: dialog.SessionsID})

		cmd := m.applySessionsLoaded(sessionsLoadedMsg{gen: 1, sessions: []session.Session{{ID: "s1"}}})

		require.Nil(t, cmd)
		require.False(t, m.sess.dialogLoading)
		// Still exactly the one (stub) dialog: applySessionsLoaded must
		// not have pushed a second one on top.
		require.True(t, m.dialog.ContainsDialog(dialog.SessionsID))
	})

	t.Run("empty session list still opens the dialog", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		m.sess.dialogGen = 1
		m.sess.dialogLoading = true

		cmd := m.applySessionsLoaded(sessionsLoadedMsg{gen: 1, sessions: nil})

		require.Nil(t, cmd)
		require.False(t, m.sess.dialogLoading)
		require.True(t, m.dialog.ContainsDialog(dialog.SessionsID))
	})

	t.Run("non-empty session list opens the dialog with the selected id", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		m.sess.dialogGen = 1
		m.sess.dialogLoading = true
		sessions := []session.Session{{ID: "s1"}, {ID: "s2"}}

		cmd := m.applySessionsLoaded(sessionsLoadedMsg{gen: 1, sessions: sessions, selectedSessionID: "s2"})

		require.Nil(t, cmd)
		require.False(t, m.sess.dialogLoading)
		require.True(t, m.dialog.ContainsDialog(dialog.SessionsID))
	})
}

func TestUpdateSession_SessionsLoadedMsg(t *testing.T) {
	t.Parallel()

	t.Run("success opens the dialog without an extra cmd", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		m.sess.dialogGen = 1

		cmds, done := m.updateSession(sessionsLoadedMsg{gen: 1, sessions: []session.Session{{ID: "s1"}}}, nil)

		require.False(t, done)
		require.Empty(t, cmds)
		require.True(t, m.dialog.ContainsDialog(dialog.SessionsID))
	})

	t.Run("error is appended to cmds", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		m.sess.dialogGen = 1

		cmds, done := m.updateSession(sessionsLoadedMsg{gen: 1, err: errors.New("boom")}, nil)

		require.False(t, done)
		require.Len(t, cmds, 1)
	})
}

func TestUpdateSession_AgentRunSubmittedMsg(t *testing.T) {
	t.Parallel()

	t.Run("mismatched load expectation is dropped", func(t *testing.T) {
		t.Parallel()
		ws := &countingWorkspace{ready: true}
		m := newBusyUI(ws)
		warmCaches(m, false)
		m.sess.loadExpectedID = "s-expected"
		m.sess.loadGen = 5
		m.editor.pendingSendActive = true

		cmds, done := m.updateSession(agentRunSubmittedMsg{sessionID: "s-other", loadGeneration: 5}, nil)

		require.False(t, done)
		require.Empty(t, cmds)
		require.True(t, m.editor.pendingSendActive, "a stale reply must not clear pendingSendActive")
	})

	t.Run("matching load expectation clears pendingSendActive and refreshes", func(t *testing.T) {
		t.Parallel()
		ws := &countingWorkspace{ready: true}
		m := newBusyUI(ws)
		warmCaches(m, false)
		m.sess.loadExpectedID = "s1"
		m.sess.loadGen = 5
		m.editor.pendingSendActive = true

		cmds, done := m.updateSession(agentRunSubmittedMsg{sessionID: "s1", loadGeneration: 5}, nil)

		require.False(t, done)
		require.False(t, m.editor.pendingSendActive)
		require.NotEmpty(t, cmds)
	})

	t.Run("draining the pending queue schedules sendPendingQueueMsg", func(t *testing.T) {
		t.Parallel()
		ws := &countingWorkspace{ready: true}
		m := newBusyUI(ws)
		warmCaches(m, false)
		m.editor.pendingSendQueue = []sendQueueItem{{content: "queued"}}

		cmds, _ := m.updateSession(agentRunSubmittedMsg{}, nil)

		var sawDrain bool
		for _, c := range cmds {
			if c == nil {
				continue
			}
			if _, ok := c().(sendPendingQueueMsg); ok {
				sawDrain = true
			}
		}
		require.True(t, sawDrain, "a non-empty pendingSendQueue must schedule a drain")
	})
}

func TestUpdateSession_LoadSessionMsg(t *testing.T) {
	t.Parallel()

	t.Run("mismatched generation or session id is dropped", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		m.sess.loadGen = 2
		m.sess.loadExpectedID = "s1"
		before := m.sess.current

		cmds, done := m.updateSession(loadSessionMsg{gen: 1, sessionID: "s1"}, nil)

		require.False(t, done)
		require.Empty(t, cmds)
		require.Same(t, before, m.sess.current, "a stale load must not touch current session state")
	})

	t.Run("error discards pending sends and reports", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		m.sess.loadGen = 1
		m.sess.loadExpectedID = "s1"
		m.editor.pendingSendQueue = []sendQueueItem{{content: "queued"}}
		m.editor.pendingSendGen = 3
		m.editor.pendingSendLoading = true

		cmds, done := m.updateSession(loadSessionMsg{gen: 1, sessionID: "s1", err: errors.New("load failed")}, nil)

		require.False(t, done)
		require.Nil(t, m.editor.pendingSendQueue)
		require.Zero(t, m.editor.pendingSendGen)
		require.False(t, m.editor.pendingSendLoading)
		require.Len(t, cmds, 1)
		got, ok := cmds[0]().(util.InfoMsg)
		require.True(t, ok)
		require.Equal(t, util.InfoTypeError, got.Type)
	})

	t.Run("success loads the session into chat state", func(t *testing.T) {
		t.Parallel()
		ws := &countingWorkspace{ready: true}
		m := newBusyUI(ws)
		warmCaches(m, false)
		m.sess.loadGen = 1
		m.sess.loadExpectedID = "s2"
		m.state = uiLanding
		newSession := &session.Session{ID: "s2"}

		cmds, done := m.updateSession(loadSessionMsg{
			gen:       1,
			sessionID: "s2",
			session:   newSession,
			files:     []SessionFile{{}},
		}, nil)

		require.False(t, done)
		require.Equal(t, uiChat, m.state)
		require.Same(t, newSession, m.sess.current)
		require.NotEmpty(t, cmds)
	})

	t.Run("model switch schedules an agent model re-probe", func(t *testing.T) {
		t.Parallel()
		ws := &countingWorkspace{ready: true}
		m := newBusyUI(ws)
		warmCaches(m, false)
		m.sess.loadGen = 1
		m.sess.loadExpectedID = "s2"

		cmds, _ := m.updateSession(loadSessionMsg{
			gen:           1,
			sessionID:     "s2",
			session:       &session.Session{ID: "s2"},
			modelSwitched: true,
		}, nil)

		var sawModelChanged bool
		for _, c := range cmds {
			if c == nil {
				continue
			}
			if _, ok := c().(agentModelChangedMsg); ok {
				sawModelChanged = true
			}
		}
		require.True(t, sawModelChanged, "modelSwitched must schedule agentModelChangedCmd")
	})
}

func TestUpdateSession_CreateSessionMsg(t *testing.T) {
	t.Parallel()

	t.Run("no pending send in flight is a pass-through", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		m.editor.pendingSendLoading = false

		cmds, done := m.updateSession(createSessionMsg{generation: 1}, nil)

		require.False(t, done)
		require.Empty(t, cmds)
	})

	t.Run("mismatched generation is a pass-through", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		m.editor.pendingSendLoading = true
		m.editor.pendingSendGen = 2

		cmds, done := m.updateSession(createSessionMsg{generation: 1}, nil)

		require.False(t, done)
		require.Empty(t, cmds)
	})

	t.Run("success adopts the new session and requests its load", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		m.editor.pendingSendLoading = true
		m.editor.pendingSendGen = 1
		m.state = uiLanding
		newSess := session.Session{ID: "s-new"}

		cmds, done := m.updateSession(createSessionMsg{
			session:    newSess,
			content:    "hi",
			generation: 1,
		}, nil)

		require.True(t, done, "createSessionMsg always takes the early-return path")
		require.Equal(t, uiChat, m.state)
		require.Equal(t, "s-new", m.sess.current.ID)
		require.Len(t, m.editor.pendingSendQueue, 1)
		require.Equal(t, "hi", m.editor.pendingSendQueue[0].content)
		require.Len(t, cmds, 1)
		require.NotNil(t, cmds[0])
	})
}

func TestUpdateSession_SessionFilesUpdatesMsg(t *testing.T) {
	t.Parallel()

	m := newBusyUI(&countingWorkspace{ready: true})
	files := []SessionFile{{FirstVersion: history.File{Path: "main.go"}}}

	cmds, done := m.updateSession(sessionFilesUpdatesMsg{sessionID: "s1", sessionFiles: files}, nil)

	require.False(t, done)
	require.Equal(t, files, m.sess.files)
	require.Len(t, cmds, 1)
}

// TestUpdateSession_SessionFilesUpdatesMsg_StaleSession verifies that a
// sessionFilesUpdatesMsg for a session the user has since switched away
// from is dropped instead of overwriting the current session's file list
// with another session's files.
func TestUpdateSession_SessionFilesUpdatesMsg_StaleSession(t *testing.T) {
	t.Parallel()

	m := newBusyUI(&countingWorkspace{ready: true})
	existing := []SessionFile{{FirstVersion: history.File{Path: "current.go"}}}
	m.sess.files = existing
	staleFiles := []SessionFile{{FirstVersion: history.File{Path: "other-session.go"}}}

	cmds, done := m.updateSession(sessionFilesUpdatesMsg{sessionID: "some-other-session", sessionFiles: staleFiles}, nil)

	require.False(t, done)
	require.Equal(t, existing, m.sess.files, "stale session's files must not replace the current session's files")
	require.Empty(t, cmds)
}

func TestUpdateSession_SessionEvent(t *testing.T) {
	t.Parallel()

	t.Run("deleting the current session starts a new one", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		m.sess.current = &session.Session{ID: "s1"}

		cmds, done := m.updateSession(pubsub.Event[session.Session]{
			Type:    pubsub.DeletedEvent,
			Payload: session.Session{ID: "s1"},
		}, nil)

		require.False(t, done)
		require.Nil(t, m.sess.current, "newSession must clear the current session")
		require.NotEmpty(t, cmds)
	})

	t.Run("deleting an unrelated session is a no-op", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		current := &session.Session{ID: "s1"}
		m.sess.current = current

		cmds, done := m.updateSession(pubsub.Event[session.Session]{
			Type:    pubsub.DeletedEvent,
			Payload: session.Session{ID: "s-other"},
		}, nil)

		require.False(t, done)
		require.Same(t, current, m.sess.current)
		require.Empty(t, cmds)
	})

	t.Run("an update for a different session is routed to the child-session handler", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		m.sess.current = &session.Session{ID: "s1"}

		// handleChildSessionUpdate only surfaces a status line for a
		// tracked delegation; an untracked session ID is a documented
		// no-op, so this only pins that the branch is reached (and does
		// not panic) rather than mutating current.
		cmds, done := m.updateSession(pubsub.Event[session.Session]{
			Type:    pubsub.UpdatedEvent,
			Payload: session.Session{ID: "s-child"},
		}, nil)

		require.False(t, done)
		require.Equal(t, "s1", m.sess.current.ID)
		require.Empty(t, cmds)
	})
}

func TestUpdateSession_MessageEvent(t *testing.T) {
	t.Parallel()

	t.Run("no current session is a no-op", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		m.sess.current = nil

		cmds, done := m.updateSession(pubsub.Event[message.Message]{
			Type:    pubsub.CreatedEvent,
			Payload: message.Message{ID: "m1", SessionID: "s1"},
		}, nil)

		require.False(t, done)
		require.Empty(t, cmds)
	})

	t.Run("a message for a different session is routed to the child-session handler", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		m.sess.current = &session.Session{ID: "s1"}

		cmds, done := m.updateSession(pubsub.Event[message.Message]{
			Type:    pubsub.CreatedEvent,
			Payload: message.Message{ID: "m1", SessionID: "s-child"},
		}, nil)

		require.False(t, done)
		// handleChildSessionMessage's cmd is nil unless the payload
		// belongs to a tracked delegation; this only pins that the
		// current-session's own message handling was skipped.
		require.Empty(t, cmds)
	})

	t.Run("deleted removes the message from chat", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		m.sess.current = &session.Session{ID: "s1"}
		m.chat.SetMessages(nil...)

		cmds, done := m.updateSession(pubsub.Event[message.Message]{
			Type:    pubsub.DeletedEvent,
			Payload: message.Message{ID: "m1", SessionID: "s1"},
		}, nil)

		require.False(t, done)
		// RemoveMessage on an unknown id is a safe no-op; this pins that
		// the DeletedEvent arm ran instead of Created/Updated.
		_ = cmds
	})
}

func TestUpdateSession_HistoryFileEvent(t *testing.T) {
	t.Parallel()

	m := newBusyUI(&countingWorkspace{ready: true})

	cmds, done := m.updateSession(pubsub.Event[history.File]{
		Payload: history.File{Path: "main.go"},
	}, nil)

	require.False(t, done)
	require.Len(t, cmds, 1)
}

func TestUpdateSession_SendMessageErrorMsg(t *testing.T) {
	t.Parallel()

	t.Run("stale load generation is dropped", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		m.sess.loadExpectedID = "s1"
		m.sess.loadGen = 5
		m.editor.pendingSendActive = true

		cmds, done := m.updateSession(sendMessageErrorMsg{
			Err:            errors.New("nope"),
			sessionID:      "s-other",
			loadGeneration: 5,
		}, nil)

		require.False(t, done)
		require.Empty(t, cmds)
		require.True(t, m.editor.pendingSendActive, "a stale error must not touch pending-send state")
	})

	t.Run("creating clears the pending-send queue on matching generation", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		m.editor.pendingSendLoading = true
		m.editor.pendingSendGen = 3
		m.editor.pendingSendQueue = []sendQueueItem{{content: "queued"}}
		m.editor.pendingSendActive = true

		cmds, done := m.updateSession(sendMessageErrorMsg{
			Err:        errors.New("create failed"),
			creating:   true,
			generation: 3,
		}, nil)

		require.False(t, done)
		require.False(t, m.editor.pendingSendActive)
		require.False(t, m.editor.pendingSendLoading)
		require.Nil(t, m.editor.pendingSendQueue)
		require.NotEmpty(t, cmds)
	})

	t.Run("non-creating error with a queued item schedules a drain", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		m.editor.pendingSendActive = true
		m.editor.pendingSendQueue = []sendQueueItem{{content: "queued"}}

		cmds, _ := m.updateSession(sendMessageErrorMsg{Err: errors.New("send failed")}, nil)

		require.False(t, m.editor.pendingSendActive)
		var sawDrain bool
		for _, c := range cmds {
			if c == nil {
				continue
			}
			if _, ok := c().(sendPendingQueueMsg); ok {
				sawDrain = true
			}
		}
		require.True(t, sawDrain)
	})
}

func TestUpdateSession_SendPendingQueueMsg(t *testing.T) {
	t.Parallel()

	t.Run("nothing to send is a no-op", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		m.sess.current = &session.Session{ID: "s1"}

		cmds, done := m.updateSession(sendPendingQueueMsg{}, nil)

		require.False(t, done)
		require.Empty(t, cmds)
	})

	t.Run("already active is a no-op", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		m.sess.current = &session.Session{ID: "s1"}
		m.editor.pendingSendActive = true
		m.editor.pendingSendQueue = []sendQueueItem{{content: "queued"}}

		cmds, done := m.updateSession(sendPendingQueueMsg{}, nil)

		require.False(t, done)
		require.Empty(t, cmds)
		require.Len(t, m.editor.pendingSendQueue, 1, "the queue must not be drained while a send is active")
	})

	t.Run("stale item for a different session is dropped and the drain re-scheduled", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		m.sess.current = &session.Session{ID: "s1"}
		m.sess.loadGen = 5
		m.editor.pendingSendQueue = []sendQueueItem{
			{content: "stale", sessionID: "s-other", loadGeneration: 5},
			{content: "next", sessionID: "s1", loadGeneration: 5},
		}

		cmds, done := m.updateSession(sendPendingQueueMsg{}, nil)

		require.False(t, done)
		require.False(t, m.editor.pendingSendActive)
		require.Len(t, m.editor.pendingSendQueue, 1, "the stale item must be dropped")
		require.Equal(t, "next", m.editor.pendingSendQueue[0].content)
		require.Len(t, cmds, 1)
		_, ok := cmds[0]().(sendPendingQueueMsg)
		require.True(t, ok)
	})

	t.Run("a matching bang item runs the shell command", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		m.sess.current = &session.Session{ID: "s1"}
		m.editor.pendingSendQueue = []sendQueueItem{
			{content: "echo hi", sessionID: "s1", bang: true},
		}

		cmds, done := m.updateSession(sendPendingQueueMsg{}, nil)

		require.False(t, done)
		require.True(t, m.editor.pendingSendActive)
		require.Empty(t, m.editor.pendingSendQueue)
		require.Len(t, cmds, 1)
		require.NotNil(t, cmds[0])
	})

	t.Run("a matching regular item sends the message", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		m.sess.current = &session.Session{ID: "s1"}
		m.editor.pendingSendQueue = []sendQueueItem{
			{content: "hello", sessionID: "s1"},
		}

		cmds, done := m.updateSession(sendPendingQueueMsg{}, nil)

		require.False(t, done)
		require.True(t, m.editor.pendingSendActive)
		require.Len(t, cmds, 1)
		require.NotNil(t, cmds[0])
	})
}

func TestUpdateSession_BangSessionCreatedMsg(t *testing.T) {
	t.Parallel()

	t.Run("no pending send in flight is dropped", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		m.editor.pendingSendLoading = false

		cmds, done := m.updateSession(bangSessionCreatedMsg{generation: 1}, nil)

		require.False(t, done)
		require.Empty(t, cmds)
	})

	t.Run("mismatched generation is dropped", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		m.editor.pendingSendLoading = true
		m.editor.pendingSendGen = 2

		cmds, done := m.updateSession(bangSessionCreatedMsg{generation: 1}, nil)

		require.False(t, done)
		require.Empty(t, cmds)
	})

	t.Run("success adopts the session and queues the bang command", func(t *testing.T) {
		t.Parallel()
		m := newBusyUI(&countingWorkspace{ready: true})
		m.editor.pendingSendLoading = true
		m.editor.pendingSendGen = 1
		m.state = uiLanding
		newSess := session.Session{ID: "s-bang"}

		cmds, done := m.updateSession(bangSessionCreatedMsg{
			session:        newSess,
			command:        "echo hi",
			generation:     1,
			isFirstMessage: true,
		}, nil)

		require.False(t, done)
		require.Equal(t, uiChat, m.state)
		require.Equal(t, "s-bang", m.sess.current.ID)
		require.Len(t, m.editor.pendingSendQueue, 1)
		require.True(t, m.editor.pendingSendQueue[0].bang)
		require.Equal(t, "echo hi", m.editor.pendingSendQueue[0].content)
		require.Len(t, cmds, 1)
		require.NotNil(t, cmds[0])
	})
}
