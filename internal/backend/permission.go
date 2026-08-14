package backend

import (
	"github.com/rave-soft/braid/internal/permission"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/thread"
)

// GrantPermission grants, denies, or persistently grants a permission
// request. The returned bool reports whether this call resolved the
// pending request (true) or found it already resolved by a previous
// caller (false). A false return is not an error.
func (b *Backend) GrantPermission(workspaceID string, req proto.PermissionGrant) (bool, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return false, err
	}

	perm := permission.PermissionRequest{
		ID:          req.Permission.ID,
		SessionID:   req.Permission.SessionID,
		ToolCallID:  req.Permission.ToolCallID,
		ToolName:    req.Permission.ToolName,
		Description: req.Permission.Description,
		Action:      req.Permission.Action,
		Params:      req.Permission.Params,
		Path:        req.Permission.Path,
		// Carried back in so the answer can be routed to the service
		// that is actually blocking on it — a thread's prompts are
		// raised inside its own isolated workspace and only relayed
		// here. Dropping it, as this conversion used to, answers the
		// wrong service and leaves the thread waiting.
		Delegation: permission.DelegationRef{
			ID:   req.Permission.Delegation.ID,
			Name: req.Permission.Delegation.Name,
			Kind: req.Permission.Delegation.Kind,
		},
	}

	svc := permissionsFor(ws, perm)
	switch req.Action {
	case proto.PermissionAllow:
		return svc.Grant(perm), nil
	case proto.PermissionAllowForSession:
		return svc.GrantPersistent(perm), nil
	case proto.PermissionDeny:
		return svc.Deny(perm), nil
	default:
		return false, ErrInvalidPermissionAction
	}
}

// permissionsFor resolves the service holding perm. See
// AppWorkspace.permissionsFor, which does the same for local mode; the two
// exist separately because each transport reaches the thread manager
// through its own workspace type.
func permissionsFor(ws *Workspace, perm permission.PermissionRequest) permission.Service {
	if perm.Delegation.ID == "" {
		return ws.Permissions
	}
	mgr, ok := ws.ThreadManager().(*thread.Manager)
	if !ok || mgr == nil {
		return ws.Permissions
	}
	if svc := mgr.PermissionsFor(perm.Delegation.ID); svc != nil {
		return svc
	}
	return ws.Permissions
}

// SetPermissionsSkip sets whether permission prompts are skipped.
func (b *Backend) SetPermissionsSkip(workspaceID string, skip bool) error {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return err
	}

	ws.SetPermissionsSkip(skip)
	return nil
}

// GetPermissionsSkip returns whether permission prompts are skipped.
func (b *Backend) GetPermissionsSkip(workspaceID string) (bool, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return false, err
	}

	return ws.Permissions.SkipRequests(), nil
}
