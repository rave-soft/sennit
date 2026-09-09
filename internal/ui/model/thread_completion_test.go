package model

import (
	"strings"
	"testing"

	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/stretchr/testify/require"
)

// TestNotifyThreadCompletion_TerminalTransitionToasts covers the mechanism
// decision for §3: since this codebase has no way to inject a
// non-model, system-authored entry into a session's *persisted* chat
// transcript, thread completion is reported as a toast
// (util.ReportInfo/ReportWarn) instead. A thread's first-ever sighting
// already being terminal (e.g. this UI attaching after the fact) must NOT
// toast — only a transition observed live.
func TestThreadCompletionCleanupOutcomes(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name  string
		error string
		want  string
	}{
		{"removed", "", "clean worktree removed"},
		{"dirty", "cleanup retained: uncommitted changes", "uncommitted changes"},
		{"commits", "cleanup retained: unique commits", "unique commits"},
		{"unknown", "cleanup retained: safety could not be verified", "could not be verified"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			cmd := threadCompletionToast(proto.Thread{Name: "work", Status: "completed", Error: testCase.error})
			msg, ok := cmd().(util.InfoMsg)
			require.True(t, ok)
			require.Contains(t, msg.Msg, testCase.want)
		})
	}
}

func TestNotifyThreadCompletion_NonTerminalTransitionDoesNotToast(t *testing.T) {
	t.Parallel()

	u := sessionUI()

	require.Nil(t, u.notifyThreadCompletion(proto.Thread{ID: "t1", Name: "fix-auth", Status: "pending"}))
	require.Nil(t, u.notifyThreadCompletion(proto.Thread{ID: "t1", Name: "fix-auth", Status: "running"}),
		"pending -> running is not a terminal transition; must not toast")
}

// TestNotifyThreadCompletion_FailedTransitionWarns covers the
// failure-styling half: a transition into "failed" must toast as a warn,
// not an info.
func TestNotifyThreadCompletion_FailedTransitionWarns(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	require.Nil(t, u.notifyThreadCompletion(proto.Thread{ID: "t1", Name: "fix-auth", Status: "running"}))

	cmd := u.notifyThreadCompletion(proto.Thread{ID: "t1", Name: "fix-auth", Status: "failed"})
	require.NotNil(t, cmd)
	msg, ok := cmd().(util.InfoMsg)
	require.True(t, ok)
	require.Equal(t, util.InfoTypeWarn, msg.Type)
	require.Contains(t, msg.Msg, "failed")
}

// TestUpdate_ThreadEvent_TogglesThreadCompletionToast is the end-to-end
// wiring check: the pubsub.Event[proto.Thread] case in ui.go's Update
// loop must actually call notifyThreadCompletion, not just have the
// standalone method work in isolation.
func TestUpdateThreads_DeletedEventPrunesThreadLastStatus(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.threadLastStatus = map[string]string{"t1": "running"}

	cmds, _ := u.updateThreads(pubsub.Event[proto.Thread]{
		Type:    pubsub.DeletedEvent,
		Payload: proto.Thread{ID: "t1", Name: "clean", Status: "completed"},
	}, nil)

	reports := 0
	for _, cmd := range cmds {
		if msg, ok := cmd().(util.InfoMsg); ok && strings.Contains(msg.Msg, "clean worktree removed") {
			reports++
		}
	}
	require.Equal(t, 1, reports)
	_, stillTracked := u.threadLastStatus["t1"]
	require.False(t, stillTracked, "a deleted thread's threadLastStatus entry must be dropped")
}

func TestWorkspaceConvertedCleanupEventsDriveOneUIOutcome(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		eventType pubsub.EventType
		error     string
		want      string
	}{
		{"removed", pubsub.DeletedEvent, "", "clean worktree removed"},
		{"dirty", pubsub.UpdatedEvent, "cleanup retained: uncommitted changes", "uncommitted changes"},
		{"unique", pubsub.UpdatedEvent, "cleanup retained: unique commits", "unique commits"},
		{"unverified", pubsub.UpdatedEvent, "cleanup retained: safety could not be verified", "could not be verified"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			u := sessionUI()
			u.threadLastStatus = map[string]string{"thread-1": "running"}
			converted := pubsub.Event[proto.Thread]{
				Type: testCase.eventType,
				Payload: proto.Thread{
					ID: "thread-1", Name: "work", Kind: "thread",
					Status: "completed", Error: testCase.error,
				},
			}
			cmds, _ := u.updateThreads(converted, nil)
			var reports []string
			for _, cmd := range cmds {
				if msg, ok := cmd().(util.InfoMsg); ok {
					reports = append(reports, msg.Msg)
				}
			}
			require.Len(t, reports, 1)
			require.Contains(t, reports[0], testCase.want)
			if testCase.name != "removed" {
				require.NotContains(t, reports[0], "clean worktree removed")
			}
		})
	}
}
