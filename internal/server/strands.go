package server

import (
	"encoding/json"
	"net/http"

	"github.com/rave-soft/braid/internal/backend"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/strand"
)

// workspaceStrandManager resolves ws (already looked up by the caller) to
// its concrete *strand.Manager. Returns (nil, false) when the workspace has
// no strand manager (non-git workspace, or a strand's own nested
// workspace) — callers turn that into a 409: the workspace exists and is
// otherwise fine, it just doesn't support strands, which is different from
// a 404 (workspace/strand not found).
func workspaceStrandManager(ws *backend.Workspace) (*strand.Manager, bool) {
	if ws == nil || ws.App == nil {
		return nil, false
	}
	mgr, ok := ws.StrandManager().(*strand.Manager)
	return mgr, ok && mgr != nil
}

// requireStrandManager looks up id's workspace and its strand manager,
// writing the appropriate error response (404/409) and returning ok=false
// on failure. Shared by every handler in this file.
func (c *controllerV1) requireStrandManager(w http.ResponseWriter, r *http.Request, id string) (*strand.Manager, bool) {
	ws, err := c.backend.GetWorkspace(id)
	if err != nil {
		c.handleError(w, r, err)
		return nil, false
	}
	mgr, ok := workspaceStrandManager(ws)
	if !ok {
		jsonError(w, http.StatusConflict, "workspace does not support strands")
		return nil, false
	}
	return mgr, true
}

// handleGetWorkspaceStrands lists strands for a workspace.
//
//	@Summary		List strands
//	@Tags			strands
//	@Produce		json
//	@Param			id	path		string			true	"Workspace ID"
//	@Success		200	{array}		proto.Strand
//	@Failure		404	{object}	proto.Error
//	@Failure		409	{object}	proto.Error
//	@Failure		500	{object}	proto.Error
//	@Router			/workspaces/{id}/strands [get]
func (c *controllerV1) handleGetWorkspaceStrands(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	mgr, ok := c.requireStrandManager(w, r, id)
	if !ok {
		return
	}
	strands, err := mgr.List(r.Context())
	if err != nil {
		c.handleError(w, r, err)
		return
	}
	result := make([]proto.Strand, len(strands))
	for i, st := range strands {
		result[i] = mgr.ToProto(st)
	}
	jsonEncode(w, result)
}

// handlePostWorkspaceStrands creates a new strand.
//
//	@Summary		Create strand
//	@Tags			strands
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string						true	"Workspace ID"
//	@Param			request	body		proto.CreateStrandRequest	true	"Strand creation params"
//	@Success		200		{object}	proto.Strand
//	@Failure		400		{object}	proto.Error
//	@Failure		404		{object}	proto.Error
//	@Failure		409		{object}	proto.Error
//	@Failure		500		{object}	proto.Error
//	@Router			/workspaces/{id}/strands [post]
func (c *controllerV1) handlePostWorkspaceStrands(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	mgr, ok := c.requireStrandManager(w, r, id)
	if !ok {
		return
	}

	var req proto.CreateStrandRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		c.server.logError(r, "Failed to decode request", "error", err)
		jsonError(w, http.StatusBadRequest, "failed to decode request")
		return
	}

	st, err := mgr.Create(r.Context(), strand.CreateArgs{
		Name:        req.Name,
		Goal:        req.Goal,
		BaseBranch:  req.BaseBranch,
		MergePolicy: strand.MergePolicy(req.MergePolicy),
	})
	if err != nil {
		// Manager.Create's errors are all validation-shaped (invalid
		// name, name already in use, branch already exists, resolve
		// base branch, ...) rather than "not found" or an ID conflict,
		// so a plain 400 covers them.
		c.server.logError(r, "Failed to create strand", "error", err)
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	jsonEncode(w, mgr.ToProto(st))
}

// handleGetWorkspaceStrand returns a single strand.
//
//	@Summary		Get strand
//	@Tags			strands
//	@Produce		json
//	@Param			id			path		string	true	"Workspace ID"
//	@Param			strandID	path		string	true	"Strand ID or name"
//	@Success		200			{object}	proto.Strand
//	@Failure		404			{object}	proto.Error
//	@Failure		409			{object}	proto.Error
//	@Failure		500			{object}	proto.Error
//	@Router			/workspaces/{id}/strands/{strandID} [get]
func (c *controllerV1) handleGetWorkspaceStrand(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	strandID := r.PathValue("strandID")
	mgr, ok := c.requireStrandManager(w, r, id)
	if !ok {
		return
	}

	st, err := mgr.Get(r.Context(), strandID)
	if err != nil {
		// Get/resolve's only failure mode is "no such ID/name" (see
		// internal/strand.Manager.resolve): it tries the store by ID,
		// falls back to name, and returns the name lookup's error. Treat
		// any error here as not-found rather than trying to distinguish
		// further.
		jsonError(w, http.StatusNotFound, "strand not found")
		return
	}
	jsonEncode(w, mgr.ToProto(st))
}

