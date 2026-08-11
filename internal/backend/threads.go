package backend

import (
	"context"
	"log/slog"

	"github.com/rave-soft/braid/internal/app"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/db"
	"github.com/rave-soft/braid/internal/git"
	"github.com/rave-soft/braid/internal/thread"
)

// attachServerThreads mirrors internal/cmd/threads.go's attachLocalThreads
// for the client/server backend: it gives a newly created workspace
// ownership of a thread manager when path is a git repository's toplevel,
// binding the manager's background goroutines to ctx (the workspace's own
// context, canceled by Workspace.Shutdown) so they stop when the
// workspace does. Best-effort: failures are logged, not returned, since a
// broken thread store should not block the workspace it would have
// belonged to from starting.
func (b *Backend) attachServerThreads(ctx context.Context, a *app.App, path string) {
	top, err := git.TopLevel(ctx, path)
	if err != nil || top != path {
		return
	}

	// This opens a second connection to the shared global database the
	// main workspace already connected via app.Bootstrap, so it does not
	// need its own lock acquisition here.
	dbDir := config.GlobalDBDir()
	conn, err := db.Connect(ctx, dbDir)
	if err != nil {
		slog.Warn("Failed to open thread store, threads unavailable", "error", err)
		return
	}
	worktreeDir := ""
	if opts := a.Config().Options.Threads; opts != nil {
		worktreeDir = opts.WorktreeDir
	}
	mgr := thread.NewManager(thread.ManagerOptions{
		Store:       thread.NewStore(db.New(conn), a.Store().WorkingDir()),
		Spawner:     b.ThreadSpawner(),
		RepoRoot:    top,
		WorktreeDir: worktreeDir,
		Context:     ctx,
	})
	a.AddCleanup(func(context.Context) error {
		// App cleanup functions run concurrently. Do not close this DB
		// connection until the manager has joined every DB-writing worker.
		if err := mgr.Shutdown(context.Background()); err != nil {
			return err
		}
		return db.Release(dbDir)
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
