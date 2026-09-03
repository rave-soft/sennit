package model

import (
	"context"
	"testing"

	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/stretchr/testify/require"
)

// newPinTestUI is newCmdDrivenGoldenUI without the golden-render setup:
// these tests assert on calls made during a load, not on what it draws.
func newPinTestUI(ws *cmdDrivingWorkspace) *UI {
	if ws.sessionsBySessionID == nil {
		ws.sessionsBySessionID = map[string]session.Session{
			"s1":       {ID: "s1", Title: "Top"},
			"s-loaded": {ID: "s-loaded", Title: "Loaded"},
			"s-parent": {ID: "s-parent", Title: "Parent"},
			"s-child":  {ID: "s-child", Title: "Child"},
		}
	}
	m := New(common.DefaultCommon(context.Background(), ws), "", false)
	m.state = uiChat
	m.focus = uiFocusEditor
	m.lay.width = 140
	m.lay.height = 45
	m.sess.current = &session.Session{ID: "s1"}
	warmCmdDrivenCaches(m)
	return m
}

// Loading a top-level session restores the model it was pinned to: this is
// a session the user can go on to type into, so the model shown and the
// model the next turn runs on have to be the same one.
func TestSessionLoad_TopLevelRestoresThePinnedModel(t *testing.T) {
	ws := &cmdDrivingWorkspace{agentReady: true}
	m := newPinTestUI(ws)

	_, cmd := m.Update(requestSessionLoad{sessionID: "s-loaded"})
	runCmdTree(m, cmd, nil)

	require.Equal(t, 1, ws.applySessionModelCalls,
		"a session the user can resume must be put back on its own model")
}

// Drilling into a sub-agent loads its transcript through the same path, but
// that transcript is read-only: there is no next turn to line a model up
// with. Restoring the pin there would take the user's own model away —
// a sub-agent commonly runs on a different one — and rebuild the
// coordinator underneath work still in flight, because they looked at
// something.
func TestSessionLoad_ChildSessionLeavesTheModelAlone(t *testing.T) {
	ws := &cmdDrivingWorkspace{agentReady: true}
	m := newPinTestUI(ws)

	// Drilled in: enterChildSession pushes the frame before asking for the
	// load, so this is the state the load happens in.
	m.sess.navStack = append(m.sess.navStack, sessionNavFrame{
		parentSessionID: "s1",
		label:           "subagent",
	})

	_, cmd := m.Update(requestSessionLoad{sessionID: "s-child"})
	runCmdTree(m, cmd, nil)

	require.Zero(t, ws.applySessionModelCalls,
		"inspecting a sub-agent's transcript must not switch the instance's model")
}

// Coming back out of a sub-agent restores the pin again: exitChildSession
// pops the frame before asking for the load, so the session being loaded
// is once more one the user can type into.
func TestSessionLoad_LeavingAChildSessionRestoresThePinAgain(t *testing.T) {
	ws := &cmdDrivingWorkspace{agentReady: true}
	m := newPinTestUI(ws)

	m.sess.navStack = append(m.sess.navStack, sessionNavFrame{
		parentSessionID: "s-parent",
		label:           "subagent",
	})
	cmd := m.exitChildSession()
	require.False(t, m.sess.viewingChildSession())
	runCmdTree(m, cmd, nil)

	require.Equal(t, 1, ws.applySessionModelCalls,
		"back on a resumable session, its own model applies again")
}
