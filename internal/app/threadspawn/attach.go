package threadspawn

import (
	"context"
	"database/sql"
	"log/slog"
	"reflect"

	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/git"
	"github.com/rave-soft/sennit/internal/log"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/thread"
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
	addPreCleanupHook  func(*app.App, func(context.Context) error) error
	forwardEvents      func(*app.App, *thread.Manager)
	finalizeTurns      func(context.Context, *app.App, *thread.Manager)
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
	addPreCleanupHook: func(a *app.App, fn func(context.Context) error) error {
		return a.AddPreCleanupHook(fn)
	},
	forwardEvents: func(a *app.App, mgr *thread.Manager) {
		app.ForwardEvents(a, "thread", mgr.Subscribe)
	},
	finalizeTurns: finalizeThreadTurns,
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
// thread workspace: [LocalSpawner] bootstraps a fresh in-process app per
// thread worktree, while [ParentAppSpawner] hands every spawned handle the
// caller's own already-running workspace (used for worktree-less tasks).
func Attach(ctx context.Context, a *app.App, path string, spawner thread.Spawner) {
	attachWithDeps(ctx, a, path, spawner, productionAttachDeps)
}

func attachWithDeps(ctx context.Context, a *app.App, path string, spawner thread.Spawner, deps attachDeps) {
	top, gitErr := deps.topLevel(ctx, path)
	isGitWorkspace := gitErr == nil && fsext.Canonical(top) == fsext.Canonical(path)
	if gitErr == nil && !isGitWorkspace {
		return
	}
	if !isGitWorkspace {
		// Tasks are workspace delegations, not git worktree operations. Keep
		// their lifecycle available in directories that are not repositories;
		// only the thread/worktree overlay below is repository-specific.
		top = path
	}
	// git and the caller can spell the same directory differently — git
	// prints its own resolved form, while path may come from t.TempDir (an
	// 8.3 short name on Windows) or an unresolved symlink. A raw string
	// compare would then say "not the root" for a directory that is,
	// which silently drops the thread manager for a workspace that should
	// have one. Compare canonical spellings instead.
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
		Store:       NewTransactionalStore(conn, a.Store().WorkingDir()),
		Spawner:     spawner,
		RepoRoot:    top,
		WorktreeDir: worktreeDir,
		DataDir:     a.Config().Options.DataDirectory,
		Context:     ctx,
		ParentApp:   parentWorkspace,
	})
	// The hook's own context, not a fresh root: it is how long this
	// caller waits, and thread.Manager.Shutdown counts its waiters -
	// when the last one gives up it cancels the teardown's own context,
	// which is what stops the manager writing terminal statuses to a
	// store the process is about to leave behind. Handed
	// context.Background() the wait never ends, so the manager was never
	// told to stop and the goroutine that runs shutdown callbacks was
	// left behind still working while the app tore down around it.
	// (app.runShutdownCallback stops waiting on its own deadline either
	// way, so this was never able to hang shutdown - it just made the
	// abandonment path unreachable from the only caller that has one.)
	if err := deps.addShutdownHook(a, func(ctx context.Context) error {
		return deps.shutdown(mgr, ctx)
	}); err != nil {
		slog.Warn("Failed to register thread manager shutdown", "error", err)
		_ = deps.release(dbDir)
		return
	}
	if err := deps.addCriticalCleanup(a, func(context.Context) error {
		return deps.release(dbDir)
	}); err != nil {
		slog.Warn("Failed to register thread database cleanup", "error", err)
		// attach's own context bounds this unwind. A fresh root here had
		// nothing to stop it: this call blocks until the manager's
		// teardown finishes, and attach is on the caller's goroutine
		// with no deadline of its own to fall back on.
		_ = deps.shutdown(mgr, ctx)
		_ = deps.release(dbDir)
		return
	}
	if err := deps.recover(mgr, ctx); err != nil {
		slog.Warn("Failed to recover thread state", "error", err)
	}
	if isGitWorkspace {
		deps.finalizeTurns(ctx, a, mgr)
	}

	// TaskManager shares mgr's own lifecycle and context (both unexported,
	// so only constructible from inside the thread package) rather than
	// fresh ones: that is what makes mgr's shutdown hook above, and the
	// recover call just above, already cover a task's in-flight run and
	// store row too — see thread.NewTaskManager's doc comment. Its Spawner
	// wraps a, the App being attached, so a task runs inside it instead of
	// spawning an isolated one; that Spawner's Release is a deliberate
	// no-op, so nothing here needs its own teardown registration.
	tasks := thread.NewTaskManagerFromManager(mgr, NewParentAppSpawner(parentWorkspace), NewMessageService(a.Messages()))

	// Publish only once shutdown and database cleanup are both registered:
	// consumers must never observe a manager whose dependencies can leak.
	//
	// Unconditional, unlike the git-only blocks around it: mgr's broker is
	// the same event source tasks publish onto (TaskManager shares mgr's
	// own lifecycle, see the comment above), and a task has no worktree
	// requirement, so it runs in a non-git workspace too. Gating this on
	// isGitWorkspace would silently drop task status from the UI there.
	// Thread-kind events can never appear on this broker in a non-git
	// workspace: threadMgr and threadTools, the only way to reach mgr's
	// thread-creating methods, stay nil below when !isGitWorkspace.
	deps.forwardEvents(a, mgr)
	var threadMgr *thread.Manager
	var threadTools tools.ThreadManager
	if isGitWorkspace {
		threadMgr = mgr
		threadTools = AsAgentToolManager(mgr)
	}
	a.SetDelegationManagers(threadMgr, tasks, threadTools, AsAgentToolTaskManager(tasks))
	if isGitWorkspace {
		if local, ok := spawner.(*LocalSpawner); ok {
			// Bound to a's own shutdown, not ctx (the command context):
			// parent.Skills.SubscribeEvents(ctx) does not close when the
			// App's broker shuts down the way parent.Events(ctx) does, so a
			// forwarder started on ctx outlives the App it forwards for —
			// see internal/app/watch.go's startExternalChangeWatchers,
			// which registers the same kind of pre-cleanup hook for the
			// config/skills watchers.
			forwardCtx, cancel := context.WithCancel(ctx)
			if err := deps.addPreCleanupHook(a, func(context.Context) error {
				cancel()
				return nil
			}); err != nil {
				slog.Warn("Failed to register thread forwarder shutdown", "error", err)
				cancel()
				return
			}
			go forwardSkillsToThreads(forwardCtx, a, local)
			go forwardAgentsToThreads(forwardCtx, a, local)
		}
	}
}

