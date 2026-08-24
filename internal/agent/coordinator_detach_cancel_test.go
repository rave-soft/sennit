package agent

import (
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/stretchr/testify/require"
)

// startDetachedDelegation drives a Detachable delegation through the
// same handshake TestRunSubAgent_DetachesOnUserInput_DeliversCompletionLater
// uses, stopping once it has actually detached, and hands back everything
// a cancel-ownership test needs: the coordinator, the parent and child
// session ids, and the delegate's own proceed channel (closing it lets
// the child run finish normally, for tests that need to check the
// registry drains on a path other than cancellation).
func startDetachedDelegation(t *testing.T, coord *coordinator, parentID string) (childID string, proceed chan struct{}) {
	t.Helper()

	childID = coord.sessions.CreateAgentToolSessionID("msg-1", "call-1")
	entered := make(chan struct{})
	proceed = make(chan struct{})
	delegate := &cancelableDelegate{
		model:   Model{ModelCfg: config.SelectedModel{Provider: "mock", Model: "mock-model"}},
		entered: entered,
		proceed: proceed,
	}

	userInput := make(chan struct{})
	ctx := tools.WithUserInput(t.Context(), func() <-chan struct{} { return userInput })

	respCh := make(chan subAgentRunOutcome, 1)
	go func() {
		resp, err := coord.runSubAgent(ctx, subAgentParams{
			Agent:          delegate,
			SessionID:      parentID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "look into X",
			SessionTitle:   "probe",
			AgentID:        "probe",
			Detachable:     true,
		})
		respCh <- subAgentRunOutcome{resp: resp, err: err}
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("delegate never entered Run")
	}

	close(userInput)

	select {
	case out := <-respCh:
		require.NoError(t, out.err)
		require.Contains(t, out.resp.Content, "moved to the background")
	case <-time.After(5 * time.Second):
		t.Fatal("runSubAgent did not detach promptly")
	}

	require.True(t, coord.IsSessionBusy(childID), "child must still be busy right after detaching")
	return childID, proceed
}

// TestCoordinatorCancel_ParentSessionCancelsDetachedDelegation proves a
// human hitting esc on the session that launched a now-detached
// delegation stops that delegation too, instead of leaving it to run to
// completion unattended: the child run's context is canceled, the child
// session stops reading busy, and the parent gets an "interrupted"
// completion rather than "failed" or nothing at all.
func TestCoordinatorCancel_ParentSessionCancelsDetachedDelegation(t *testing.T) {
	fake := &fakeTaskManager{info: tools.TaskInfo{ID: "task-1", SessionID: "child-sess", Status: "running"}}
	capture := &capturingSessionAgent{notify: make(chan struct{}, 1)}
	coord := newSubAgentDetachTestCoordinator(t, fake, capture, "")

	parent, err := coord.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)

	childID, _ := startDetachedDelegation(t, coord, parent.ID)

	coord.Cancel(parent.ID)

	select {
	case <-capture.notify:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled delegation's completion was never delivered")
	}

	delivered := capture.snapshot()
	require.Len(t, delivered, 1)
	require.Equal(t, parent.ID, delivered[0].sessionID)
	require.Equal(t, "interrupted", delivered[0].completion.Status)
	require.NotEmpty(t, delivered[0].completion.Error)
	require.Empty(t, delivered[0].completion.ResultText)

	require.False(t, coord.IsSessionBusy(childID), "child must no longer be busy once the cancel takes effect")

	cancelled, _ := capture.cancelledSnapshot()
	require.Contains(t, cancelled, parent.ID, "Cancel must still reach currentAgent")

	coord.detachedDelegationsMu.Lock()
	_, stillRegistered := coord.detachedDelegations[childID]
	coord.detachedDelegationsMu.Unlock()
	require.False(t, stillRegistered, "registry entry must be gone once the completion is delivered")
}

