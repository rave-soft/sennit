package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestWakeStopsAfterRepeatedContinuationFailures pins the loop breaker. A
// continuation that fails before its PrepareStep drains anything leaves
// the inbox untouched and the session idle, and run()'s own exit hook
// wakes another one immediately — a session deleted while a delegation was
// still running turned that into a hot loop of failed turns, database
// writes and error logs for as long as the process lived.
func TestWakeStopsAfterRepeatedContinuationFailures(t *testing.T) {
	d := newDispatcher()
	d.SetLiveSession("s1")

	d.enqueueCompletion("s1", TaskCompletion{DelegationID: "d1"})
	require.True(t, d.wakeEligible("s1"), "a queued completion on an idle session wakes it")

	boom := errors.New("boom")
	for i := 1; i < maxContinuationAttempts; i++ {
		require.Equal(t, i, d.noteContinuationOutcome("s1", boom))
		require.True(t, d.wakeEligible("s1"), "attempt %d is still within the budget", i)
	}

	require.Equal(t, maxContinuationAttempts, d.noteContinuationOutcome("s1", boom))
	require.False(t, d.wakeEligible("s1"), "the wake path must give up rather than spin")

	// The events themselves are not lost: they stay queued for whatever
	// real turn runs next.
	require.Len(t, d.drainCompletionsForStep("s1"), 1)
}

// TestANewCompletionGivesTheWakePathItsAttemptsBack keeps the cap from
// wedging a session for good after a transient failure: a completion
// arriving is an external event, not another go at the one that failed.
func TestANewCompletionGivesTheWakePathItsAttemptsBack(t *testing.T) {
	d := newDispatcher()
	d.SetLiveSession("s1")
	boom := errors.New("boom")

	d.enqueueCompletion("s1", TaskCompletion{DelegationID: "d1"})
	for range maxContinuationAttempts {
		d.noteContinuationOutcome("s1", boom)
	}
	require.False(t, d.wakeEligible("s1"))

	d.enqueueCompletion("s1", TaskCompletion{DelegationID: "d2"})
	require.True(t, d.wakeEligible("s1"), "a new delegation's completion must be able to wake the session")
}

// TestASucceedingContinuationClearsTheFailureCount covers the ordinary
// recovery: one good turn and the budget is whole again.
//
// The queued completion is what keeps the dispatch state alive across the
// calls — a state with nothing pending is removed as soon as the last
// reference goes, and the count goes with it. That is the right
// semantics, and it is also the loop's own shape: a continuation that
// fails without draining leaves the inbox exactly this full.
func TestASucceedingContinuationClearsTheFailureCount(t *testing.T) {
	d := newDispatcher()
	d.enqueueCompletion("s1", TaskCompletion{DelegationID: "d1"})

	d.noteContinuationOutcome("s1", errors.New("boom"))
	require.Equal(t, 1, d.continuationFailureCount("s1"))
	require.Equal(t, 0, d.noteContinuationOutcome("s1", nil))
	require.Zero(t, d.continuationFailureCount("s1"))
}

// TestContinuationBackoffIsZeroOnTheFirstAttempt keeps the common path
// immediate: a finished delegation has to reach the agent at once, and
// only a retry waits.
func TestContinuationBackoffIsZeroOnTheFirstAttempt(t *testing.T) {
	require.Zero(t, continuationRetryBackoff(0))
	require.Positive(t, continuationRetryBackoff(1))
	require.Greater(t, continuationRetryBackoff(2), continuationRetryBackoff(1))
}

func TestContinuationBackoffStopsWhenCoordinatorLifecycleCloses(t *testing.T) {
	lifecycle := &readinessLifecycle{}
	a := &sessionAgent{dispatcher: newDispatcher(), continuationContext: lifecycle.context}
	a.enqueueCompletion("s1", TaskCompletion{DelegationID: "d1"})
	a.noteContinuationOutcome("s1", errors.New("previous failure"))

	started := make(chan struct{}, 1)
	a.continuationRunner = func(context.Context, string) error {
		started <- struct{}{}
		return nil
	}
	a.startContinuation(context.Background(), "s1", "test")

	deadline, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, lifecycle.close(deadline))
	select {
	case <-started:
		t.Fatal("continuation runner started after coordinator close")
	case <-time.After(continuationRetryBackoff(1) + 100*time.Millisecond):
	}
}

// TestDropCompletionsClearsAnUndeliverableInbox covers the deleted-session
// case the loop breaker exists for: nothing will ever drain these.
func TestDropCompletionsClearsAnUndeliverableInbox(t *testing.T) {
	d := newDispatcher()
	d.enqueueCompletion("s1", TaskCompletion{DelegationID: "d1"})

	d.dropCompletions("s1")

	require.Empty(t, d.drainCompletionsForStep("s1"))
	require.False(t, d.wakeEligible("s1"))
}
