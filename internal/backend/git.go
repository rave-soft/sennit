package backend

import (
	"context"

	"github.com/rave-soft/braid/internal/git"
)

// UncommittedFiles returns uncommitted files for a workspace.
func (b *Backend) UncommittedFiles(ctx context.Context, workspaceID string) ([]git.FileChange, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	return git.UncommittedFiles(ctx, ws.Cfg.WorkingDir())
}
