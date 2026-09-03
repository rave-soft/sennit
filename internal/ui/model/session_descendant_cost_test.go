package model

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/session"
)

// descendantCostWorkspace stubs SessionDescendantCost with a canned value,
// on top of countingWorkspace so the rest of a *UI's construction keeps
// working. calls records every sessionID it was asked about.
type descendantCostWorkspace struct {
	*countingWorkspace

	cost  float64
	calls []string
}

func (w *descendantCostWorkspace) SessionDescendantCost(_ context.Context, sessionID string) (float64, error) {
	w.calls = append(w.calls, sessionID)
	return w.cost, nil
}

// TestApplySessionEvent_DelegationRefreshesDescendantCost covers the broad
// trigger: an event for a session that is not the one on screen, but that
// carries a ParentSessionID (so it is somewhere in a delegation tree),
// still dispatches a descendant-cost refresh for the session on screen.
func TestApplySessionEvent_DelegationRefreshesDescendantCost(t *testing.T) {
	t.Parallel()

	ws := &descendantCostWorkspace{countingWorkspace: &countingWorkspace{ready: true}, cost: 2.5}
	m := newBusyUI(ws)
	m.sess.current = &session.Session{ID: "root"}

	cmds := m.applySessionEvent(pubsub.Event[session.Session]{
		Type:    pubsub.UpdatedEvent,
		Payload: session.Session{ID: "child", ParentSessionID: "root"},
	}, nil)

	require.NotEmpty(t, cmds)
	var got sessionDescendantCostMsg
	var found bool
	for _, cmd := range cmds {
		if cmd == nil {
			continue
		}
		if msg, ok := cmd().(sessionDescendantCostMsg); ok {
			got, found = msg, true
		}
	}
	require.True(t, found, "expected a sessionDescendantCostMsg among the dispatched commands")
	require.Equal(t, "root", got.sessionID)
	require.InDelta(t, 2.5, got.cost, 0.0001)
	require.Equal(t, []string{"root"}, ws.calls, "the refresh must be for the session on screen, not the event's own session")

	// Delivering the message updates state.
	m.sess.descendantCost = 0
	cmds2, done := m.updateSession(got, nil)
	require.False(t, done)
	require.Empty(t, cmds2)
	require.InDelta(t, 2.5, m.sess.descendantCost, 0.0001)
}

// TestApplySessionEvent_UnrelatedSessionDoesNotRefresh checks the negative
// case: an event for a session with no ParentSessionID (a top-level
// session, not a delegation) triggers no descendant-cost refresh.
func TestApplySessionEvent_UnrelatedSessionDoesNotRefresh(t *testing.T) {
	t.Parallel()

	ws := &descendantCostWorkspace{countingWorkspace: &countingWorkspace{ready: true}}
	m := newBusyUI(ws)
	m.sess.current = &session.Session{ID: "root"}

	m.applySessionEvent(pubsub.Event[session.Session]{
		Type:    pubsub.UpdatedEvent,
		Payload: session.Session{ID: "other-root"},
	}, nil)

	require.Empty(t, ws.calls)
}

// TestSessionSwitch_ClearsPreviousDescendantCost drives applyLoadSession
// (via updateSession's loadSessionMsg branch) with a stale descendantCost
// left over from the session being switched away from, and checks it is
// zeroed immediately rather than carried onto the newly current session —
// the real figure follows once the dispatched refresh lands.
func TestSessionSwitch_ClearsPreviousDescendantCost(t *testing.T) {
	t.Parallel()

	ws := &descendantCostWorkspace{countingWorkspace: &countingWorkspace{ready: true}, cost: 9}
	m := newBusyUI(ws)
	warmCaches(m, false)
	m.sess.loadGen = 1
	m.sess.loadExpectedID = "s2"
	m.sess.current = &session.Session{ID: "s1"}
	m.sess.descendantCost = 42

	cmds, done := m.updateSession(loadSessionMsg{
		gen:       1,
		sessionID: "s2",
		session:   &session.Session{ID: "s2"},
	}, nil)

	require.False(t, done)
	require.Zero(t, m.sess.descendantCost, "the previous session's figure must not survive the switch")
	require.NotEmpty(t, cmds)

	var sawRefresh bool
	for _, cmd := range cmds {
		if cmd == nil {
			continue
		}
		if msg, ok := cmd().(sessionDescendantCostMsg); ok {
			sawRefresh = true
			require.Equal(t, "s2", msg.sessionID)
		}
	}
	require.True(t, sawRefresh, "expected the switch to dispatch a descendant-cost refresh for the new session")
	require.Contains(t, ws.calls, "s2")
}

// TestNewSession_ClearsDescendantCost checks that starting a fresh session
// (no current session at all) resets the figure rather than leaving the
// departed session's total on screen with nothing to attribute it to.
func TestNewSession_ClearsDescendantCost(t *testing.T) {
	t.Parallel()

	ws := &descendantCostWorkspace{countingWorkspace: &countingWorkspace{ready: true}}
	m := newBusyUI(ws)
	m.sess.current = &session.Session{ID: "s1"}
	m.sess.descendantCost = 7

	m.newSession()

	require.Nil(t, m.sess.current)
	require.Zero(t, m.sess.descendantCost)
}
