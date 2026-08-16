package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/permission"
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
	sendCalls   []struct{ id, message string }
	outputs     map[string]TaskOutput
	listErr     error
	// sendOutcome is what Send reports back for an accepted message, so a
	// test can pick which of the outcomes task_send has to render.
	sendOutcome SendOutcome
}

func newFakeTaskManager() *fakeTaskManager {
	return &fakeTaskManager{tasks: make(map[string]TaskInfo), outputs: make(map[string]TaskOutput)}
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
	ti.Status = "cancelled"
	ti.Error = reason
	f.tasks[id] = ti
	return nil
}

func (f *fakeTaskManager) Send(_ context.Context, id, message string) (SendOutcome, error) {
	if _, ok := f.tasks[id]; !ok {
		return SendOutcome{}, fmt.Errorf("thread: %q is not a task", id)
	}
	f.sendCalls = append(f.sendCalls, struct{ id, message string }{id, message})
	return f.sendOutcome, nil
}

// Output returns exactly what the test configured in f.outputs[id],
// including its Total: the limit/truncation bound itself is
// internal/thread's behavior (see TestTaskManager_OutputReportsTruncation
// there), not this fake's to reimplement. This tool layer only needs to
// prove the tool renders whatever (Messages, Total) shape it is handed.
func (f *fakeTaskManager) Output(_ context.Context, id string, _ int) (TaskOutput, error) {
	if _, ok := f.tasks[id]; !ok {
		return TaskOutput{}, fmt.Errorf("thread: %q is not a task", id)
	}
	return f.outputs[id], nil
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
	require.Contains(t, resp.Content, "cancelled")

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

func TestTaskSendTool_SendsMessage(t *testing.T) {
	manager := newFakeTaskManager()
	manager.tasks["t1"] = TaskInfo{ID: "t1", Status: "running"}

	resp := callTaskTool(t, NewTaskSendTool(manager), TaskSendParams{ID: "t1", Message: "keep going"})
	require.False(t, resp.IsError)

	require.Len(t, manager.sendCalls, 1)
	require.Equal(t, "t1", manager.sendCalls[0].id)
	require.Equal(t, "keep going", manager.sendCalls[0].message)
}

// The tool's answer has to distinguish "the agent is reading this now"
// from "this is waiting behind a turn already in flight" — a caller that
// cannot tell them apart treats a message that steered nothing as
// delivered. See SendOutcome.
func TestTaskSendTool_ReportsQueuedDelivery(t *testing.T) {
	manager := newFakeTaskManager()
	manager.tasks["t1"] = TaskInfo{ID: "t1", Status: "running"}
	manager.sendOutcome = SendOutcome{Queued: true, Ahead: 2}

	resp := callTaskTool(t, NewTaskSendTool(manager), TaskSendParams{ID: "t1", Message: "wrap up"})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "Queued")
	require.Contains(t, resp.Content, "mid-turn")
	require.Contains(t, resp.Content, "2 earlier message(s)")

	manager.sendOutcome = SendOutcome{}
	resp = callTaskTool(t, NewTaskSendTool(manager), TaskSendParams{ID: "t1", Message: "and this"})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "starts a turn on it now")
}

func TestTaskSendTool_MissingFields(t *testing.T) {
	manager := newFakeTaskManager()
	manager.tasks["t1"] = TaskInfo{ID: "t1", Status: "running"}
	tool := NewTaskSendTool(manager)

	resp := callTaskTool(t, tool, TaskSendParams{Message: "hi"})
	require.True(t, resp.IsError)

	resp = callTaskTool(t, tool, TaskSendParams{ID: "t1"})
	require.True(t, resp.IsError)
}

// TestTaskSendTool_WrongKindRejection mirrors the other tools' wrong-kind
// tests.
func TestTaskSendTool_WrongKindRejection(t *testing.T) {
	manager := newFakeTaskManager()
	resp := callTaskTool(t, NewTaskSendTool(manager), TaskSendParams{ID: "a-thread-id", Message: "hi"})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "is not a task")
	require.Empty(t, manager.sendCalls, "rejected id must never reach a resolved Send")
}

func TestTaskOutputTool_ReturnsMessages(t *testing.T) {
	manager := newFakeTaskManager()
	manager.tasks["t1"] = TaskInfo{ID: "t1"}
	manager.outputs["t1"] = TaskOutput{
		Messages: []TaskOutputMessage{
			{Role: "user", Text: "investigate X"},
			{Role: "assistant", Text: "here is what I found"},
		},
		Total: 2,
	}

	resp := callTaskTool(t, NewTaskOutputTool(manager), TaskOutputParams{ID: "t1"})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "investigate X")
	require.Contains(t, resp.Content, "here is what I found")
	require.NotContains(t, resp.Content, "Showing last", "no truncation banner when nothing was truncated")
}

// TestTaskOutputTool_ReportsTruncation is the bound the coordinator asked
// to be explicit about: a task with more messages than the limit must say
// so in the response text, not just silently hand back a partial tail.
func TestTaskOutputTool_ReportsTruncation(t *testing.T) {
	manager := newFakeTaskManager()
	manager.tasks["t1"] = TaskInfo{ID: "t1"}
	manager.outputs["t1"] = TaskOutput{
		Messages: []TaskOutputMessage{
			{Role: "user", Text: "message 3"},
			{Role: "user", Text: "message 4"},
		},
		Total: 5,
	}

	resp := callTaskTool(t, NewTaskOutputTool(manager), TaskOutputParams{ID: "t1", Limit: 2})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "Showing last 2 of 5 messages")

	var meta TaskOutput
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, 5, meta.Total)
	require.Len(t, meta.Messages, 2)
}

func TestTaskOutputTool_EmptyWhenNoMessages(t *testing.T) {
	manager := newFakeTaskManager()
	manager.tasks["t1"] = TaskInfo{ID: "t1"}

	resp := callTaskTool(t, NewTaskOutputTool(manager), TaskOutputParams{ID: "t1"})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "No output yet")
}

func TestTaskOutputTool_MissingID(t *testing.T) {
	resp := callTaskTool(t, NewTaskOutputTool(newFakeTaskManager()), TaskOutputParams{})
	require.True(t, resp.IsError)
}

func TestTaskOutputTool_WrongKindRejection(t *testing.T) {
	resp := callTaskTool(t, NewTaskOutputTool(newFakeTaskManager()), TaskOutputParams{ID: "a-thread-id"})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "is not a task")
}
