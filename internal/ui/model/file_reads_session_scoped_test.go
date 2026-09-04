package model

import (
	"testing"

	"github.com/rave-soft/sennit/internal/session"
	"github.com/stretchr/testify/require"
)

// TestUpdateSession_LoadSessionMsg_ClearsFileReadsFromThePreviousSession is
// the regression test for finding 6: sess.fileReads carries no session ID
// of its own (see its field doc in session.go) and is written
// unconditionally whenever a file is opened by @ (ui.go's
// fileAddedToContextMsg handler). Only newSession ever cleared it —
// applyLoadSession did not — so a file read in session A stayed marked
// "read" after switching to session B: @ in B then skipped attaching its
// content, and checkFileFreshness reported the file as already read in a
// session that never touched it, letting the agent edit a file it was
// never shown there.
func TestUpdateSession_LoadSessionMsg_ClearsFileReadsFromThePreviousSession(t *testing.T) {
	t.Parallel()

	ws := &countingWorkspace{ready: true}
	m := newBusyUI(ws)
	warmCaches(m, false)
	m.sess.fileReads = []string{"/repo/a.go"}
	m.sess.loadGen = 1
	m.sess.loadExpectedID = "s2"
	m.state = uiLanding

	cmds, done := m.updateSession(loadSessionMsg{
		gen:       1,
		sessionID: "s2",
		session:   &session.Session{ID: "s2"},
	}, nil)

	require.False(t, done)
	_ = cmds
	require.Empty(t, m.sess.fileReads, "loading a different session must not carry over the previous session's read files")
}
