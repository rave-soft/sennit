package model

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// collectInfoMsgs walks a (possibly batched) cmd and gathers every
// util.InfoMsg it produces. Unlike runCmds it does not feed messages
// back through Update: the assertion here is about what the handler
// emits, not about how the model reacts to it.
func collectInfoMsgs(cmd tea.Cmd, out *[]util.InfoMsg) {
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			collectInfoMsgs(c, out)
		}
	case util.InfoMsg:
		*out = append(*out, msg)
	}
}

// TestAgentErrorNotificationReportsInApp pins the in-app half of how a
// failed turn reaches the user. The desktop notification the handler
// also sends is not enough on its own: sendNotification suppresses it
// whenever the terminal window is focused, which is precisely when
// someone is watching the UI.
//
// This matters most for a failure raised before streaming began
// (provider readiness, model resolution): no assistant message exists
// to carry a FinishReasonError, so without this report the only visible
// sign of the failure is the busy indicator switching off.
//
// It also matters for local mode specifically. Runtime errors used to
// return from Workspace.AgentRun and surface through sendMessageErrorMsg;
// once AgentRun started returning at accept time they arrive here
// instead, so this is the path that has to carry them.
func TestAgentErrorNotificationReportsInApp(t *testing.T) {
	pinTTLs(t)

	ws := &countingWorkspace{ready: true}
	m := newBusyUI(ws)
	warmCaches(m, true)

	_, cmd := m.Update(pubsub.Event[workspace.AgentNotification]{
		Type: pubsub.CreatedEvent,
		Payload: workspace.AgentNotification{
			Type:      workspace.AgentNotificationError,
			SessionID: "s1",
			Message:   "provider request failed",
		},
	})
	require.NotNil(t, cmd)

	var infos []util.InfoMsg
	collectInfoMsgs(cmd, &infos)

	var reported []string
	for _, info := range infos {
		if info.Type == util.InfoTypeError {
			reported = append(reported, info.Msg)
		}
	}
	require.Contains(t, reported, "provider request failed",
		"a failed turn must be reported in-app, not only through the focus-gated desktop notification")
}

// TestAgentFinishedNotificationReportsNothing guards the other side of
// the edge: a turn that ended normally must stay quiet in-app. Without
// this, widening the error report to both terminal notification types
// would go unnoticed.
func TestAgentFinishedNotificationReportsNothing(t *testing.T) {
	pinTTLs(t)

	ws := &countingWorkspace{ready: true}
	m := newBusyUI(ws)
	warmCaches(m, true)

	_, cmd := m.Update(pubsub.Event[workspace.AgentNotification]{
		Type: pubsub.CreatedEvent,
		Payload: workspace.AgentNotification{
			Type:      workspace.AgentNotificationFinished,
			SessionID: "s1",
		},
	})

	var infos []util.InfoMsg
	collectInfoMsgs(cmd, &infos)
	require.Empty(t, infos, "a normally finished turn must not raise an in-app report")
}
