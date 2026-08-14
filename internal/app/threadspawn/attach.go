package threadspawn

import (
	"context"
	"database/sql"
	"log/slog"

	"github.com/rave-soft/braid/internal/app"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/db"
	"github.com/rave-soft/braid/internal/git"
	"github.com/rave-soft/braid/internal/thread"
)

// attachDeps keeps Attach's external dependencies explicit so failure and
// cleanup ordering can be verified without process-wide test hooks.
type attachDeps struct {
	topLevel           func(context.Context, string) (string, error)
	globalDBDir        func() string
	connect            func(context.Context, string) (*sql.DB, error)
	release            func(string) error
	newManager         func(thread.ManagerOptions) *thread.Manager
	recover            func(*thread.Manager, context.Context) error
	shutdown           func(*thread.Manager, context.Context) error
	addShutdownHook    func(*app.App, func(context.Context) error) error
	addCriticalCleanup func(*app.App, func(context.Context) error) error
	forwardEvents      func(*app.App, *thread.Manager)
}

var productionAttachDeps = attachDeps{
	topLevel:    git.TopLevel,
	globalDBDir: config.GlobalDBDir,
	connect:     db.Connect,
	release:     db.Release,
	newManager:  thread.NewManager,
	recover: func(mgr *thread.Manager, ctx context.Context) error {
		return mgr.Recover(ctx)
	},
	shutdown: func(mgr *thread.Manager, ctx context.Context) error {
		return mgr.Shutdown(ctx)
	},
	addShutdownHook: func(a *app.App, fn func(context.Context) error) error {
		return a.AddShutdownHook(fn)
	},
	addCriticalCleanup: func(a *app.App, fn func(context.Context) error) error {
		return a.AddCriticalCleanup(fn)
	},
	forwardEvents: func(a *app.App, mgr *thread.Manager) {
		app.ForwardEvents(a, "thread", mgr.Subscribe)
	},
}

// AttachDeps is the [attachDeps] type, exported (as a type alias) so tests
// outside this package can build a production deps value and override
// individual fields the same way this package's own tests do.
type AttachDeps = attachDeps

// ProductionAttachDeps returns the production dependency set, for tests
// that want the real wiring and then override one or two of it.
func ProductionAttachDeps() attachDeps { return productionAttachDeps }

// AttachWithDeps runs [Attach] with an explicit dependency set instead of
// the production one. It exists so tests in other packages can verify
// Attach's failure and cleanup ordering without process-wide test hooks;
// production callers use [Attach] and never this.
func AttachWithDeps(ctx context.Context, a *app.App, path string, spawner thread.Spawner, deps attachDeps) {
	attachWithDeps(ctx, a, path, spawner, deps)
}

// Attach gives a workspace ownership of a thread manager when path is a git
// repository's toplevel. It is deliberately best-effort: a non-repository or
// an unavailable thread store must not prevent the parent workspace starting.
//
// The caller provides the Spawner because it owns the lifetime policy for a
// thread workspace: local mode creates in-process apps, while backend mode
// keeps backend workspaces held for the thread's lifetime.
func Attach(ctx context.Context, a *app.App, path string, spawner thread.Spawner) {
	attachWithDeps(ctx, a, path, spawner, productionAttachDeps)
}

func attachWithDeps(ctx context.Context, a *app.App, path string, spawner thread.Spawner, deps attachDeps) {
	top, err := deps.topLevel(ctx, path)
	if err != nil || top != path {
		return
	}

	dbDir := deps.globalDBDir()
	conn, err := deps.connect(ctx, dbDir)
	if err != nil {
		slog.Warn("Failed to open thread store, threads unavailable", "error", err)
		return
	}
	worktreeDir := ""
	if opts := a.Config().Options.Threads; opts != nil {
		worktreeDir = opts.WorktreeDir
	}
	parentWorkspace := NewAppWorkspaceAdapter(a)
	mgr := deps.newManager(thread.ManagerOptions{
		Store:       NewStore(db.New(conn), a.Store().WorkingDir()),
		Spawner:     spawner,
		RepoRoot:    top,
		WorktreeDir: worktreeDir,
		DataDir:     a.Config().Options.DataDirectory,
		Context:     ctx,
		ParentApp:   parentWorkspace,
	})
	if err := deps.addShutdownHook(a, func(context.Context) error {
		return deps.shutdown(mgr, context.Background())
	}); err != nil {
		slog.Warn("Failed to register thread manager shutdown", "error", err)
		_ = deps.release(dbDir)
		return
	}
	if err := deps.addCriticalCleanup(a, func(context.Context) error {
		return deps.release(dbDir)
	}); err != nil {
		slog.Warn("Failed to register thread database cleanup", "error", err)
		_ = deps.shutdown(mgr, context.Background())
		_ = deps.release(dbDir)
		return
	}
	if err := deps.recover(mgr, ctx); err != nil {
		slog.Warn("Failed to recover thread state", "error", err)
	}

	// TaskManager shares mgr's own lifecycle and context (both unexported,
	// so only constructible from inside the thread package) rather than
	// fresh ones: that is what makes mgr's shutdown hook above, and the
	// recover call just above, already cover a task's in-flight run and
	// store row too — see thread.NewTaskManager's doc comment. Its Spawner
	// wraps a, the App being attached, so a task runs inside it instead of
	// spawning an isolated one; that Spawner's Release is a deliberate
	// no-op, so nothing here needs its own teardown registration.
	tasks := thread.NewTaskManagerForTest(mgr, NewParentAppSpawner(parentWorkspace), NewMessageService(a.Messages()))

	// Publish only once shutdown and database cleanup are both registered:
	// consumers must never observe a manager whose dependencies can leak.
	a.SetThreads(AsAgentToolManager(mgr))
	a.SetThreadManager(mgr)
	a.SetTaskManager(tasks)
	a.SetTasks(AsAgentToolTaskManager(tasks))
	deps.forwardEvents(a, mgr)
}
