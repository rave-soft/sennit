package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent"
	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/stretchr/testify/require"
)

// stubDispatchCoordinator is a minimal agent.Coordinator whose
// RunAccepted returns a configurable error (optionally after marking
// the run-complete marker). Every other method returns a zero value;
// BeginAccepted returns nil since AgentDispatcher only needs it to be
// forwarded to RunAccepted and closed, both of which are nil-safe on
// *agent.AcceptedRun.
type stubDispatchCoordinator struct {
	err           error
	markPublished bool
	runCount      atomic.Int32
	entered       chan struct{}
	release       chan struct{}
}

func (c *stubDispatchCoordinator) Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	return nil, nil
}

func (c *stubDispatchCoordinator) Steer(ctx context.Context, call agent.SessionAgentCall) (agent.SteerOutcome, *fantasy.AgentResult, error) {
	return agent.SteerRan, nil, nil
}

func (c *stubDispatchCoordinator) RunAccepted(ctx context.Context, accept *agent.AcceptedRun, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	c.runCount.Add(1)
	if c.entered != nil {
		close(c.entered)
	}
	if c.release != nil {
		<-c.release
	}
	if c.markPublished {
		agent.MarkRunCompletePublished(ctx)
	}
	return nil, c.err
}

func (c *stubDispatchCoordinator) BeginAccepted(sessionID string) *agent.AcceptedRun { return nil }
func (c *stubDispatchCoordinator) Cancel(string)                                     {}
func (c *stubDispatchCoordinator) CancelAll()                                        {}
func (c *stubDispatchCoordinator) IsBusy() bool                                      { return false }

func (c *stubDispatchCoordinator) IsSessionBusy(string) bool                     { return false }
func (c *stubDispatchCoordinator) QueuedPrompts(string) int                      { return 0 }
func (c *stubDispatchCoordinator) QueuedPromptsList(string) []string             { return nil }
func (c *stubDispatchCoordinator) ClearQueue(string)                             {}
func (c *stubDispatchCoordinator) Summarize(context.Context, string) error       { return nil }
func (c *stubDispatchCoordinator) Model() agent.Model                            { return agent.Model{} }
func (c *stubDispatchCoordinator) UpdateModels(context.Context) error            { return nil }
func (c *stubDispatchCoordinator) GenerateTitle(context.Context, string, string) {}
func (c *stubDispatchCoordinator) SetThreads(tools.ThreadManager)                {}
func (c *stubDispatchCoordinator) SetTasks(tools.TaskManager)                    {}
func (c *stubDispatchCoordinator) DeliverTaskCompletion(context.Context, string, agent.TaskCompletion) {
}
func (c *stubDispatchCoordinator) RefreshSkills([]*skills.Skill, []*skills.Skill)          {}
func (c *stubDispatchCoordinator) RegisterDelegationParent(string, agent.DelegationParent) {}
func (c *stubDispatchCoordinator) SendToParent(context.Context, string, string) error      { return nil }

// TestAgentDispatcher_SendRefusedAfterMarkClosing asserts that once
// MarkClosing has been called, Send refuses synchronously with
// ErrDispatcherClosed and never dispatches a run.
func TestAgentDispatcher_SendRefusedAfterMarkClosing(t *testing.T) {
	t.Parallel()

	coord := &stubDispatchCoordinator{}
	d := NewAgentDispatcher(t.Context(), func() agent.Coordinator { return coord }, pubsub.NewBroker[notify.Notification](), pubsub.NewBroker[notify.RunComplete]())
	d.MarkClosing()

	err := d.Send("S1", "run-1", "hi", nil)
	require.ErrorIs(t, err, ErrDispatcherClosed)

	// Give any accidental dispatch a chance to run, then confirm it
	// never did.
	d.Wait()
	require.Equal(t, int32(0), coord.runCount.Load())
}

// TestAgentDispatcher_SendDispatchesAndWaits asserts the happy path
// returns nil synchronously, dispatches the run on its own goroutine,
// and that Wait blocks until it returns.
func TestAgentDispatcher_SendDispatchesAndWaits(t *testing.T) {
	t.Parallel()

	coord := &stubDispatchCoordinator{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	d := NewAgentDispatcher(t.Context(), func() agent.Coordinator { return coord }, pubsub.NewBroker[notify.Notification](), pubsub.NewBroker[notify.RunComplete]())

	err := d.Send("S1", "run-1", "hi", nil)
	require.NoError(t, err)

	select {
	case <-coord.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatched goroutine never entered RunAccepted")
	}

	waited := make(chan struct{})
	go func() {
		d.Wait()
		close(waited)
	}()

	select {
	case <-waited:
		t.Fatal("Wait completed while the run was still in flight")
	case <-time.After(100 * time.Millisecond):
	}

	close(coord.release)

	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not complete after the run returned")
	}
	require.Equal(t, int32(1), coord.runCount.Load())
}

