package server

import (
	"encoding/json"
	"net/http"

	"github.com/rave-soft/braid/internal/backend"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/thread"
)

// workspaceTaskManager resolves ws (already looked up by the caller) to its
// concrete *thread.TaskManager. Mirrors workspaceThreadManager (threads.go)
// for the same reason: a non-git workspace, or one Attach otherwise
// declined for, has neither manager.
func workspaceTaskManager(ws *backend.Workspace) (*thread.TaskManager, bool) {
	if ws == nil || ws.App == nil {
		return nil, false
	}
	mgr, ok := ws.TaskManager().(*thread.TaskManager)
	return mgr, ok && mgr != nil
}

// requireTaskManager looks up id's workspace and its task manager, writing
// the appropriate error response (404/409) and returning ok=false on
// failure. Mirrors requireThreadManager.
func (c *controllerV1) requireTaskManager(w http.ResponseWriter, r *http.Request, id string) (*thread.TaskManager, bool) {
	ws, err := c.backend.GetWorkspace(id)
	if err != nil {
		c.handleError(w, r, err)
		return nil, false
	}
	mgr, ok := workspaceTaskManager(ws)
	if !ok {
		jsonError(w, http.StatusConflict, "workspace does not support tasks")
		return nil, false
	}
	return mgr, true
}

// requireTask resolves taskID within a workspace that already has a task
// manager (ws, mgr from requireTaskManager) to a task, rejecting a
// thread's id the same way the local TaskManager.Get/Cancel path does —
// "is not a task" — but as a distinguishable HTTP status.
//
// It resolves through mgr's own thread.Manager (kind-agnostic — see
// Manager.Get's doc comment: unlike TaskManager.Get, it never checks Kind)
// purely to tell "doesn't exist at all" (404, mirroring
// handleGetWorkspaceThread's convention for the analogous case) apart from
// "exists, wrong kind" (409 — the same status already used elsewhere in
// this package for a capability/state mismatch: workspace-does-not-
// support-X, Activate refusing a thread mid-merge, Remove refusing an
// active/dirty thread — rather than 400, reserved here for malformed
// request bodies and Create-time validation). Tasks and threads always
// share one thread.Manager/TaskManager pair per workspace (see
// thread.NewTaskManager's doc comment), so threadMgr not existing once
// taskMgr does would be a server invariant violation, not reachable client
// behavior — treated as a 500.
func (c *controllerV1) requireTask(w http.ResponseWriter, r *http.Request, id, taskID string) (thread.Thread, bool) {
	ws, err := c.backend.GetWorkspace(id)
	if err != nil {
		c.handleError(w, r, err)
		return thread.Thread{}, false
	}
	threadMgr, ok := workspaceThreadManager(ws)
	if !ok {
		c.server.logError(r, "task manager present without a thread manager", "workspace", id)
		jsonError(w, http.StatusInternalServerError, "workspace task/thread managers disagree")
		return thread.Thread{}, false
	}

	st, err := threadMgr.Get(r.Context(), taskID)
	if err != nil {
		jsonError(w, http.StatusNotFound, "task not found")
		return thread.Thread{}, false
	}
	if st.Kind != thread.KindTask {
		jsonError(w, http.StatusConflict, "not a task")
		return thread.Thread{}, false
	}
	return st, true
}

// handleGetWorkspaceTasks lists tasks for a workspace.
//
//	@Summary		List tasks
//	@Tags			tasks
//	@Produce		json
//	@Param			id	path		string			true	"Workspace ID"
//	@Success		200	{array}		proto.Thread
//	@Failure		404	{object}	proto.Error
//	@Failure		409	{object}	proto.Error
//	@Failure		500	{object}	proto.Error
//	@Router			/workspaces/{id}/tasks [get]
func (c *controllerV1) handleGetWorkspaceTasks(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	mgr, ok := c.requireTaskManager(w, r, id)
	if !ok {
		return
	}
	tasks, err := mgr.List(r.Context())
	if err != nil {
		c.handleError(w, r, err)
		return
	}
	result := make([]proto.Thread, len(tasks))
	for i, st := range tasks {
		// A task has no workspace of its own — see TaskController's doc
		// comment in internal/workspace/workspace.go.
		result[i] = thread.ToProto(st, "")
	}
	jsonEncode(w, result)
}

// handlePostWorkspaceTaskCancel cancels a task's in-flight run, recording
// the request's reason as its terminal error.
//
//	@Summary		Cancel task
//	@Tags			tasks
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string					true	"Workspace ID"
//	@Param			taskID	path		string					true	"Task ID"
//	@Param			request	body		proto.CancelDelegationRequest	false	"Cancel reason"
//	@Success		200		{object}	proto.Thread
//	@Failure		400		{object}	proto.Error
//	@Failure		404		{object}	proto.Error
//	@Failure		409		{object}	proto.Error
//	@Failure		500		{object}	proto.Error
//	@Router			/workspaces/{id}/tasks/{taskID}/cancel [post]
func (c *controllerV1) handlePostWorkspaceTaskCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	taskID := r.PathValue("taskID")
	mgr, ok := c.requireTaskManager(w, r, id)
	if !ok {
		return
	}
	if _, ok := c.requireTask(w, r, id, taskID); !ok {
		return
	}

	var req proto.CancelDelegationRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			c.server.logError(r, "Failed to decode request", "error", err)
			jsonError(w, http.StatusBadRequest, "failed to decode request")
			return
		}
	}

	if err := mgr.Cancel(r.Context(), taskID, req.Reason); err != nil {
		c.server.logError(r, "Failed to cancel task", "error", err)
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	st, err := mgr.Get(r.Context(), taskID)
	if err != nil {
		c.handleError(w, r, err)
		return
	}
	jsonEncode(w, thread.ToProto(st, ""))
}
