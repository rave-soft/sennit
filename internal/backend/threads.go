package backend

import (
	"context"
	"log/slog"

	"github.com/rave-soft/braid/internal/app"
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

	dataDir := a.Config().Options.DataDirectory
	conn, err := db.Connect(ctx, dataDir, db.WithDataDirLock(true))
	if err != nil {
		slog.Warn("Failed to open thread store, threads unavailable", "error", err)
		return
	}
	a.AddCleanup(func(context.Context) error { return db.Release(dataDir) })

	worktreeDir := ""
	if opts := a.Config().Options.Threads; opts != nil {
		worktreeDir = opts.WorktreeDir
	}
	mgr := thread.NewManager(thread.ManagerOptions{
		Store:       thread.NewStore(db.New(conn)),
		Spawner:     b.ThreadSpawner(),
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
