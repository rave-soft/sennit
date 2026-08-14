package server

import (
	"encoding/json"
	"net/http"

	"github.com/rave-soft/braid/internal/backend"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/thread"
)

// workspaceThreadManager resolves ws (already looked up by the caller) to
// its concrete *thread.Manager. Returns (nil, false) when the workspace has
// no thread manager (non-git workspace, or a thread's own nested
// workspace) — callers turn that into a 409: the workspace exists and is
// otherwise fine, it just doesn't support threads, which is different from
// a 404 (workspace/thread not found).
func workspaceThreadManager(ws *backend.Workspace) (*thread.Manager, bool) {
	if ws == nil || ws.App == nil {
		return nil, false
	}
	mgr, ok := ws.ThreadManager().(*thread.Manager)
	return mgr, ok && mgr != nil
}

// requireThreadManager looks up id's workspace and its thread manager,
// writing the appropriate error response (404/409) and returning ok=false
// on failure. Shared by every handler in this file.
func (c *controllerV1) requireThreadManager(w http.ResponseWriter, r *http.Request, id string) (*thread.Manager, bool) {
	ws, err := c.backend.GetWorkspace(id)
	if err != nil {
		c.handleError(w, r, err)
		return nil, false
	}
	mgr, ok := workspaceThreadManager(ws)
	if !ok {
		jsonError(w, http.StatusConflict, "workspace does not support threads")
		return nil, false
	}
	return mgr, true
}

// handleGetWorkspaceThreads lists threads for a workspace.
//
//	@Summary		List threads
//	@Tags			threads
//	@Produce		json
//	@Param			id	path		string			true	"Workspace ID"
//	@Success		200	{array}		proto.Thread
//	@Failure		404	{object}	proto.Error
//	@Failure		409	{object}	proto.Error
//	@Failure		500	{object}	proto.Error
//	@Router			/workspaces/{id}/threads [get]
func (c *controllerV1) handleGetWorkspaceThreads(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	mgr, ok := c.requireThreadManager(w, r, id)
	if !ok {
		return
	}
	threads, err := mgr.List(r.Context())
	if err != nil {
		c.handleError(w, r, err)
		return
	}
	result := make([]proto.Thread, len(threads))
	for i, st := range threads {
		result[i] = mgr.ToProto(st)
	}
	jsonEncode(w, result)
}

// handlePostWorkspaceThreads creates a new thread.
//
//	@Summary		Create thread
//	@Tags			threads
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string						true	"Workspace ID"
//	@Param			request	body		proto.CreateThreadRequest	true	"Thread creation params"
//	@Success		200		{object}	proto.Thread
//	@Failure		400		{object}	proto.Error
//	@Failure		404		{object}	proto.Error
//	@Failure		409		{object}	proto.Error
//	@Failure		500		{object}	proto.Error
//	@Router			/workspaces/{id}/threads [post]
func (c *controllerV1) handlePostWorkspaceThreads(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	mgr, ok := c.requireThreadManager(w, r, id)
	if !ok {
		return
	}

	var req proto.CreateThreadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		c.server.logError(r, "Failed to decode request", "error", err)
		jsonError(w, http.StatusBadRequest, "failed to decode request")
		return
	}

	st, err := mgr.Create(r.Context(), thread.CreateArgs{
		Name:        req.Name,
		Goal:        req.Goal,
		BaseBranch:  req.BaseBranch,
		MergePolicy: thread.MergePolicy(req.MergePolicy),
	})
	if err != nil {
		// Manager.Create's errors are all validation-shaped (invalid
		// name, name already in use, branch already exists, resolve
		// base branch, ...) rather than "not found" or an ID conflict,
		// so a plain 400 covers them.
		c.server.logError(r, "Failed to create thread", "error", err)
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	jsonEncode(w, mgr.ToProto(st))
}