// handlePostWorkspaceStrandSend sends a follow-up message to a strand.
//
//	@Summary		Send message to strand
//	@Tags			strands
//	@Accept			json
//	@Param			id			path	string						true	"Workspace ID"
//	@Param			strandID	path	string						true	"Strand ID or name"
//	@Param			request		body	proto.SendStrandRequest	true	"Message to send"
//	@Success		200
//	@Failure		400	{object}	proto.Error
//	@Failure		404	{object}	proto.Error
//	@Failure		409	{object}	proto.Error
//	@Failure		500	{object}	proto.Error
//	@Router			/workspaces/{id}/strands/{strandID}/send [post]
func (c *controllerV1) handlePostWorkspaceStrandSend(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	strandID := r.PathValue("strandID")
	mgr, ok := c.requireStrandManager(w, r, id)
	if !ok {
		return
	}

	var req proto.SendStrandRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		c.server.logError(r, "Failed to decode request", "error", err)
		jsonError(w, http.StatusBadRequest, "failed to decode request")
		return
	}

	if _, err := mgr.Get(r.Context(), strandID); err != nil {
		jsonError(w, http.StatusNotFound, "strand not found")
		return
	}
	if err := mgr.Send(r.Context(), strandID, req.Message); err != nil {
		c.server.logError(r, "Failed to send to strand", "error", err)
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handlePostWorkspaceStrandMerge merges (or retries merging) a strand.
//
//	@Summary		Merge strand
//	@Tags			strands
//	@Produce		json
//	@Param			id			path		string	true	"Workspace ID"
//	@Param			strandID	path		string	true	"Strand ID or name"
//	@Success		200			{object}	proto.Strand
//	@Failure		404			{object}	proto.Error
//	@Failure		409			{object}	proto.Error
//	@Failure		500			{object}	proto.Error
//	@Router			/workspaces/{id}/strands/{strandID}/merge [post]
func (c *controllerV1) handlePostWorkspaceStrandMerge(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	strandID := r.PathValue("strandID")
	mgr, ok := c.requireStrandManager(w, r, id)
	if !ok {
		return
	}

	if _, err := mgr.Get(r.Context(), strandID); err != nil {
		jsonError(w, http.StatusNotFound, "strand not found")
		return
	}
	// Conflict/merge-blocked outcomes are recorded as strand statuses,
	// not returned as Go errors (Merge returns nil in those cases) — see
	// internal/strand.Manager.mergeAttempt. A non-nil error here means
	// something more fundamental went wrong (e.g. a git command itself
	// failed to run), which is a server-side failure, not a client one.
	if err := mgr.Merge(r.Context(), strandID); err != nil {
		c.server.logError(r, "Failed to merge strand", "error", err)
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	st, err := mgr.Get(r.Context(), strandID)
	if err != nil {
		c.handleError(w, r, err)
		return
	}
	jsonEncode(w, mgr.ToProto(st))
}

// handleDeleteWorkspaceStrand removes a strand.
//
//	@Summary		Remove strand
//	@Tags			strands
//	@Param			id				path	string	true	"Workspace ID"
//	@Param			strandID		path	string	true	"Strand ID or name"
//	@Param			force			query	boolean	false	"Force removal of an active or dirty strand"
//	@Param			delete_branch	query	boolean	false	"Also delete the strand's git branch"
//	@Success		200
//	@Failure		404	{object}	proto.Error
//	@Failure		409	{object}	proto.Error
//	@Failure		500	{object}	proto.Error
//	@Router			/workspaces/{id}/strands/{strandID} [delete]
func (c *controllerV1) handleDeleteWorkspaceStrand(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	strandID := r.PathValue("strandID")
	mgr, ok := c.requireStrandManager(w, r, id)
	if !ok {
		return
	}

	if _, err := mgr.Get(r.Context(), strandID); err != nil {
		jsonError(w, http.StatusNotFound, "strand not found")
		return
	}

	force := r.URL.Query().Get("force") == "true"
	deleteBranch := r.URL.Query().Get("delete_branch") == "true"
	// Remove's remaining error modes (active without force, dirty
	// worktree without force) are state conflicts, not missing-resource
	// or malformed-request errors — the not-found case was already ruled
	// out by the Get above.
	if err := mgr.Remove(r.Context(), strandID, force, deleteBranch); err != nil {
		jsonError(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}
