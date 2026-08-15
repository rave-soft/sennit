package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/rave-soft/braid/internal/app/threadspawn"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/thread"
	"github.com/stretchr/testify/require"
)

// newTaskTestHarness extends newThreadTestHarness with a *thread.TaskManager
// sharing the harness's *thread.Manager lifecycle — exactly how
// thread.Attach wires a workspace in production — via
// thread.NewTaskManagerForTest, since NewTaskManager itself needs the
// Manager's unexported lc/ctx and so can't be called from this package
// directly.
//
// Unlike a thread (which gets a fresh, fully-wired isolated App per spawn
// — see fakeThreadSpawner.Spawn), a task's ParentAppSpawner runs inside
// the workspace's own top-level App (h.ws.App) directly. newThreadTestHarness
// never needed that App's own Sessions/AgentCoordinator wired (every
// thread brings its own), so this fills them in the same way
// fakeThreadSpawner does, giving a task's dispatched run the same
// deterministic, LLM-free behavior a thread's has: it returns immediately
// without erroring, leaving the task's runtime live (StatusRunning) until
// something explicitly cancels it.
func newTaskTestHarness(t *testing.T) (*threadTestHarness, *thread.TaskManager) {
	t.Helper()
	h := newThreadTestHarness(t)
	h.ws.SetSessionsForTest(&fakeThreadSessions{})
	h.ws.AgentCoordinator = &fakeThreadCoordinator{}
	tasks := thread.NewTaskManagerForTest(h.mgr, threadspawn.NewParentAppSpawner(h.parentWorkspace), threadspawn.NewMessageService(h.ws.Messages()))
	h.ws.SetTaskManager(tasks)
	return h, tasks
}

func taskURL(h *threadTestHarness, path string) string {
	return h.httpSrv.URL + "/v1/workspaces/" + h.ws.ID + "/tasks" + path
}

func doTaskRequest(t *testing.T, h *threadTestHarness, method, path string, body any) *http.Response {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, taskURL(h, path), reader)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.httpSrv.Client().Do(req)
	require.NoError(t, err)
	return resp
}

// TestHandleWorkspaceTasks_NoManager verifies that a workspace with a
// thread manager but no task manager reports 409 on the tasks route —
// mirroring TestHandleWorkspaceThreads_NoManager exactly, just for the
// sibling capability.
func TestHandleWorkspaceTasks_NoManager(t *testing.T) {
	t.Parallel()
	h := newThreadTestHarness(t) // no SetTaskManager

	resp := doTaskRequest(t, h, http.MethodGet, "", nil)
	defer resp.Body.Close()
	require.Equal(t, http.StatusConflict, resp.StatusCode)
}

// TestHandleWorkspaceTasks_List is the happy path: tasks created
// in-process (task creation is deliberately not exposed over HTTP — the
// model creates tasks through its tools, inside the workspace) show up in
// the list, tagged Kind=task.
func TestHandleWorkspaceTasks_List(t *testing.T) {
	t.Parallel()
	h, tasks := newTaskTestHarness(t)

	first, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "scan for TODOs", ParentSessionID: "parent-sess"})
	require.NoError(t, err)
	second, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "check the build", ParentSessionID: "parent-sess"})
	require.NoError(t, err)

	resp := doTaskRequest(t, h, http.MethodGet, "", nil)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var list []proto.Thread
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&list))
	require.Len(t, list, 2)
	for _, item := range list {
		require.Equal(t, "task", item.Kind)
		require.Empty(t, item.WorkspaceID, "a task has no isolated workspace of its own")
	}
	ids := []string{list[0].ID, list[1].ID}
	require.ElementsMatch(t, []string{first.ID, second.ID}, ids)
}

// TestHandleWorkspaceTaskCancel_Success cancels a live task over HTTP and
// checks the returned state: cancelled, with the request's reason
// recorded as its terminal error — the same outcome
// TaskManager.Cancel produces locally.
func TestHandleWorkspaceTaskCancel_Success(t *testing.T) {
	t.Parallel()
	h, tasks := newTaskTestHarness(t)

	created, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "do it", ParentSessionID: "parent-sess"})
	require.NoError(t, err)
	require.Equal(t, thread.StatusRunning, created.Status)

	resp := doTaskRequest(t, h, http.MethodPost, "/"+created.ID+"/cancel", proto.CancelDelegationRequest{Reason: "no longer needed"})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got proto.Thread
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Equal(t, created.ID, got.ID)
	require.Equal(t, "cancelled", got.Status)
	require.Equal(t, "no longer needed", got.Error)
}

