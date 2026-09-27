package cmd

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/herdr"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/workspace"
)

// withHerdrClientConstructor substitutes herdrClientConstructor for the
// duration of a test and restores it on cleanup -- the "fake constructor
// hook" this package's daemon-mode herdr wiring is built around (see
// herdr_daemon.go's doc comment), used instead of exercising herdr.Init's
// real HERDR_ENV-gated singleton, which only ever answers once per
// process (sync.Once), so a second test wanting the other outcome in the
// same binary could never get it.
func withHerdrClientConstructor(t *testing.T, ctor func() *herdr.Client) {
	t.Helper()
	orig := herdrClientConstructor
	herdrClientConstructor = ctor
	t.Cleanup(func() { herdrClientConstructor = orig })
}

// TestWireHerdr_NoHerdrEnv_StillForwardsEvents covers the "not running
// inside a herdr pane" half of runInteractiveDaemon/attach's setup
// (both funnel through runDaemonTUI's wireHerdr call): a fake
// constructor standing in for herdr.Init's own "HERDR_ENV absent" answer
// (nil) must still leave every event reaching the TUI's own send
// unchanged, and wireHerdr must call the hook rather than deciding for
// itself whether to build one.
func TestWireHerdr_NoHerdrEnv_StillForwardsEvents(t *testing.T) {
	var called int
	withHerdrClientConstructor(t, func() *herdr.Client {
		called++
		return nil
	})

	var got []any
	send := wireHerdr(func(msg any) { got = append(got, msg) })

	ev := pubsub.Event[workspace.AgentNotification]{
		Payload: workspace.AgentNotification{SessionID: "s1", Type: workspace.AgentNotificationFinished},
	}
	send(ev)
	send("untranslatable")

	require.Equal(t, 1, called, "wireHerdr must ask the constructor hook exactly once, when it is built")
	require.Equal(t, []any{ev, "untranslatable"}, got, "every event must still reach the TUI's own send")
}

// TestWireHerdr_HerdrEnvPresent_ConstructsAndDrivesTheRealClient covers
// the other half: with HERDR_ENV naming a real (if unreachable) pane,
// herdrClientConstructor set to the real herdr.Init -- exactly
// production's default -- must hand wireHerdr a non-nil client, and
// feeding it the daemon-mode turn-started/permission/finished sequence
// must not panic or block (the client's sender is best-effort against a
// socket path that does not exist, see herdr.Client.HandleEvent's own
// nil/error-tolerant design), while every event still reaches the TUI's
// own send unchanged.
func TestWireHerdr_HerdrEnvPresent_ConstructsAndDrivesTheRealClient(t *testing.T) {
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_SOCKET_PATH", t.TempDir()+"/does-not-exist.sock")
	t.Setenv("HERDR_PANE_ID", "test-pane")
	withHerdrClientConstructor(t, herdr.Init)

	var got []any
	send := wireHerdr(func(msg any) { got = append(got, msg) })

	events := []any{
		pubsub.Event[workspace.AgentNotification]{
			Payload: workspace.AgentNotification{SessionID: "s1", Type: workspace.AgentNotificationTurnStarted},
		},
		pubsub.Event[message.Message]{Payload: message.Message{Role: message.Assistant, SessionID: "s1"}},
		pubsub.Event[workspace.AgentNotification]{
			Payload: workspace.AgentNotification{SessionID: "s1", Type: workspace.AgentNotificationFinished},
		},
	}
	for _, ev := range events {
		send(ev)
	}

	require.Equal(t, events, got, "every event must still reach the TUI's own send")
}
