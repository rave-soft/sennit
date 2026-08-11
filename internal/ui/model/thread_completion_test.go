package model

import (
	"testing"

	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/ui/dialog"
	"github.com/rave-soft/braid/internal/ui/util"
	"github.com/stretchr/testify/require"
)

// TestNotifyThreadCompletion_TerminalTransitionToasts covers the mechanism
// decision for §3: since this codebase has no way to inject a
// non-model, system-authored entry into a session's *persisted* chat
// transcript, thread completion is reported as a toast
// (util.ReportInfo/ReportWarn) instead. A thread's first-ever sighting
// already being terminal (e.g. this UI attaching after the fact) must NOT
// toast — only a transition observed live.
func TestNotifyThreadCompletion_TerminalTransitionToasts(t *testing.T) {
	t.Parallel()

	u := sessionUI()

	// First sighting, already running: no prior status recorded, and
	// "running" isn't terminal anyway — no toast either way.
	require.Nil(t, u.notifyThreadCompletion(proto.Thread{ID: "t1", Name: "fix-auth", Status: "running"}))

	// Transition running -> merged: must toast, info-styled.
	cmd := u.notifyThreadCompletion(proto.Thread{ID: "t1", Name: "fix-auth", Status: "merged", CreatedAt: 1000, CompletedAt: 1720})
	require.NotNil(t, cmd)
	msg, ok := cmd().(util.InfoMsg)
	require.True(t, ok)
	require.Equal(t, util.InfoTypeInfo, msg.Type)
	require.Contains(t, msg.Msg, "fix-auth")
	require.Contains(t, msg.Msg, "merged")
	require.Contains(t, msg.Msg, "12m", "must include the elapsed time from CreatedAt/CompletedAt")

	// A repeated event for the same (already-reported) status must not
	// re-fire.
	require.Nil(t, u.notifyThreadCompletion(proto.Thread{ID: "t1", Name: "fix-auth", Status: "merged"}))
}

// TestNotifyThreadCompletion_NonTerminalTransitionDoesNotToast is the
// direct regression guard for "must not re-fire for a non-terminal status
// change (e.g. pending -> running)".
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
func TestUpdate_ThreadEvent_TogglesThreadCompletionToast(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.dialog = dialog.NewOverlay()

	_, _ = u.Update(pubsub.Event[proto.Thread]{Type: pubsub.UpdatedEvent, Payload: proto.Thread{ID: "t1", Name: "fix-auth", Status: "running"}})
	_, cmd := u.Update(pubsub.Event[proto.Thread]{Type: pubsub.UpdatedEvent, Payload: proto.Thread{ID: "t1", Name: "fix-auth", Status: "merged"}})
	require.NotNil(t, cmd)
}
