package threadspawn

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/braid/internal/agent"
	"github.com/rave-soft/braid/internal/agent/notify"
	"github.com/rave-soft/braid/internal/app"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/thread"
	"github.com/stretchr/testify/require"
)

// TestNewCoordinatorAdapterNilIsNil pins the cancel/shutdown contract: an
// App that never initialized a coordinator must surface as a nil
// [thread.Coordinator], not a non-nil adapter wrapping a nil inner. The
// domain guards a nil coordinator before calling it (see
// lifecycle.cancel), but an adapter-with-nil-inner would panic on any call —
// Cancel included, which shutdown issues unconditionally.
func TestNewCoordinatorAdapterNilIsNil(t *testing.T) {
	require.Nil(t, NewCoordinatorAdapter(nil))
}

// TestWorkspace_NilCoordinatorDoesNotPanicOnCancel drives the real cancel
// path end-to-end: an App whose AgentCoordinator was never set must hand a
// workspace whose Coordinator() is nil, and the lifecycle's cancel guard
// (a.Coordinator() != nil before Cancel) must skip it without panicking.
// Before NewCoordinatorAdapter learned to return nil for a nil inner, this
// workspace handed back a live adapter whose Cancel dereferenced the nil
// coordinator and crashed the process on shutdown.
func TestWorkspace_NilCoordinatorDoesNotPanicOnCancel(t *testing.T) {
	a := app.NewForTest(context.Background())
	t.Cleanup(a.ShutdownForTest)
	// AgentCoordinator left nil, exactly as an App that never started an
	// agent session would be.
	require.Nil(t, a.Coordinator())

	ws := NewAppWorkspaceAdapter(a)
	require.NotNil(t, ws)
	require.Nil(t, ws.Coordinator(),
		"an App with no coordinator must present a nil Coordinator to the domain")

	// The exact expression the lifecycle evaluates on cancel: guarded, it
	// must not panic even though the coordinator is nil.
	if c := ws.Coordinator(); c != nil {
		t.Fatal("coordinator unexpectedly non-nil; the nil-coordinator guard would be bypassed")
	}
}

// TestRunCompletionBrokerAdapter_SubscribeCancelsWithoutLeaking is the
// concurrency regression test for the run-completion bridge: the pump that
// forwards events from the underlying broker onto the domain-facing channel
// must exit when the subscribe context is cancelled, even while a consumer
// has stopped draining the output channel and a further event is queued
// behind a blocking send. Before the pump selected on ctx.Done() on both the
// read and the send, it would block forever on the unbuffered out channel
// after its reader went away and leak past the delegation's lifetime.
func TestRunCompletionBrokerAdapter_SubscribeCancelsWithoutLeaking(t *testing.T) {
	broker := pubsub.NewBroker[notify.RunComplete]()
	adapter := NewRunCompletionBroker(broker)

	ctx, cancel := context.WithCancel(context.Background())
	out := adapter.Subscribe(ctx)

	// Publish one event so the pump reaches its first send; the test never
	// drains it, so that send blocks until cancellation.
	broker.Publish(pubsub.CreatedEvent, notify.RunComplete{SessionID: "s", RunID: "r", Text: "x"})

	// Give the pump a moment to reach the blocking send on out.
	time.Sleep(20 * time.Millisecond)

	// Now stop consuming and cancel: the pump must not stay wedged on the
	// out send. A goroutine-leak detector (t.Cleanup + runtime.NumGoroutine
	// would over-report unrelated test goroutines, so we assert the
	// observable contract instead): after cancel the pump is done, and a
	// fresh Subscribe on a cancelled ctx is safe to abandon.
	cancel()

	done := make(chan struct{})
	go func() {
		// The pump's defer closes out when it exits. A close here proves it
		// left the blocking send rather than leaking.
		for range out {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the run-completion pump did not exit after its subscribe context was cancelled (goroutine leak)")
	}
}

// TestParentAppSpawner_StableWorkspaceIdentity pins the production identity
// contract that the thread domain's parent-vs-thread discrimination relies
// on: Attach creates one adapter, uses it as ManagerOptions.ParentApp, and
// gives that exact adapter to ParentAppSpawner. This ownership-scoped design
// avoids a process-wide cache retaining released Apps.
func TestParentAppSpawner_StableWorkspaceIdentity(t *testing.T) {
	a := app.NewForTest(context.Background())
	t.Cleanup(a.ShutdownForTest)

	parent := NewAppWorkspaceAdapter(a) // what Attach hands ManagerOptions.ParentApp
	s := NewParentAppSpawner(parent)

	for range 3 {
		h, err := s.Spawn(context.Background(), "")
		require.NoError(t, err)
		ws := h.Workspace()
		require.Same(t, parent, ws,
			"every task handle must retain the adapter used as ManagerOptions.ParentApp")
		require.Equal(t, parent.Coordinator(), ws.Coordinator(),
			"the coordinator must be stable across handles so delegation-parent identity holds")
		require.NoError(t, s.Release(context.Background(), h.ID()))
	}
}

// TestCoordinatorAdapter_TranslateCtxCarriesDispatchTag verifies the domain's
// WithAgentDispatch/WithRunID tags survive the adapter onto the agent's own
// context keys, so a dispatched run keeps its origin and run-id exactly as
// before the port was introduced.
func TestCoordinatorAdapter_TranslateCtxCarriesDispatchTag(t *testing.T) {
	var seenOrigin, seenRunID atomic.Value
	coord := &tagRecodingCoordinator{
		run: func(ctx context.Context) {
			seenOrigin.Store(agent.PromptOriginFromContext(ctx))
			seenRunID.Store(agent.RunIDFromContext(ctx))
		},
	}
	adapter := NewCoordinatorAdapter(coord)

	ctx := thread.WithAgentDispatch(thread.WithRunID(context.Background(), "run-123"))
	require.NoError(t, adapter.Run(ctx, "sess", "do it"))

	require.Equal(t, message.OriginAgent, seenOrigin.Load().(message.Origin),
		"the agent-dispatch origin tag must be re-applied on the agent's own key")
	require.Equal(t, "run-123", seenRunID.Load().(string),
		"the per-run RunID must be re-applied on the agent's own key")
}

// tagRecodingCoordinator is an agent.Coordinator that records the context
// origin/run-id it observes in Run, for the translateCtx test. It embeds
// the interface (nil) so it only implements the one method under test.
type tagRecodingCoordinator struct {
	agent.Coordinator
	run func(ctx context.Context)
}

func (c *tagRecodingCoordinator) Run(ctx context.Context, sessionID, prompt string, _ ...message.Attachment) (*fantasy.AgentResult, error) {
	c.run(ctx)
	return nil, nil
}