// handleGetWorkspaceThread returns a single thread.
//
//	@Summary		Get thread
//	@Tags			threads
//	@Produce		json
//	@Param			id			path		string	true	"Workspace ID"
//	@Param			threadID	path		string	true	"Thread ID or name"
//	@Success		200			{object}	proto.Thread
//	@Failure		404			{object}	proto.Error
//	@Failure		409			{object}	proto.Error
//	@Failure		500			{object}	proto.Error
//	@Router			/workspaces/{id}/threads/{threadID} [get]
func (c *controllerV1) handleGetWorkspaceThread(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	threadID := r.PathValue("threadID")
	mgr, ok := c.requireThreadManager(w, r, id)
	if !ok {
		return
	}

	st, err := mgr.Get(r.Context(), threadID)
	if err != nil {
		// Get/resolve's only failure mode is "no such ID/name" (see
		// internal/thread.Manager.resolve): it tries the store by ID,
		// falls back to name, and returns the name lookup's error. Treat
		// any error here as not-found rather than trying to distinguish
		// further.
		jsonError(w, http.StatusNotFound, "thread not found")
		return
	}
	jsonEncode(w, mgr.ToProto(st))
}

// handlePostWorkspaceThreadSend sends a follow-up message to a thread.
//
//	@Summary		Send message to thread
//	@Tags			threads
//	@Accept			json
//	@Param			id			path	string						true	"Workspace ID"
//	@Param			threadID	path	string						true	"Thread ID or name"
//	@Param			request		body	proto.SendThreadRequest	true	"Message to send"
//	@Success		200
//	@Failure		400	{object}	proto.Error
//	@Failure		404	{object}	proto.Error
//	@Failure		409	{object}	proto.Error
//	@Failure		500	{object}	proto.Error
//	@Router			/workspaces/{id}/threads/{threadID}/send [post]
func (c *controllerV1) handlePostWorkspaceThreadSend(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	threadID := r.PathValue("threadID")
	mgr, ok := c.requireThreadManager(w, r, id)
	if !ok {
		return
	}

	var req proto.SendThreadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		c.server.logError(r, "Failed to decode request", "error", err)
		jsonError(w, http.StatusBadRequest, "failed to decode request")
		return
	}

	if _, err := mgr.Get(r.Context(), threadID); err != nil {
		jsonError(w, http.StatusNotFound, "thread not found")
		return
	}
	if err := mgr.Send(r.Context(), threadID, req.Message); err != nil {
		c.server.logError(r, "Failed to send to thread", "error", err)
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handlePostWorkspaceThreadActivate reactivates a thread's workspace.
//
//	@Summary		Activate thread
//	@Tags			threads
//	@Produce		json
//	@Param			id			path		string	true	"Workspace ID"
//	@Param			threadID	path		string	true	"Thread ID or name"
//	@Success		200			{object}	proto.Thread
//	@Failure		404			{object}	proto.Error
//	@Failure		409			{object}	proto.Error
//	@Failure		500			{object}	proto.Error
//	@Router			/workspaces/{id}/threads/{threadID}/activate [post]
func (c *controllerV1) handlePostWorkspaceThreadActivate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	threadID := r.PathValue("threadID")
	mgr, ok := c.requireThreadManager(w, r, id)
	if !ok {
		return
	}

	if _, err := mgr.Get(r.Context(), threadID); err != nil {
		jsonError(w, http.StatusNotFound, "thread not found")
		return
	}
	st, err := mgr.Activate(r.Context(), threadID)
	if err != nil {
		// Refusing to reactivate a thread in the merge flow is a
		// statement about the thread's state, not a server fault, so it
		// reads as a conflict rather than a 500.
		c.server.logError(r, "Failed to activate thread", "error", err)
		jsonError(w, http.StatusConflict, err.Error())
		return
	}
	jsonEncode(w, mgr.ToProto(st))
}

