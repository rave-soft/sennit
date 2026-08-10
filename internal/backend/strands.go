package backend

import (
	"context"
	"log/slog"

	"github.com/rave-soft/braid/internal/app"
	"github.com/rave-soft/braid/internal/db"
	"github.com/rave-soft/braid/internal/git"
	"github.com/rave-soft/braid/internal/strand"
)

// attachServerStrands mirrors internal/cmd/strands.go's attachLocalStrands
// for the client/server backend: it gives a newly created workspace
// ownership of a strand manager when path is a git repository's toplevel,
// binding the manager's background goroutines to ctx (the workspace's own
// context, canceled by Workspace.Shutdown) so they stop when the
// workspace does. Best-effort: failures are logged, not returned, since a
// broken strand store should not block the workspace it would have
// belonged to from starting.
func (b *Backend) attachServerStrands(ctx context.Context, a *app.App, path string) {
	top, err := git.TopLevel(ctx, path)
	if err != nil || top != path {
		return
	}

	dataDir := a.Config().Options.DataDirectory
	conn, err := db.Connect(ctx, dataDir, db.WithDataDirLock(true))
	if err != nil {
		slog.Warn("Failed to open strand store, strands unavailable", "error", err)
		return
	}
	a.AddCleanup(func(context.Context) error { return db.Release(dataDir) })

	worktreeDir := ""
	if opts := a.Config().Options.Strands; opts != nil {
		worktreeDir = opts.WorktreeDir
	}
	mgr := strand.NewManager(strand.ManagerOptions{
		Store:       strand.NewStore(db.New(conn)),
		Spawner:     b.StrandSpawner(),
		RepoRoot:    top,
		WorktreeDir: worktreeDir,
		Context:     ctx,
	})
	if err := mgr.Recover(ctx); err != nil {
		slog.Warn("Failed to recover strand state", "error", err)
	}

	// SetStrands makes the strand_* agent tools available to the main
	// coordinator; SetStrandManager gives internal/workspace and
	// internal/server (later steps) access to the full *strand.Manager;
	// ForwardEvents fans strand lifecycle events into this workspace's
	// event stream (app.Subscribe locally, app.Events/SSE remotely).
	a.SetStrands(strand.AsAgentToolManager(mgr))
	a.SetStrandManager(mgr)
	app.ForwardEvents(a, "strand", mgr.Subscribe)
}