// TestHandleWorkspaceTaskCancel_UnknownID verifies an id that doesn't exist
// at all reports 404.
func TestHandleWorkspaceTaskCancel_UnknownID(t *testing.T) {
	t.Parallel()
	h, _ := newTaskTestHarness(t)

	resp := doTaskRequest(t, h, http.MethodPost, "/does-not-exist/cancel", proto.CancelDelegationRequest{Reason: "x"})
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestHandleWorkspaceTaskCancel_WrongKind verifies that cancelling a
// thread's id through the task cancel route is rejected — a 409, the same
// status this package already uses for a capability/state mismatch
// (workspace-does-not-support-X, Activate refusing a thread mid-merge,
// Remove refusing an active/dirty thread), not a 400 (reserved here for
// malformed request bodies and Create-time validation) or a 404 (the id
// does exist, just as the wrong kind) — mirroring how the local
// TaskManager.Get/Cancel path refuses a thread's id, just with a
// distinguishable HTTP status the local Go error doesn't carry on its
// own.
func TestHandleWorkspaceTaskCancel_WrongKind(t *testing.T) {
	t.Parallel()
	h, _ := newTaskTestHarness(t)

	// A goal-less thread rests idle rather than dispatching a run — the
	// simplest way to get a real thread id without needing the fake
	// coordinator's run to settle first.
	th, err := h.mgr.Create(t.Context(), thread.CreateArgs{Name: "a-thread"})
	require.NoError(t, err)

	resp := doTaskRequest(t, h, http.MethodPost, "/"+th.ID+"/cancel", proto.CancelDelegationRequest{Reason: "x"})
	defer resp.Body.Close()
	require.Equal(t, http.StatusConflict, resp.StatusCode)
}

// TestTaskEvent_DeliveredOverSSE is the SSE finding, made concrete: a
// task's status-changed event (published on the same shared lifecycle
// broker a thread's event is — see internal/thread.NewTaskManager's doc
// comment) reaches the workspace's SSE stream exactly like a thread's
// does, with Kind="task" intact. wrapEvent (events.go) has no kind
// filtering at all in its `case pubsub.Event[thread.Event]` branch, and
// neither AppWorkspace.translateEvent (local mode) nor
// ClientWorkspace.translateEvent (client/server mode, decoding this same
// SSE payload) filter by kind either — the whole event pipeline was
// already generic across delegation kinds before this step, so a task's
// row never goes stale on screen in client/server mode the way a
// kind-filtered pipeline would have left it.
func TestTaskEvent_DeliveredOverSSE(t *testing.T) {
	t.Parallel()
	h, tasks := newTaskTestHarness(t)

	events, err := h.c.backend.SubscribeEvents(t.Context(), h.ws.ID)
	require.NoError(t, err)

	created, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "do it", ParentSessionID: "parent-sess"})
	require.NoError(t, err)

	require.NoError(t, tasks.Cancel(t.Context(), created.ID, "stop"))

	for {
		select {
		case ev := <-events:
			wrapped := wrapEvent(ev.Payload)
			if wrapped == nil || wrapped.Type != pubsub.PayloadTypeThreadEvent {
				continue
			}
			var decoded pubsub.Event[proto.ThreadEvent]
			require.NoError(t, json.Unmarshal(wrapped.Payload, &decoded))
			if decoded.Payload.Thread.ID != created.ID {
				continue
			}
			if decoded.Payload.Thread.Status != "cancelled" {
				continue // the "created"/"running" events for this id arrive first
			}
			require.Equal(t, "task", decoded.Payload.Thread.Kind)
			require.Equal(t, proto.ThreadEventStatusChanged, decoded.Payload.Type)
			return
		case <-t.Context().Done():
			t.Fatal("timed out waiting for the task cancellation event over SSE")
		}
	}
}