// TestAgentDispatcher_TerminalFallbackOnPreRunError proves that an
// error returned from RunAccepted before the coordinator could publish
// its own terminal event still yields a reliable terminal RunComplete
// for the run's RunID, mirroring the guarantee runAgent used to provide
// directly.
func TestAgentDispatcher_TerminalFallbackOnPreRunError(t *testing.T) {
	t.Parallel()

	runErr := errors.New("update models failed")
	coord := &stubDispatchCoordinator{err: runErr}
	runCompletions := pubsub.NewBroker[notify.RunComplete]()

	d := NewAgentDispatcher(t.Context(), func() agent.Coordinator { return coord }, pubsub.NewBroker[notify.Notification](), runCompletions)

	subCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch := runCompletions.Subscribe(subCtx)

	require.NoError(t, d.Send("S1", "run-1", "hi", nil))

	select {
	case ev := <-ch:
		require.Equal(t, "run-1", ev.Payload.RunID)
		require.Equal(t, "S1", ev.Payload.SessionID)
		require.Equal(t, runErr.Error(), ev.Payload.Error)
	case <-time.After(2 * time.Second):
		t.Fatal("no terminal RunComplete published for a pre-run error; a run waiter would hang")
	}
}

// TestAgentDispatcher_NoFallbackWhenCoordinatorPublished ensures the
// fallback is suppressed when the coordinator already emitted the
// run's authoritative terminal RunComplete.
func TestAgentDispatcher_NoFallbackWhenCoordinatorPublished(t *testing.T) {
	t.Parallel()

	runErr := errors.New("stream failed after publishing terminal event")
	coord := &stubDispatchCoordinator{err: runErr, markPublished: true}
	runCompletions := pubsub.NewBroker[notify.RunComplete]()

	d := NewAgentDispatcher(t.Context(), func() agent.Coordinator { return coord }, pubsub.NewBroker[notify.Notification](), runCompletions)

	subCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch := runCompletions.Subscribe(subCtx)

	require.NoError(t, d.Send("S1", "run-1", "hi", nil))
	d.Wait()

	select {
	case ev := <-ch:
		t.Fatalf("dispatcher published a duplicate terminal RunComplete: %+v", ev.Payload)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestAgentDispatcher_CancellationPublishesNoErrorTerminal verifies
// that a context.Canceled result from RunAccepted produces no errored
// terminal RunComplete: cancellation is sessionAgent.Run's
// responsibility and the dispatcher must not synthesize one.
func TestAgentDispatcher_CancellationPublishesNoErrorTerminal(t *testing.T) {
	t.Parallel()

	coord := &stubDispatchCoordinator{err: context.Canceled}
	runCompletions := pubsub.NewBroker[notify.RunComplete]()

	d := NewAgentDispatcher(t.Context(), func() agent.Coordinator { return coord }, pubsub.NewBroker[notify.Notification](), runCompletions)

	subCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch := runCompletions.Subscribe(subCtx)

	require.NoError(t, d.Send("S1", "run-1", "hi", nil))
	d.Wait()

	select {
	case ev := <-ch:
		t.Fatalf("cancellation must not publish a terminal RunComplete: %+v", ev.Payload)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestAgentDispatcher_SendValidatesCall asserts Send surfaces
// agent.ValidateCall's structural errors synchronously, without
// dispatching a run.
func TestAgentDispatcher_SendValidatesCall(t *testing.T) {
	t.Parallel()

	coord := &stubDispatchCoordinator{}
	d := NewAgentDispatcher(t.Context(), func() agent.Coordinator { return coord }, pubsub.NewBroker[notify.Notification](), pubsub.NewBroker[notify.RunComplete]())

	err := d.Send("S1", "", "", nil)
	require.ErrorIs(t, err, agent.ErrEmptyPrompt)

	err = d.Send("", "", "hi", nil)
	require.ErrorIs(t, err, agent.ErrSessionMissing)

	d.Wait()
	require.Equal(t, int32(0), coord.runCount.Load())
}
