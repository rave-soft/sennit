package model

import (
	"testing"

	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// TestAgentErrorNotification_OtherSessionDoesNotReportInApp is the
// regression test for finding 7: two top-level sessions can be busy at
// once, but handleAgentNotification's in-app util.ReportError used to run
// unconditionally on n.SessionID — a failure in session A landed in the
// status bar while the person was looking at session B, telling them the
// wrong turn had failed. StopTurn and the busy/queue cache invalidation
// are correctly unconditional (n.SessionID is the only thing that says
// which timer/cache to touch); only the in-app report needed the guard.
func TestAgentErrorNotification_OtherSessionDoesNotReportInApp(t *testing.T) {
	pinTTLs(t)

	ws := &countingWorkspace{ready: true}
	m := newBusyUI(ws) // m.sess.current.ID == "s1"
	warmCaches(m, true)

	common.StartTurn("s-other")
	t.Cleanup(func() { common.StopTurn("s-other") })

	_, cmd := m.Update(pubsub.Event[workspace.AgentNotification]{
		Type: pubsub.CreatedEvent,
		Payload: workspace.AgentNotification{
			Type:      workspace.AgentNotificationError,
			SessionID: "s-other",
			Message:   "provider request failed",
		},
	})
	require.NotNil(t, cmd)

	var infos []util.InfoMsg
	collectInfoMsgs(cmd, &infos)
	for _, info := range infos {
		require.NotEqual(t, util.InfoTypeError, info.Type,
			"a failed turn in another session must not report in the status bar of the one on screen")
	}

	require.Equal(t, "", common.Elapsed("s-other"), "StopTurn must still run for the other session's turn")
}
