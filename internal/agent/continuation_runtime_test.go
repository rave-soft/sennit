package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestStartContinuationGoesThroughTheCoordinatorWhenWired pins how an
// auto-woken continuation is dispatched. It used to call
// sessionAgent.Run directly with nothing but a session id and a
// placeholder prompt: no Runtime (so no thinking options and no
// output-token budget), no OnAuthRefresh (so an OAuth token that expired
// while the delegation ran produced a 401 with no retry), and no MCP
// wait. A continuation fires long after the turn that started the
// delegation, which is exactly when those matter.
func TestStartContinuationGoesThroughTheCoordinatorWhenWired(t *testing.T) {
	a := &sessionAgent{dispatch: newDispatcher()}

	called := make(chan string, 1)
	a.continuationRunner = func(_ context.Context, sessionID string) error {
		called <- sessionID
		return nil
	}

	a.startContinuation(context.Background(), "s1", "test")

	select {
	case got := <-called:
		require.Equal(t, "s1", got)
	case <-time.After(2 * time.Second):
		t.Fatal("the continuation must be dispatched through the wired runner")
	}
}

// TestStartContinuationRecordsTheRunnersFailure keeps the loop breaker
// wired to the new path: a runner that fails still counts against the
// attempt budget.
func TestStartContinuationRecordsTheRunnersFailure(t *testing.T) {
	a := &sessionAgent{dispatch: newDispatcher()}
	a.enqueueCompletion("s1", TaskCompletion{DelegationID: "d1"})

	failed := make(chan struct{}, 1)
	a.continuationRunner = func(context.Context, string) error {
		failed <- struct{}{}
		return errors.New("boom")
	}

	a.startContinuation(context.Background(), "s1", "test")
	select {
	case <-failed:
	case <-time.After(2 * time.Second):
		t.Fatal("the runner must be called")
	}

	require.Eventually(t, func() bool {
		return a.continuationFailureCount("s1") == 1
	}, 2*time.Second, 10*time.Millisecond, "a failed continuation must count against the budget")
}
