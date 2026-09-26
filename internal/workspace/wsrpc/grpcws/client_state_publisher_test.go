package grpcws

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/wsrpctest"
)

// TestEventHub_ClientStatePublisher_OnlyPublishesOnChange checks
// maybePublishState's core contract (CLIENT-SERVER.md, PR 1.4a): a real
// change to the workspace's class-C state produces exactly one
// client_state event, with Version bumped by exactly one; calling it
// again with nothing changed produces none. The tick interval is
// effectively infinite here (no ticker fires during the test) so every
// call to maybePublishState below is the one under test, not a race
// against the ticker's own schedule.
func TestEventHub_ClientStatePublisher_OnlyPublishesOnChange(t *testing.T) {
	t.Parallel()

	stub := &wsrpctest.StubWorkspace{}
	h := newEventHubWithStateTick(64, time.Hour)
	sub, _, _ := h.subscribe(0)
	h.ensureStarted(stub)
	t.Cleanup(h.close)

	// The first check ever run has nothing to compare against, so it
	// always "changes" (from no state at all) and publishes Version 1.
	h.maybePublishState()
	select {
	case frame := <-sub.ch:
		require.Equal(t, "client_state", frame.Event.Type)
	case <-time.After(time.Second):
		t.Fatal("expected the initial client_state publish, got nothing")
	}

	// No change: must not publish again.
	h.maybePublishState()
	select {
	case frame := <-sub.ch:
		t.Fatalf("unexpected event published with nothing changed: %+v", frame)
	case <-time.After(50 * time.Millisecond):
	}

	// A real change: exactly one new event.
	stub.AgentIsBusyResult = true
	h.maybePublishState()
	select {
	case frame := <-sub.ch:
		require.Equal(t, "client_state", frame.Event.Type)
	case <-time.After(time.Second):
		t.Fatal("expected a client_state event after AgentIsBusy changed")
	}

	select {
	case frame := <-sub.ch:
		t.Fatalf("unexpected second event published with nothing changed: %+v", frame)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestEventHub_ClientStatePublisher_VersionIncrementsByOnePerChange
// checks the Version sequence directly (not just "an event arrived") by
// reading eventHub.snapshot after each explicit change.
func TestEventHub_ClientStatePublisher_VersionIncrementsByOnePerChange(t *testing.T) {
	t.Parallel()

	stub := &wsrpctest.StubWorkspace{}
	h := newEventHubWithStateTick(64, time.Hour)
	h.ensureStarted(stub)
	t.Cleanup(h.close)

	h.maybePublishState()
	_, state := h.snapshot()
	require.Equal(t, uint64(1), state.Version)

	// No change: Version must not move.
	h.maybePublishState()
	_, state = h.snapshot()
	require.Equal(t, uint64(1), state.Version)

	stub.AgentIsReadyResult = true
	h.maybePublishState()
	_, state = h.snapshot()
	require.Equal(t, uint64(2), state.Version)
	require.True(t, state.AgentIsReady)
}

// TestEventHub_StatePublisher_TicksOnItsOwnSchedule checks the ticker half
// of the publisher (CLIENT-SERVER.md, PR 1.4a: "и по таймеру"), started at
// ensureStarted time with no Subscribe call needed first, and stopped
// cleanly by close (no goroutine leak, checked by the package's own
// goleak-covered tests elsewhere -- this one only checks the tick fires).
func TestEventHub_StatePublisher_TicksOnItsOwnSchedule(t *testing.T) {
	t.Parallel()

	stub := &wsrpctest.StubWorkspace{WorkingDirResult: "/repo"}
	h := newEventHubWithStateTick(64, 5*time.Millisecond)
	h.ensureStarted(stub)
	t.Cleanup(h.close)

	require.Eventually(t, func() bool {
		seq, state := h.snapshot()
		return seq >= 1 && state.Version >= 1
	}, time.Second, time.Millisecond, "the ticker must publish the initial client_state on its own, with no Subscribe call")

	_, state := h.snapshot()
	require.Equal(t, "/repo", state.WorkingDir)
}

// TestEventHub_Snapshot_FallsBackToFreshStateBeforeAnyPublish checks
// eventHub.snapshot's other branch: before this hub's publisher has ever
// run (ensureStarted not yet called, or its very first check not yet
// done), Snapshot still answers from a freshly built state rather than
// blocking or erroring -- with Seq 0 and Version 0, so a client's later
// first real client_state event (Version 1) is never mistaken for a
// regression.
func TestEventHub_Snapshot_FallsBackToFreshStateBeforeAnyPublish(t *testing.T) {
	t.Parallel()

	h := newEventHubWithStateTick(64, time.Hour)

	seq, state := h.snapshot()
	require.Equal(t, uint64(0), seq, "nothing published yet")
	require.Equal(t, uint64(0), state.Version)
	require.Equal(t, "", state.WorkingDir, "no ws set yet: nothing to build from")
}

// TestEventHub_SnapshotThenSubscribe_NoNewerStateLost is the Snapshot/
// subscribe consistency test CLIENT-SERVER.md's PR 1.2/1.4a review point 2
// asks for: an event published between a Snapshot and the client's
// following Subscribe(FromSeq: Snapshot.Seq+1) must still reach it, and a
// replayed client_state event must never be mistaken for a regression
// against what the snapshot already reported (its Version is always
// strictly greater).
func TestEventHub_SnapshotThenSubscribe_NoNewerStateLost(t *testing.T) {
	t.Parallel()

	stub := &wsrpctest.StubWorkspace{}
	h := newEventHubWithStateTick(64, time.Hour)
	h.ensureStarted(stub)
	t.Cleanup(h.close)

	h.maybePublishState()
	seq0, state0 := h.snapshot()
	require.Equal(t, uint64(1), seq0)
	require.Equal(t, uint64(1), state0.Version)

	// Simulate traffic landing strictly after the snapshot was taken: a
	// plain domain event, then a real class-C change.
	h.publish(wsrpctest.SessionEvent)
	stub.AgentIsBusyResult = true
	h.maybePublishState()

	_, replay, resync := h.subscribe(seq0 + 1)
	require.False(t, resync)
	require.Len(t, replay, 2)
	require.Equal(t, "session", replay[0].env.Type)
	require.Equal(t, "client_state", replay[1].env.Type)

	decoded, err := wsrpc.DecodeEvent(replay[1].env)
	require.NoError(t, err)
	replayedState := decoded.(pubsub.Event[workspace.ClientState]).Payload
	require.Greater(t, replayedState.Version, state0.Version,
		"a state event a client receives after its snapshot must never be safe to mistake for a regression")
}
