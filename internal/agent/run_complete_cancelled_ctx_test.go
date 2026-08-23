package agent

import (
	"context"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// TestRunTurn_CancelledCtxStillPublishesTerminalEvent is the regression
// test for the streaming defer at the end of runTurn publishing on the
// run's own ctx instead of a detached one. Workspace shutdown can cancel
// ctx before that defer runs (the flush above it already accounts for
// this), but the final reporter.publish call used to still pass the
// cancelled ctx straight through. PublishMustDeliver's blocking fallback
// selects on ctx.Done() alongside the send, so an already-cancelled ctx
// makes it drop the event instead of delivering it to a subscriber that
// has not yet reached its receive - exactly the shape a `sennit run`
// caller blocking on this RunID would hit.
//
// The broker is built with a zero-size subscriber buffer so the
// non-blocking first attempt in sendMustDeliver never succeeds by luck:
// delivery only happens through the (ctx-sensitive) blocking fallback,
// which is the code path this test exists to pin down.
func TestRunTurn_CancelledCtxStillPublishesTerminalEvent(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	broker := pubsub.NewBrokerWithOptions[notify.RunComplete](0)
	t.Cleanup(broker.Shutdown)
	// Widen the must-deliver timeout well past this test's own delayed
	// receive below (defaultMustDeliverTimeout is only 50ms - too tight
	// a window to reliably let the receiving goroutine get scheduled).
	broker.SetMustDeliverTimeout(2 * time.Second)

	sa := NewSessionAgent(SessionAgentOptions{
		Sessions:    env.sessions,
		Messages:    env.messages,
		RunComplete: broker,
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	subCtx, subCancel := context.WithCancel(t.Context())
	defer subCancel()
	ch := broker.Subscribe(subCtx)

	// A ctx that is already cancelled by the time runTurn's fallible
	// setup (session.Get et al.) observes it - mirroring the real
	// scenario, where workspace shutdown cancels ctx before the
	// deferred publisher runs.
	runCtx, runCancel := context.WithCancel(context.Background())
	runCancel()

	// The unbuffered channel means the blocking fallback in
	// sendMustDeliver only completes once this test's own receive below
	// rendezvous with it, so Run must be driven from a goroutine.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = sa.Run(runCtx, SessionAgentCall{
			SessionID: sess.ID,
			RunID:     "run-cancelled-ctx",
			Prompt:    "the user's actual prompt",
		})
	}()

	// Delay this test's own receive so it is not already parked on ch
	// when the publish's non-blocking send attempt runs (a receiver
	// parked on an unbuffered channel makes even a "non-blocking" send
	// succeed instantly, which would mask the bug: it only shows up in
	// sendMustDeliver's ctx-sensitive *blocking* fallback). Against the
	// cancelled ctx bug, the deferred publisher gives up well within
	// this window and this receive then times out with nothing ever
	// sent; against the fix, the blocking fallback is still waiting and
	// rendezvous with this receive as soon as it arrives.
	time.Sleep(200 * time.Millisecond)

	select {
	case ev := <-ch:
		require.Equal(t, "run-cancelled-ctx", ev.Payload.RunID,
			"the terminal event must be attributed to this turn's own RunID")
		require.True(t, ev.Payload.Cancelled,
			"a turn observed against an already-cancelled ctx must report itself cancelled")
	case <-time.After(5 * time.Second):
		t.Fatal("no terminal RunComplete was published for a turn whose ctx was already " +
			"cancelled when the deferred publisher ran - it hung the subscriber instead")
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its terminal event was delivered")
	}
}