// forwardAgentsToThreads pushes the parent workspace's user-defined agents
// into every live thread whenever the parent's config reloads with a
// different set — the agents counterpart of forwardSkillsToThreads, and
// for the same reason: a thread inherits its agents at spawn (see
// LocalSpawner.Spawn / BootstrapOptions.InheritedAgents) from a directory
// its own watcher never looks at. Without this, editing an agent in the
// parent (its model, say) only reached threads started after the edit; a
// thread already running kept delegating to the agent it was born with —
// on the old model — while the parent's config, and anything reading it,
// already showed the new one.
//
// Only a changed set is pushed: every push republishes the thread's
// Config and forces its runtime to recompile before the next turn, and
// most config reloads have nothing to do with agents.
func forwardAgentsToThreads(ctx context.Context, parent *app.App, spawner *LocalSpawner) {
	defer log.RecoverPanic("threadspawn.forwardAgentsToThreads", func() {})
	events := parent.Events(ctx)
	last := parent.Config().UserAgents()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if _, changed := ev.Payload.(pubsub.Event[app.WorkspaceChanged]); !changed {
				continue
			}
			inherited := parent.Config().UserAgents()
			if reflect.DeepEqual(inherited, last) {
				continue
			}
			last = inherited
			threadApps := spawner.Apps()
			for _, threadApp := range threadApps {
				threadApp.Store().ReplaceInheritedAgents(inherited)
			}
			if len(threadApps) > 0 {
				slog.Info("Pushed agent update to threads",
					"component", "agents", "threads", len(threadApps), "agents", len(inherited))
			}
		}
	}
}

