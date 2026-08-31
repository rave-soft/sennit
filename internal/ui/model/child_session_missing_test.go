package model

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/ui/util"
)

// newChildNavUI is newTestUI plus the dialog overlay Update reaches for
// on every message.
func newChildNavUI() *UI {
	u := newTestUI()
	u.dialog = dialog.NewOverlay()
	return u
}

// enterMissingChild drives the exact sequence a person produces by
// opening a delegation that has not started yet: the nav frame is pushed
// and focus moves off the editor, then the asynchronous load comes back
// not-found.
func enterMissingChild(t *testing.T, u *UI, childID string) []util.InfoMsg {
	t.Helper()

	u.sess.navStack = append(u.sess.navStack, sessionNavFrame{
		parentSessionID: "parent",
		childSessionID:  childID,
	})
	u.focus = uiFocusMain
	u.sess.loadGen++
	u.sess.loadExpectedID = childID

	_, cmd := u.Update(loadSessionMsg{
		uiOwned:   uiOwned{owner: u},
		gen:       u.sess.loadGen,
		sessionID: childID,
		err:       fmt.Errorf("%w: %q", session.ErrNotFound, childID),
	})

	var infos []util.InfoMsg
	collectInfoMsgs(cmd, &infos)
	return infos
}

// TestOpeningAnUnstartedDelegationDoesNotStrandTheUI is the regression
// for what the logs showed: a delegation opened during the window
// between the model emitting the tool call and runSubAgent writing the
// sub-session row. enterChildSession commits to the child view before
// the load resolves, so a not-found load used to leave the person inside
// a child session that does not exist — editor blurred and replaced by
// the delegation panel, chat read-only — with alt+up the only way out.
func TestOpeningAnUnstartedDelegationDoesNotStrandTheUI(t *testing.T) {
	t.Parallel()

	u := newChildNavUI()
	enterMissingChild(t, u, "parent$$call_notyet")

	require.False(t, u.viewingChildSession(),
		"a child session that could not be loaded must not be left on the nav stack")
	require.Equal(t, uiFocusEditor, u.focus,
		"focus must return to the editor once the child view is abandoned")
}

// TestOpeningAnUnstartedDelegationExplainsItself pins the wording. The
// raw error is a database identity — session: no such session:
// "<uuid>$$call_..." — which tells the person nothing about what to do,
// and what to do is wait a moment and try again.
func TestOpeningAnUnstartedDelegationExplainsItself(t *testing.T) {
	t.Parallel()

	u := newChildNavUI()
	infos := enterMissingChild(t, u, "parent$$call_notyet")

	require.Len(t, infos, 1, "exactly one status message")
	require.Equal(t, util.InfoTypeWarn, infos[0].Type,
		"a delegation that has not begun is not an error")
	require.Equal(t, "This delegation has not started yet", infos[0].Msg)
	require.NotContains(t, infos[0].Msg, "$$",
		"the synthetic session id must not reach the status bar")
}

// TestChildSessionLoadFailureStillReportsRealErrors guards the other
// half: only not-found is softened. A load that failed for any other
// reason still rolls the entry back, but says what went wrong.
func TestChildSessionLoadFailureStillReportsRealErrors(t *testing.T) {
	t.Parallel()

	u := newChildNavUI()
	childID := "parent$$call_broken"
	u.sess.navStack = append(u.sess.navStack, sessionNavFrame{
		parentSessionID: "parent",
		childSessionID:  childID,
	})
	u.focus = uiFocusMain
	u.sess.loadGen++
	u.sess.loadExpectedID = childID

	_, cmd := u.Update(loadSessionMsg{
		uiOwned:   uiOwned{owner: u},
		gen:       u.sess.loadGen,
		sessionID: childID,
		err:       fmt.Errorf("database is locked"),
	})

	var infos []util.InfoMsg
	collectInfoMsgs(cmd, &infos)

	require.False(t, u.viewingChildSession(), "a failed entry rolls back whatever the cause")
	require.Len(t, infos, 1)
	require.Equal(t, util.InfoTypeError, infos[0].Type)
	require.Contains(t, infos[0].Msg, "database is locked")
}

// TestFailedChildLoadClearsLoadExpectedID is the regression test for a
// stuck queue: after a not-found load rolls back the nav frame,
// loadExpectedID used to stay pointed at the child that just failed to
// load, even though m.sess.current is (and remains) the parent. Every
// later sendMessage then saw loadExpectedID != current.ID and treated the
// prompt as "still waiting on a delegation load", queuing it for a
// session that will never answer instead of running it — with no error
// surfaced to the person typing.
func TestFailedChildLoadClearsLoadExpectedID(t *testing.T) {
	t.Parallel()

	u := newChildNavUI()
	u.sess.current = &session.Session{ID: "parent"}
	childID := "parent$$call_notyet"
	enterMissingChild(t, u, childID)

	require.Empty(t, u.sess.loadExpectedID,
		"a failed load must not leave loadExpectedID pointed at a session that no longer exists")

	cmd := u.sendMessage("hello")
	require.NotNil(t, cmd, "the prompt must run against the parent, not queue for the missing child")
	require.Empty(t, u.editor.pendingSend.queue,
		"the prompt must not be queued for a session that will never resolve")
}

// TestAbandonOnlyPopsTheFrameItWasAskedAbout pins that a late failure
// from a session the person has already navigated away from cannot pop
// somebody else's frame.
func TestAbandonOnlyPopsTheFrameItWasAskedAbout(t *testing.T) {
	t.Parallel()

	u := newChildNavUI()
	u.sess.navStack = append(u.sess.navStack, sessionNavFrame{childSessionID: "parent$$call_current"})

	require.False(t, u.abandonChildSessionEntry("parent$$call_other"))
	require.True(t, u.viewingChildSession(), "the current frame must survive")
}
