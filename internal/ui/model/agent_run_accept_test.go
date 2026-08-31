package model

import (
	"context"
	"testing"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/stretchr/testify/require"
)

// acceptTimeAgentRunWorkspace records every AgentRun call and returns
// nil immediately, the way AppWorkspace.AgentRun behaves (dispatch through
// AgentDispatcher.Send): the call returns at accept time, not once the
// turn finishes.
// agentBusy stays whatever the test sets it to, standing in for the
// real turn still being active on the coordinator side after AgentRun
// has already returned.
type acceptTimeAgentRunWorkspace struct {
	*countingWorkspace
	agentRunCalls []string
}

func (w *acceptTimeAgentRunWorkspace) AgentRun(_ context.Context, _ string, prompt string, _ ...message.Attachment) error {
	w.agentRunCalls = append(w.agentRunCalls, prompt)
	return nil
}

// TestSecondSubmitDispatchesAfterAcceptInsteadOfQueueing pins the
// headline behavior the local-mode dispatch change (stage 3) exists to
// deliver, verified here at the UI level: sendMessageNow's
// agentRunSubmittedMsg fires as soon as AgentRun returns, and the
// agentRunSubmittedMsg handler in Update clears
// editor.pendingSendActive and drains pendingSendQueue right there —
// so once that message has been processed, a second submit finds
// pendingSendActive already false and calls sendMessageNow (which
// calls AgentRun) directly instead of appending to pendingSendQueue.
//
// Before stage 3, local mode's AgentRun blocked until the whole LLM
// turn finished, so agentRunSubmittedMsg — and the pendingSendActive
// clear that rides on it — would not have fired until then either: a
// second submit arriving while the first turn was still running would
// have gone into pendingSendQueue instead of reaching AgentRun. This
// test would fail (agentRunCalls would only ever contain "first", and
// pendingSendQueue would hold the second item) if that were still true
// today.
func TestSecondSubmitDispatchesAfterAcceptInsteadOfQueueing(t *testing.T) {
	pinTTLs(t)

	ws := &acceptTimeAgentRunWorkspace{countingWorkspace: &countingWorkspace{ready: true, agentBusy: true}}
	m := newBusyUI(ws)
	warmCaches(m, false)

	cmd := m.sendMessage("first")
	require.NotNil(t, cmd)
	runCmds(m, cmd)

	require.Equal(t, []string{"first"}, ws.agentRunCalls)
	require.False(t, m.editor.pendingSend.active,
		"agentRunSubmittedMsg must clear pendingSendActive once AgentRun returns at accept time")
	require.Empty(t, m.editor.pendingSend.queue)

	// The agent is still busy (ws.agentBusy stays true, standing in for
	// the still-running first turn); a second submit must still reach
	// AgentRun directly rather than sitting behind it in the queue.
	cmd = m.sendMessage("second")
	require.NotNil(t, cmd)
	runCmds(m, cmd)

	require.Equal(t, []string{"first", "second"}, ws.agentRunCalls,
		"second submit must be dispatched to the workspace, not parked in pendingSendQueue")
	require.Empty(t, m.editor.pendingSend.queue)
}
