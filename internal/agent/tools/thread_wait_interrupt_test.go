package tools_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

// blockingThreadManager waits until its context ends, which is what a real
// manager does while threads are still running. Only the two methods
// thread_wait uses are implemented; anything else panics rather than
// pretending to work.
type blockingThreadManager struct {
	tools.ThreadManager
	threads []tools.ThreadInfo
}

func (m *blockingThreadManager) Wait(ctx context.Context, _ []string, timeout time.Duration) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	<-ctx.Done()
	return ctx.Err()
}

func (m *blockingThreadManager) List(context.Context) ([]tools.ThreadInfo, error) {
	return m.threads, nil
}

func runWaitTool(t *testing.T, ctx context.Context, mgr tools.ThreadManager, params tools.ThreadWaitParams) fantasy.ToolResponse {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	resp, err := tools.NewThreadWaitTool(mgr).Run(ctx, fantasy.ToolCall{ID: "call-1", Input: string(input)})
	require.NoError(t, err)
	return resp
}

// TestThreadWait_UserMessageEndsTheWait is the point of the change: while
// the agent waits on threads it is doing nothing, so a message from the
// user must not sit in the queue behind that wait. The turn is handed back
// with a report instead of an error, since nothing went wrong.
func TestThreadWait_UserMessageEndsTheWait(t *testing.T) {
	t.Parallel()

	userSpoke := make(chan struct{})
	ctx := tools.WithUserInput(t.Context(), func() <-chan struct{} { return userSpoke })

	mgr := &blockingThreadManager{threads: []tools.ThreadInfo{
		{ID: "t1", Name: "alpha", Status: "running"},
		{ID: "t2", Name: "beta", Status: "merged"},
	}}

	go func() {
		time.Sleep(10 * time.Millisecond)
		close(userSpoke)
	}()

	resp := runWaitTool(t, ctx, mgr, tools.ThreadWaitParams{})
	require.False(t, resp.IsError, "being interrupted by the user is not a failure")
	require.Contains(t, resp.Content, "the user sent a message")
	require.Contains(t, resp.Content, "alpha (running)", "name what is still going")
	require.NotContains(t, resp.Content, "beta", "a settled thread is not still going")
}

// TestThreadWait_ReportsOnlyRequestedThreads: a wait scoped to some threads
// reports on those, not on everything running in the repo.
func TestThreadWait_ReportsOnlyRequestedThreads(t *testing.T) {
	t.Parallel()

	userSpoke := make(chan struct{})
	close(userSpoke)
	ctx := tools.WithUserInput(t.Context(), func() <-chan struct{} { return userSpoke })

	mgr := &blockingThreadManager{threads: []tools.ThreadInfo{
		{ID: "t1", Name: "alpha", Status: "running"},
		{ID: "t2", Name: "beta", Status: "running"},
	}}

	resp := runWaitTool(t, ctx, mgr, tools.ThreadWaitParams{IDs: []string{"beta"}})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "beta (running)")
	require.NotContains(t, resp.Content, "alpha")
}

// TestThreadWait_TimeoutStillFails: with nobody speaking, the tool behaves
// exactly as before — a timeout is still a failed wait, not a polite
// report.
func TestThreadWait_TimeoutStillFails(t *testing.T) {
	t.Parallel()

	ctx := tools.WithUserInput(t.Context(), func() <-chan struct{} { return make(chan struct{}) })
	mgr := &blockingThreadManager{}

	resp := runWaitTool(t, ctx, mgr, tools.ThreadWaitParams{TimeoutSeconds: 1})
	require.True(t, resp.IsError)
	require.NotContains(t, resp.Content, "the user sent a message")
}

// TestThreadWait_TimeoutSaysWhatToDoNext: a timeout is now the default
// ending, not something the caller asked for, so "context deadline
// exceeded" is not enough — the report has to name what is still going and
// steer away from immediately waiting again.
func TestThreadWait_TimeoutSaysWhatToDoNext(t *testing.T) {
	t.Parallel()

	ctx := tools.WithUserInput(t.Context(), func() <-chan struct{} { return make(chan struct{}) })
	mgr := &blockingThreadManager{threads: []tools.ThreadInfo{
		{ID: "t1", Name: "alpha", Status: "running"},
		{ID: "t2", Name: "beta", Status: "merged"},
	}}

	resp := runWaitTool(t, ctx, mgr, tools.ThreadWaitParams{TimeoutSeconds: 1})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "1s")
	require.Contains(t, resp.Content, "end your turn")
	require.Contains(t, resp.Content, "alpha (running)")
	require.NotContains(t, resp.Content, "beta")
}

// TestThreadWait_NoTimeoutGivenIsStillBounded: an unbounded wait rests
// entirely on the user typing or the turn being canceled, so a thread that
// hangs with neither happening would park the turn indefinitely. Saying
// nothing gets the default bound; only a negative value opts out.
func TestThreadWait_NoTimeoutGivenIsStillBounded(t *testing.T) {
	t.Parallel()

	mgr := &recordingWaitManager{}
	runWaitTool(t, t.Context(), mgr, tools.ThreadWaitParams{})
	require.Equal(t, 10*time.Minute, mgr.timeout)

	unbounded := &recordingWaitManager{}
	runWaitTool(t, t.Context(), unbounded, tools.ThreadWaitParams{TimeoutSeconds: -1})
	require.Zero(t, unbounded.timeout, "a negative timeout is the explicit opt-out")
}

// recordingWaitManager settles immediately and remembers the timeout it was
// handed, which is the thing under test.
type recordingWaitManager struct {
	tools.ThreadManager
	timeout time.Duration
}

func (m *recordingWaitManager) Wait(_ context.Context, _ []string, timeout time.Duration) error {
	m.timeout = timeout
	return nil
}

// TestThreadWait_CancelledTurnIsNotAnInterrupt: pressing escape cancels the
// whole turn, which is a different thing from typing a follow-up — it must
// not be reported as "answer the user now".
func TestThreadWait_CancelledTurnIsNotAnInterrupt(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(tools.WithUserInput(t.Context(),
		func() <-chan struct{} { return make(chan struct{}) }))
	mgr := &blockingThreadManager{}

	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	resp := runWaitTool(t, ctx, mgr, tools.ThreadWaitParams{})
	require.True(t, resp.IsError)
	require.NotContains(t, resp.Content, "the user sent a message")
}

// TestThreadWait_WithoutSignalStillWaits: a context with no user-input
// signal (a tool call outside a session) must not break — it simply never
// gets interrupted.
func TestThreadWait_WithoutSignalStillWaits(t *testing.T) {
	t.Parallel()

	mgr := &blockingThreadManager{}
	require.Nil(t, tools.WaitForUserInput(t.Context()))

	resp := runWaitTool(t, t.Context(), mgr, tools.ThreadWaitParams{TimeoutSeconds: 1})
	require.True(t, resp.IsError)
}