// TestCoordinatorCancel_ChildSessionIDCancelsDetachedDelegation proves
// the same delegation can be canceled by its own (child) session id -
// necessary because a detached child session is visible in the UI as a
// session of its own, addressable independently of its parent.
func TestCoordinatorCancel_ChildSessionIDCancelsDetachedDelegation(t *testing.T) {
	fake := &fakeTaskManager{info: tools.TaskInfo{ID: "task-1", SessionID: "child-sess", Status: "running"}}
	capture := &capturingSessionAgent{notify: make(chan struct{}, 1)}
	coord := newSubAgentDetachTestCoordinator(t, fake, capture, "")

	parent, err := coord.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)

	childID, _ := startDetachedDelegation(t, coord, parent.ID)

	coord.Cancel(childID)

	select {
	case <-capture.notify:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled delegation's completion was never delivered")
	}

	delivered := capture.snapshot()
	require.Len(t, delivered, 1)
	require.Equal(t, "interrupted", delivered[0].completion.Status)
	require.False(t, coord.IsSessionBusy(childID))
}

// TestCoordinatorCancelAll_CancelsDetachedDelegation proves CancelAll -
// esc esc, stop everything - reaches a detached delegation too.
func TestCoordinatorCancelAll_CancelsDetachedDelegation(t *testing.T) {
	fake := &fakeTaskManager{info: tools.TaskInfo{ID: "task-1", SessionID: "child-sess", Status: "running"}}
	capture := &capturingSessionAgent{notify: make(chan struct{}, 1)}
	coord := newSubAgentDetachTestCoordinator(t, fake, capture, "")

	parent, err := coord.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)

	childID, _ := startDetachedDelegation(t, coord, parent.ID)

	coord.CancelAll()

	select {
	case <-capture.notify:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled delegation's completion was never delivered")
	}

	delivered := capture.snapshot()
	require.Len(t, delivered, 1)
	require.Equal(t, "interrupted", delivered[0].completion.Status)
	require.False(t, coord.IsSessionBusy(childID))

	_, all := capture.cancelledSnapshot()
	require.True(t, all, "CancelAll must still reach currentAgent")
}

// TestDetachedDelegationRegistry_DrainsOnNormalCompletion proves the
// registry entry a detach creates is removed once the delegation
// finishes on its own, with no cancel involved - otherwise every
// detached delegation that runs to completion would leak a registry
// entry forever.
func TestDetachedDelegationRegistry_DrainsOnNormalCompletion(t *testing.T) {
	fake := &fakeTaskManager{info: tools.TaskInfo{ID: "task-1", SessionID: "child-sess", Status: "running"}}
	capture := &capturingSessionAgent{notify: make(chan struct{}, 1)}
	coord := newSubAgentDetachTestCoordinator(t, fake, capture, "")

	parent, err := coord.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)

	childID, proceed := startDetachedDelegation(t, coord, parent.ID)

	coord.detachedDelegationsMu.Lock()
	_, registered := coord.detachedDelegations[childID]
	coord.detachedDelegationsMu.Unlock()
	require.True(t, registered, "registry must hold the entry while the delegation is still running")

	close(proceed)

	select {
	case <-capture.notify:
	case <-time.After(5 * time.Second):
		t.Fatal("detached delegation's completion was never delivered")
	}

	delivered := capture.snapshot()
	require.Len(t, delivered, 1)
	require.Equal(t, "completed", delivered[0].completion.Status)

	coord.detachedDelegationsMu.Lock()
	_, stillRegistered := coord.detachedDelegations[childID]
	coord.detachedDelegationsMu.Unlock()
	require.False(t, stillRegistered, "registry entry must not leak once the delegation finishes on its own")
}

// TestCoordinatorCancel_NoDetachedDelegations_StillReachesCurrentAgent
// proves Cancel keeps working exactly as before for a session with
// nothing detached under it - the new detached-delegation lookup must
// be a no-op rather than short-circuit the existing call.
func TestCoordinatorCancel_NoDetachedDelegations_StillReachesCurrentAgent(t *testing.T) {
	capture := &capturingSessionAgent{notify: make(chan struct{}, 1)}
	coord := newSubAgentDetachTestCoordinator(t, nil, capture, "")

	coord.Cancel("some-session")

	cancelled, _ := capture.cancelledSnapshot()
	require.Equal(t, []string{"some-session"}, cancelled)
}
