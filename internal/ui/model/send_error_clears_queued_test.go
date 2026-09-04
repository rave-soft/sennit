package model

import (
	"context"
	"testing"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/stretchr/testify/require"
)

// alwaysRefusingAgentRunWorkspace mimics a thread attached read-only (a
// merged or still-merging delegation): AgentRun refuses every call, the
// same as attachedThreadWorkspace's read-only fallback.
type alwaysRefusingAgentRunWorkspace struct {
	*countingWorkspace
}

func (w *alwaysRefusingAgentRunWorkspace) AgentRun(context.Context, string, string, ...message.Attachment) error {
	return errAlwaysRefused
}

var errAlwaysRefused = errRefused{}

type errRefused struct{}

func (errRefused) Error() string { return "thread is not running" }

// TestSendMessage_FailedSendClearsItsQueuedPlaceholder is the regression
// test for finding 3: sendMessage draws a "queued" placeholder up front,
// on the optimistic assumption that a busy turn ahead of it will
// eventually take the prompt (see queued.show in send.go). When the send
// fails outright instead — e.g. AgentRun refusing unconditionally on a
// thread attached read-only — nothing ever calls queued.deliver for it,
// so the placeholder used to sit in chat forever labelled "queued" even
// though it is not queued anywhere; the person's own message looked like
// it vanished into a real queue that does not exist.
func TestSendMessage_FailedSendClearsItsQueuedPlaceholder(t *testing.T) {
	ws := &alwaysRefusingAgentRunWorkspace{countingWorkspace: &countingWorkspace{ready: true, agentBusy: true}}
	m := newBusyUI(ws)
	warmCaches(m, true)

	cmd := m.sendMessage("hello while busy")
	require.NotNil(t, cmd)
	require.Len(t, m.queued.items, 1, "precondition: the optimistic placeholder was drawn")

	msg := cmd()
	errMsg, ok := msg.(sendMessageErrorMsg)
	require.True(t, ok, "AgentRun's refusal must surface as sendMessageErrorMsg")

	cmds, done := m.updateSession(errMsg, nil)
	require.False(t, done)
	require.NotEmpty(t, cmds)

	require.Empty(t, m.queued.items, "a failed send must drop the placeholder it drew, not leave it queued forever")
}