// handlePostWorkspaceThreadMerge merges (or retries merging) a thread.
//
//	@Summary		Merge thread
//	@Tags			threads
//	@Produce		json
//	@Param			id			path		string	true	"Workspace ID"
//	@Param			threadID	path		string	true	"Thread ID or name"
//	@Success		200			{object}	proto.Thread
//	@Failure		404			{object}	proto.Error
//	@Failure		409			{object}	proto.Error
//	@Failure		500			{object}	proto.Error
//	@Router			/workspaces/{id}/threads/{threadID}/merge [post]
func (c *controllerV1) handlePostWorkspaceThreadMerge(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	threadID := r.PathValue("threadID")
	mgr, ok := c.requireThreadManager(w, r, id)
	if !ok {
		return
	}

	if _, err := mgr.Get(r.Context(), threadID); err != nil {
		jsonError(w, http.StatusNotFound, "thread not found")
		return
	}
	// Conflict/merge-blocked outcomes are recorded as thread statuses,
	// not returned as Go errors (Merge returns nil in those cases) — see
	// internal/thread.Manager.mergeAttempt. A non-nil error here means
	// something more fundamental went wrong (e.g. a git command itself
	// failed to run), which is a server-side failure, not a client one.
	// Merge returns the outcome directly: a thread that merged cleanly is
	// discarded, so there is no row left to re-read here.
	st, err := mgr.Merge(r.Context(), threadID)
	if err != nil {
		c.server.logError(r, "Failed to merge thread", "error", err)
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonEncode(w, mgr.ToProto(st))
}

// handlePostWorkspaceThreadCancel cancels a thread's in-flight run,
// leaving its worktree and branch on disk — unlike delete, which tears
// everything down. Mirrors handlePostWorkspaceTaskCancel.
//
//	@Summary		Cancel thread
//	@Tags			threads
//	@Accept			json
//	@Produce		json
//	@Param			id			path		string						true	"Workspace ID"
//	@Param			threadID	path		string						true	"Thread ID or name"
//	@Param			request		body		proto.CancelDelegationRequest	false	"Cancel reason"
//	@Success		200			{object}	proto.Thread
//	@Failure		400			{object}	proto.Error
//	@Failure		404			{object}	proto.Error
//	@Failure		409			{object}	proto.Error
//	@Failure		500			{object}	proto.Error
//	@Router			/workspaces/{id}/threads/{threadID}/cancel [post]
func (c *controllerV1) handlePostWorkspaceThreadCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	threadID := r.PathValue("threadID")
	mgr, ok := c.requireThreadManager(w, r, id)
	if !ok {
		return
	}

	if _, err := mgr.Get(r.Context(), threadID); err != nil {
		jsonError(w, http.StatusNotFound, "thread not found")
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

	// Refusing to cancel a thread in the merge flow is a statement about
	// the thread's state, not a server fault, so it reads as a conflict
	// rather than a 500 — mirroring handlePostWorkspaceThreadActivate's
	// identical call.
	if err := mgr.Cancel(r.Context(), threadID, req.Reason); err != nil {
		c.server.logError(r, "Failed to cancel thread", "error", err)
		jsonError(w, http.StatusConflict, err.Error())
		return
	}
	st, err := mgr.Get(r.Context(), threadID)
	if err != nil {
		c.handleError(w, r, err)
		return
	}
	jsonEncode(w, mgr.ToProto(st))
}

// handleDeleteWorkspaceThread removes a thread.
//
//	@Summary		Remove thread
//	@Tags			threads
//	@Param			id				path	string	true	"Workspace ID"
//	@Param			threadID		path	string	true	"Thread ID or name"
//	@Param			force			query	boolean	false	"Force removal of an active or dirty thread"
//	@Param			delete_branch	query	boolean	false	"Also delete the thread's git branch"
//	@Success		200
//	@Failure		404	{object}	proto.Error
//	@Failure		409	{object}	proto.Error
//	@Failure		500	{object}	proto.Error
//	@Router			/workspaces/{id}/threads/{threadID} [delete]
func (c *controllerV1) handleDeleteWorkspaceThread(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	threadID := r.PathValue("threadID")
	mgr, ok := c.requireThreadManager(w, r, id)
	if !ok {
		return
	}

	if _, err := mgr.Get(r.Context(), threadID); err != nil {
		jsonError(w, http.StatusNotFound, "thread not found")
		return
	}

	force := r.URL.Query().Get("force") == "true"
	deleteBranch := r.URL.Query().Get("delete_branch") == "true"
	// Remove's remaining error modes (active without force, dirty
	// worktree without force) are state conflicts, not missing-resource
	// or malformed-request errors — the not-found case was already ruled
	// out by the Get above.
	if err := mgr.Remove(r.Context(), threadID, force, deleteBranch); err != nil {
		jsonError(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}
