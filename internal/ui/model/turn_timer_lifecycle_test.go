package model

import (
	"context"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/stretchr/testify/require"
)

// succeedingAgentRunWorkspace answers AgentRun with success unconditionally,
// so a send's cmd resolves to agentRunSubmittedMsg without needing a real
// coordinator.
type succeedingAgentRunWorkspace struct {
	*countingWorkspace
}

func (w *succeedingAgentRunWorkspace) AgentRun(context.Context, string, string, ...message.Attachment) error {
	return nil
}

// TestSendMessageNow_DoesNotResetAnAlreadyRunningTurnsTimer is the
// regression test for finding 4's first half: common.StartTurn used to run
// unconditionally on every dispatch, including a follow-up prompt sent
// while the session's turn was already in flight (AgentRun folds it in
// rather than starting a new one — see attachedThreadWorkspace.AgentRun).
// That reset the elapsed-time clock on every such follow-up even though
// the turn it was timing never restarted.
func TestSendMessageNow_DoesNotResetAnAlreadyRunningTurnsTimer(t *testing.T) {
	const sessionID = "s1"
	t.Cleanup(func() { common.StopTurn(sessionID) })

	ws := &succeedingAgentRunWorkspace{countingWorkspace: &countingWorkspace{
		ready:       true,
		sessionBusy: map[string]bool{sessionID: true},
	}}
	m := newBusyUI(ws)
	warmCaches(m, true)

	common.StartTurn(sessionID)
	time.Sleep(1100 * time.Millisecond)
	before := common.Elapsed(sessionID)
	require.NotEqual(t, "", before)
	require.NotEqual(t, "0s", before, "precondition: the turn has visibly been running for a while")

	cmd := m.sendMessageNow("a follow-up while the turn is running")
	require.NotNil(t, cmd)
	_ = cmd()

	after := common.Elapsed(sessionID)
	require.NotEqual(t, "0s", after, "a fold-in send must not reset the timer of the turn it is folding into")
}

// TestSendMessageNow_StartsTheTimerForAGenuinelyNewTurn is the mirror
// check: a session that is not already busy must still get its clock
// started, so the fix above does not also suppress the ordinary case.
func TestSendMessageNow_StartsTheTimerForAGenuinelyNewTurn(t *testing.T) {
	const sessionID = "s1"
	t.Cleanup(func() { common.StopTurn(sessionID) })

	ws := &succeedingAgentRunWorkspace{countingWorkspace: &countingWorkspace{ready: true}}
	m := newBusyUI(ws)
	warmCaches(m, false)

	require.Equal(t, "", common.Elapsed(sessionID), "precondition: no turn running yet")

	cmd := m.sendMessageNow("hello")
	require.NotNil(t, cmd)
	_ = cmd()

	require.NotEqual(t, "", common.Elapsed(sessionID), "a genuinely new turn must start the clock")
}

// TestConfirmAgentCancellation_StopsTheTurnTimer is the regression test for
// finding 4's cancellation half: a cancelled turn publishes neither
// AgentNotificationFinished nor AgentNotificationError (see
// handleAgentNotification), so before this fix nothing ever called
// StopTurn for it — the timer table entry outlived the turn forever.
func TestConfirmAgentCancellation_StopsTheTurnTimer(t *testing.T) {
	const sessionID = "s1"
	t.Cleanup(func() { common.StopTurn(sessionID) })

	ws := &countingWorkspace{ready: true}
	m := newBusyUI(ws)
	warmCaches(m, true)

	common.StartTurn(sessionID)
	require.NotEqual(t, "", common.Elapsed(sessionID))

	cmd := m.confirmAgentCancellation()
	_ = cmd

	require.Equal(t, "", common.Elapsed(sessionID), "cancelling a turn must stop its timer")
}