// forwardSkillsToThreads pushes the parent workspace's skills into every
// live thread whenever the parent re-discovers them.
//
// A thread cannot see the edit itself: its skills were inherited at spawn
// (see skills.DiscoveryConfig.InheritedSkills) and the file that changed
// lives outside the worktree, where the thread's own watcher neither looks
// nor should. Without this, an edited SKILL.md only reached threads
// started after the edit, and a thread running at the time kept the
// version it was born with until it finished.
func forwardSkillsToThreads(ctx context.Context, parent *app.App, spawner *LocalSpawner) {
	defer log.RecoverPanic("threadspawn.forwardSkillsToThreads", func() {})
	if parent.Skills == nil {
		return
	}
	events := parent.Skills.SubscribeEvents(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-events:
			if !ok {
				return
			}
			inherited := skills.Inheritable(parent.Skills.AllSkills())
			threadApps := spawner.Apps()
			for _, threadApp := range threadApps {
				if threadApp.Skills == nil {
					continue
				}
				// Replace the stored set first: the thread's own watcher
				// discovers against it, so a later local change must not
				// resurrect the pre-edit copy.
				threadApp.Skills.ReplaceInherited(inherited)
				all, active, states := skills.DiscoverFromConfig(threadSkillsConfig(threadApp, inherited))
				threadApp.Skills.ReplaceDiscovery(all, active, states)
				if coord := threadApp.Coordinator(); coord != nil {
					coord.RefreshSkills(all, active)
				}
			}
			if len(threadApps) > 0 {
				slog.Info("Pushed skill update to threads",
					"component", "skills", "threads", len(threadApps), "skills", len(inherited))
			}
		}
	}
}

// threadSkillsConfig rebuilds a thread workspace's discovery config with a
// fresh inherited set.
func threadSkillsConfig(threadApp *app.App, inherited []*skills.Skill) skills.DiscoveryConfig {
	cfg := app.SkillsDiscoveryConfig(threadApp.Store())
	cfg.InheritedSkills = inherited
	return cfg
}

// finalizeThreadTurns closes out the turns a killed process left mid-flight
// inside this project's thread worktrees, the same way app.Bootstrap does
// for the workspace it starts.
//
// Bootstrap's own sweep cannot reach them. It scopes by project path, and a
// thread's sessions — its own and every sub-agent's — are recorded under
// the thread's worktree, not under the repository the parent workspace was
// started in. The thread's App sweeps them when it is spawned, which covers
// every thread that gets reactivated; a thread that is never reactivated,
// or that cannot be (see appws.AppWorkspace.AttachThread's read-only
// fallback), keeps a transcript full of tool calls that never came back.
// The UI reads that shape as still running, so the thread sits there
// spinning "Waiting for tool response..." forever, across every restart,
// for work that ended when a process died hours ago.
//
// Safe here for the same reason it is safe in Bootstrap, and for one more.
// Threads were recovered on the line above and none of them is running in
// this process yet. And a thread worktree does not have a workspace lock of
// its own: it locks its repository's git common directory, the very lock
// the workspace being attached holds — so no other sennit is running turns
// in these worktrees either.
//
// Best-effort throughout: this repairs the record of work already over, and
// nothing about it is worth failing an attach for.
func finalizeThreadTurns(ctx context.Context, a *app.App, mgr *thread.Manager) {
	// The paragraph above rests on the attached workspace holding the
	// repository's lock. It may not: SENNIT_SKIP_DATADIR_LOCK makes
	// acquisition a no-op that excludes nobody, and then a second sennit
	// can be mid-turn in these very worktrees - where this would stamp
	// error tool results and a canceled finish onto its live message.
	if !a.WorkspaceLockEnforced() {
		slog.Warn("Skipping interrupted-turn cleanup in thread worktrees: no enforced workspace lock")
		return
	}
	threads, err := mgr.List(ctx)
	if err != nil {
		slog.Warn("Failed to list threads while closing out interrupted turns", "error", err)
		return
	}
	for _, st := range threads {
		// Kinds that share their parent's workspace (a task) have no
		// worktree of their own, and their sessions are under the parent's
		// project path, which Bootstrap already swept.
		if st.WorktreePath == "" {
			continue
		}
		if err := app.FinalizeInterruptedTurns(ctx, st.WorktreePath, a.Messages()); err != nil {
			slog.Warn("Failed to close out interrupted turns in a thread worktree",
				"thread", st.ID, "worktree", st.WorktreePath, "error", err)
		}
	}
}
