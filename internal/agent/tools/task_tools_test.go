package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"charm.land/fantasy"
	"github.com/rave-soft/braid/internal/permission"
	"github.com/stretchr/testify/require"
)

// fakeTaskManager is an in-memory TaskManager double for testing the
// task_* tool wrappers' parameter validation and response shaping. It
// does not model internal/thread's actual kind-guard or run-dispatch
// behavior — that is exercised by internal/thread's own tests — but it
// does return the same shape of error a real TaskManager would for an
// unknown or wrong-kind id, so these tests can prove the tools relay it
// correctly.
type fakeTaskManager struct {
	tasks       map[string]TaskInfo
	cancelCalls []struct{ id, reason string }
	listErr     error
}

func newFakeTaskManager() *fakeTaskManager {
	return &fakeTaskManager{tasks: make(map[string]TaskInfo)}
}

func (f *fakeTaskManager) Create(context.Context, TaskCreateArgs) (TaskInfo, error) {
	panic("not used by task_* tool tests")
}

func (f *fakeTaskManager) List(context.Context) ([]TaskInfo, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]TaskInfo, 0, len(f.tasks))
	for _, ti := range f.tasks {
		out = append(out, ti)
	}
	return out, nil
}

func (f *fakeTaskManager) Get(_ context.Context, id string) (TaskInfo, error) {
	ti, ok := f.tasks[id]
	if !ok {
		return TaskInfo{}, fmt.Errorf("thread: %q is not a task", id)
	}
	return ti, nil
}

func (f *fakeTaskManager) Cancel(_ context.Context, id, reason string) error {
	ti, ok := f.tasks[id]
	if !ok {
		return fmt.Errorf("thread: %q is not a task", id)
	}
	f.cancelCalls = append(f.cancelCalls, struct{ id, reason string }{id, reason})
	if reason == "" {
		reason = "cancelled"
	}
	ti.Status = "interrupted"
	ti.Error = reason
	f.tasks[id] = ti
	return nil
}

func skipPermissions(t *testing.T) permission.Service {
	t.Helper()
	return permission.NewPermissionService(t.TempDir(), true, nil)
}

func callTaskTool(t *testing.T, tool fantasy.AgentTool, params any) fantasy.ToolResponse {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	resp, err := tool.Run(context.WithValue(t.Context(), SessionIDContextKey, "caller-session"),
		fantasy.ToolCall{ID: "call-1", Input: string(input)})
	require.NoError(t, err)
	return resp
}

func TestTaskListTool_ListsTasks(t *testing.T) {
	manager := newFakeTaskManager()
	manager.tasks["t1"] = TaskInfo{ID: "t1", Goal: "look into X", Status: "running"}

	resp := callTaskTool(t, NewTaskListTool(manager), TaskListParams{})
	require.False(t, resp.IsError)

	var meta TaskListResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Len(t, meta.Tasks, 1)
	require.Equal(t, "t1", meta.Tasks[0].ID)
}

func TestTaskListTool_EmptyWhenNoTasks(t *testing.T) {
	resp := callTaskTool(t, NewTaskListTool(newFakeTaskManager()), TaskListParams{})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "No background tasks")
}

func TestTaskListTool_SurfacesManagerError(t *testing.T) {
	manager := newFakeTaskManager()
	manager.listErr = fmt.Errorf("boom")

	resp := callTaskTool(t, NewTaskListTool(manager), TaskListParams{})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "boom")
}

func TestTaskResultTool_ReturnsFinalAnswerWhenCompleted(t *testing.T) {
	manager := newFakeTaskManager()
	manager.tasks["t1"] = TaskInfo{ID: "t1", Status: "completed", ResultSummary: "the answer is 42"}

	resp := callTaskTool(t, NewTaskResultTool(manager), TaskResultParams{ID: "t1"})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "the answer is 42")
}

func TestTaskResultTool_ReportsStatusWhenStillRunning(t *testing.T) {
	manager := newFakeTaskManager()
	manager.tasks["t1"] = TaskInfo{ID: "t1", Status: "running"}

	resp := callTaskTool(t, NewTaskResultTool(manager), TaskResultParams{ID: "t1"})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "still running")
	require.NotContains(t, resp.Content, "finished")
}

func TestTaskResultTool_MissingID(t *testing.T) {
	resp := callTaskTool(t, NewTaskResultTool(newFakeTaskManager()), TaskResultParams{})
	require.True(t, resp.IsError)
}

// TestTaskResultTool_WrongKindRejection proves the tool relays the
// "not a task" rejection a real TaskManager.Get returns for a thread's
// id (see internal/thread's TestTaskManager_GetRejectsThreadID for the
// guard itself) as a clear tool error, not a crash or a silent result.
func TestTaskResultTool_WrongKindRejection(t *testing.T) {
	resp := callTaskTool(t, NewTaskResultTool(newFakeTaskManager()), TaskResultParams{ID: "a-thread-id"})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "is not a task")
}

func TestTaskCancelTool_CancelsRunningTask(t *testing.T) {
	manager := newFakeTaskManager()
	manager.tasks["t1"] = TaskInfo{ID: "t1", Status: "running"}
	tool := NewTaskCancelTool(manager, skipPermissions(t))

	resp := callTaskTool(t, tool, TaskCancelParams{ID: "t1", Reason: "no longer needed"})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "interrupted")

	require.Len(t, manager.cancelCalls, 1)
	require.Equal(t, "t1", manager.cancelCalls[0].id)
	require.Equal(t, "no longer needed", manager.cancelCalls[0].reason)
}

func TestTaskCancelTool_MissingID(t *testing.T) {
	tool := NewTaskCancelTool(newFakeTaskManager(), skipPermissions(t))
	resp := callTaskTool(t, tool, TaskCancelParams{})
	require.True(t, resp.IsError)
}

// TestTaskCancelTool_WrongKindRejection mirrors
// TestTaskResultTool_WrongKindRejection: cancelling a thread's id must
// surface the same clear rejection, not silently do nothing or panic.
func TestTaskCancelTool_WrongKindRejection(t *testing.T) {
	manager := newFakeTaskManager()
	tool := NewTaskCancelTool(manager, skipPermissions(t))
	resp := callTaskTool(t, tool, TaskCancelParams{ID: "a-thread-id"})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "is not a task")
	require.Empty(t, manager.cancelCalls, "rejected id must never reach a resolved Cancel")
}
