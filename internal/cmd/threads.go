package cmd

import (
	"context"
	"log/slog"

	"github.com/rave-soft/braid/internal/app"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/db"
	"github.com/rave-soft/braid/internal/git"
	"github.com/rave-soft/braid/internal/thread"
)

// attachLocalThreads gives a's single-process CLI workspace ownership of a
// thread manager, when it's warranted: cwd must be a git repository's
// toplevel. A workspace opened in a subdirectory of a repo is not given
// one, since a thread's worktrees and branches are rooted at the repo
// toplevel, not at an arbitrary subdirectory a session happens to be
// running from.
//
// This is best-effort: failures are logged, not returned, so a broken
// thread store (or a non-git cwd) never blocks the workspace it would
// have belonged to from starting.
func attachLocalThreads(ctx context.Context, a *app.App, cwd string) {
	top, err := git.TopLevel(ctx, cwd)
	if err != nil || top != cwd {
		return
	}

	dbDir := config.GlobalDBDir()
	conn, err := db.Connect(ctx, dbDir)
	if err != nil {
		slog.Warn("Failed to open thread store, threads unavailable", "error", err)
		return
	}
	a.AddCleanup(func(context.Context) error { return db.Release(dbDir) })

	worktreeDir := ""
	if opts := a.Config().Options.Threads; opts != nil {
		worktreeDir = opts.WorktreeDir
	}
	mgr := thread.NewManager(thread.ManagerOptions{
		Store:       thread.NewStore(db.New(conn), a.Store().WorkingDir()),
		Spawner:     thread.NewLocalSpawner(),
		RepoRoot:    top,
		WorktreeDir: worktreeDir,
		Context:     ctx,
	})
	if err := mgr.Recover(ctx); err != nil {
		slog.Warn("Failed to recover thread state", "error", err)
	}

	// SetThreads makes the thread_* agent tools available to the main
	// coordinator; SetThreadManager gives internal/workspace and
	// internal/server (later steps) access to the full *thread.Manager;
	// ForwardEvents fans thread lifecycle events into this workspace's
	// event stream (app.Subscribe locally, app.Events/SSE remotely).
	a.SetThreads(thread.AsAgentToolManager(mgr))
	a.SetThreadManager(mgr)
	app.ForwardEvents(a, "thread", mgr.Subscribe)
}
