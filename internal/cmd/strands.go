package cmd

import (
	"context"
	"log/slog"

	"github.com/rave-soft/braid/internal/app"
	"github.com/rave-soft/braid/internal/db"
	"github.com/rave-soft/braid/internal/git"
	"github.com/rave-soft/braid/internal/strand"
)

// attachLocalStrands gives a's single-process CLI workspace ownership of a
// strand manager, when it's warranted: cwd must be a git repository's
// toplevel. A workspace opened in a subdirectory of a repo is not given
// one, since a strand's worktrees and branches are rooted at the repo
// toplevel, not at an arbitrary subdirectory a session happens to be
// running from.
//
// This is best-effort: failures are logged, not returned, so a broken
// strand store (or a non-git cwd) never blocks the workspace it would
// have belonged to from starting.
func attachLocalStrands(ctx context.Context, a *app.App, cwd string) {
	top, err := git.TopLevel(ctx, cwd)
	if err != nil || top != cwd {
		return
	}

	dataDir := a.Config().Options.DataDirectory
	conn, err := db.Connect(ctx, dataDir)
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
		Spawner:     strand.NewLocalSpawner(),
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
