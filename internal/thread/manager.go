// Package thread implements threads: parallel agent work streams, each
// running in its own git worktree and branch with a fully isolated
// workspace (own .braid data directory, database, and agent
// coordinator), and by default auto-merged back into its base branch on
// completion.
package thread

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rave-soft/braid/internal/git"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/session"
)

// nameRe restricts thread names to values safe to embed in a branch name
// and a worktree directory: lowercase alphanumeric slugs, hyphen
// separated, not leading/trailing with a hyphen.
var nameRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)

// CreateArgs holds the inputs to [Manager.Create]. BaseBranch defaults to
// the repository's currently checked-out branch when empty; MergePolicy
// defaults to [MergeAuto].
type CreateArgs struct {
	Name            string
	Goal            string
	BaseBranch      string
	MergePolicy     MergePolicy
	ParentSessionID string
}

// ManagerOptions holds the dependencies and tunables for [NewManager].
type ManagerOptions struct {
	Store Store
	// Spawner bootstraps and tears down each thread's isolated
	// workspace.
	Spawner Spawner
	// RepoRoot is the top-level directory of the git repository hosting
	// threads. The caller is responsible for constructing a Manager
	// only for git-toplevel workspaces.
	RepoRoot string
	// WorktreeDir is the parent directory under which each thread's
	// worktree is created (at WorktreeDir/<name>). Empty defaults to a
	// "<repo>-threads" sibling of RepoRoot; a relative value is resolved
	// against RepoRoot's parent directory (the same place the default
	// lives), not against the process's working directory; an absolute
	// value is used as-is.
	WorktreeDir string
	// Context is the base context background thread goroutines (agent
	// runs, RunComplete watchers) are bound to. Defaults to
	// context.Background().
	Context context.Context
}

// Manager is the core of the threads feature: it drives thread creation,
// dispatches and tracks each thread's agent run in its isolated
// workspace, and folds completed work back into the base branch. The
// generic admission, per-entity serialization, worker tracking, and event
// plumbing it needs to do that live in the [lifecycle] it holds; Manager
// itself is the git/merge-specific overlay on top.
type Manager struct {
	store       Store
	spawner     Spawner
	repoRoot    string
	worktreeDir string
	ctx         context.Context
	cancel      context.CancelFunc

	lc *lifecycle

	shutdownOnce    sync.Once
	shutdownStarted chan struct{}
	shutdownDone    chan struct{}
}

// NewManager constructs a Manager. Callers must only do so for
// git-toplevel workspaces; the manager itself does not verify RepoRoot is
// a repository.
func NewManager(opts ManagerOptions) *Manager {
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	worktreeDir := opts.WorktreeDir
	switch {
	case worktreeDir == "":
		worktreeDir = filepath.Join(filepath.Dir(opts.RepoRoot), filepath.Base(opts.RepoRoot)+"-threads")
	case !filepath.IsAbs(worktreeDir):
		worktreeDir = filepath.Join(filepath.Dir(opts.RepoRoot), worktreeDir)
	}
	m := &Manager{
		store:           opts.Store,
		spawner:         opts.Spawner,
		repoRoot:        opts.RepoRoot,
		worktreeDir:     worktreeDir,
		ctx:             ctx,
		shutdownStarted: make(chan struct{}),
		shutdownDone:    make(chan struct{}),
	}
	// onAutoMerge/recoverWorktree are this package's git/merge overlay on
	// the generic lifecycle; a lighter, worktree-less delegation kind
	// would pass nil for both. A TaskManager sharing this lifecycle (see
	// NewTaskManager) must be constructed with this same m.lc and m.ctx,
	// not fresh ones, or recovery and shutdown would only ever see threads.
	m.lc = newLifecycle(opts.Store, m.onAutoMerge, m.recoverWorktree)
	m.ctx, m.cancel = context.WithCancel(ctx)
	return m
}

// Subscribe returns a per-caller channel of thread lifecycle events.
func (m *Manager) Subscribe(ctx context.Context) <-chan pubsub.Event[Event] {
	return m.lc.subscribe(ctx)
}

// List returns every known thread.
func (m *Manager) List(ctx context.Context) ([]Thread, error) {
	return m.store.List(ctx)
}

// Get resolves idOrName (an ID or a name) to a thread.
func (m *Manager) Get(ctx context.Context, idOrName string) (Thread, error) {
	return m.resolve(ctx, idOrName)
}

// resolve looks a thread up by ID first, falling back to name.
func (m *Manager) resolve(ctx context.Context, idOrName string) (Thread, error) {
	if st, err := m.store.Get(ctx, idOrName); err == nil {
		return st, nil
	}
	return m.store.GetByName(ctx, idOrName)
}

// Create validates and dispatches a new thread: it records the thread,
// creates its git worktree and branch, spawns its isolated workspace, and
// launches its goal prompt in the background. It returns once the thread
// is running (or has failed to get there); it does not wait for the
// agent run itself to finish — subscribe or use [Manager.Wait] for that.
//
// An empty Goal creates the thread without dispatching anything: the
// worktree, branch, session, and workspace are set up and the thread
// rests at [StatusIdle], ready to be attached to and driven by hand. Both
// callers already treat the goal as optional (the CLI's --goal flag, the
// TUI's create dialog), and isolating work you intend to do yourself is
// the point of that path — dispatching an empty prompt would only fail
// agent validation and strand the thread at [StatusFailed] with a
// worktree on disk.
func (m *Manager) Create(ctx context.Context, args CreateArgs) (Thread, error) {
	done, err := m.lc.beginOp()
	if err != nil {
		return Thread{}, err
	}
	defer done()
	name, err := validateName(args.Name)
	if err != nil {
		return Thread{}, err
	}
	if _, err := m.store.GetByName(ctx, name); err == nil {
		return Thread{}, fmt.Errorf("thread: name %q is already in use", name)
	}

	base := args.BaseBranch
	if base == "" {
		base, err = git.CurrentBranch(ctx, m.repoRoot)
		if err != nil {
			return Thread{}, fmt.Errorf("thread: resolve base branch: %w", err)
		}
	}

	branch := "thread/" + name
	if exists, err := git.BranchExists(ctx, m.repoRoot, branch); err != nil {
		return Thread{}, fmt.Errorf("thread: check branch: %w", err)
	} else if exists {
		return Thread{}, fmt.Errorf("thread: branch %q already exists", branch)
	}

	mergePolicy := args.MergePolicy
	if mergePolicy == "" {
		mergePolicy = MergeAuto
	}
	worktreePath := filepath.Join(m.worktreeDir, name)

	st, err := m.store.Create(ctx, CreateParams{
		Name:         name,
		Goal:         args.Goal,
		BaseBranch:   base,
		Branch:       branch,
		WorktreePath: worktreePath,
		MergePolicy:  mergePolicy,
	})
	if err != nil {
		return Thread{}, fmt.Errorf("thread: create record: %w", err)
	}
	m.lc.publish(EventCreated, st)

	// The thread is resolvable from here on, so a concurrent Remove can
	// race the rest of creation. Hold the per-thread lifecycle lock across
	// worktree/spawn/startRun: Remove takes the same lock, so it either
	// runs before this point (nothing beyond the row exists yet) or waits
	// until the runtime is fully installed and tears it down normally.
	c := m.lc.control(st.ID)
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.mu.Lock()
	removed := c.removed
	c.mu.Unlock()
	if removed {
		return Thread{}, fmt.Errorf("thread: %q was removed during creation", name)
	}

	if err := git.WorktreeAdd(ctx, m.repoRoot, worktreePath, branch, base); err != nil {
		return Thread{}, m.failCreate(ctx, st, err)
	}

	handle, err := m.spawner.Spawn(m.ctx, worktreePath)
	if err != nil {
		_ = git.WorktreeRemove(ctx, m.repoRoot, worktreePath, true)
		return Thread{}, m.failCreate(ctx, st, err)
	}
	if err := m.ctx.Err(); err != nil {
		m.abortSpawn(context.Background(), handle, worktreePath)
		return Thread{}, err
	}

	var sess session.Session
	if args.ParentSessionID == "" {
		sess, err = handle.App().Sessions.Create(ctx, args.Goal)
	} else {
		sess, err = handle.App().Sessions.CreateTaskSession(ctx, uuid.NewString(), args.ParentSessionID, args.Goal)
	}
	if err != nil {
		m.abortSpawn(ctx, handle, worktreePath)
		return Thread{}, m.failCreate(ctx, st, err)
	}

	st, err = m.store.SetSession(ctx, st.ID, sess.ID)
	if err != nil {
		m.abortSpawn(ctx, handle, worktreePath)
		return Thread{}, m.failCreate(ctx, st, err)
	}

	if args.Goal == "" {
		st, err = m.lc.setStatus(ctx, st.ID, StatusIdle, "", "", 0)
		if err != nil {
			m.abortSpawn(ctx, handle, worktreePath)
			return Thread{}, err
		}
		// Keep the workspace live so attaching to the thread lands in a
		// writable session rather than the read-only view reserved for
		// unspawned threads. Shutdown and Remove release it like any
		// other runtime.
		m.lc.installRuntime(m.ctx, handle, m.spawner, st.ID)
		return st, nil
	}

	st, err = m.lc.setStatus(ctx, st.ID, StatusRunning, "", "", 0)
	if err != nil {
		m.abortSpawn(ctx, handle, worktreePath)
		return Thread{}, err
	}
	m.lc.startRun(m.ctx, handle, m.spawner, st.ID, st.SessionID, args.Goal)

	return st, nil
}

// abortSpawn tears a just-spawned workspace and worktree back down after a
// failure partway through Create. Errors are logged, not returned: the
// caller already has the error that triggered the rollback and that is
// what gets surfaced.
func (m *Manager) abortSpawn(ctx context.Context, handle Handle, worktreePath string) {
	if err := m.spawner.Release(ctx, handle.ID()); err != nil {
		slog.Error("thread: release spawner handle during create rollback failed", "error", err)
	}
	if err := git.WorktreeRemove(ctx, m.repoRoot, worktreePath, true); err != nil {
		slog.Error("thread: worktree removal during create rollback failed", "error", err)
	}
}

// failCreate records cause as the thread's terminal failure and returns it
// to Create's caller.
func (m *Manager) failCreate(ctx context.Context, st Thread, cause error) error {
	if _, err := m.lc.setStatus(ctx, st.ID, StatusFailed, cause.Error(), "", 0); err != nil {
		slog.Error("thread: recording create failure failed", "thread", st.ID, "error", err)
	}
	return cause
}

// onAutoMerge is the [runCompleteHook] that gives MergeAuto threads their
// merge-instead-of-completed treatment: on a successful run it folds the
// thread's branch back into its base branch rather than letting the
// generic lifecycle rest the thread at StatusCompleted. Manual-policy
// threads decline (return false) and get the generic StatusCompleted
// write.
//
// Auto-merge threads go straight from running into the merge flow without
// resting at "completed" in between: setting a terminal "completed"
// status first would give [Manager.Wait] a window where it observes a
// non-active status and returns before the merge that is about to start
// has even begun. That is what the goroutine handoff below preserves —
// handleRunComplete calls this method with the thread's opMu held, so the
// merge itself cannot start until this method (and the handleRunComplete
// call it returns to) release it; until then the thread is still
// StatusRunning, which [Status.Active] reports as active, so Wait never
// observes anything but an active status between the run finishing and
// mergeAttempt setting StatusMerging.
func (m *Manager) onAutoMerge(ctx context.Context, c *threadControl, st Thread, resultText string) bool {
	// Merging makes no sense for a delegation kind with no worktree.
	// store.Create never defaults a non-thread's MergePolicy to
	// MergeAuto, so this should not be reachable for a task today, but
	// checking Kind directly — the same defense recoverWorktree applies —
	// means this hook stays correct even if that changes.
	if st.Kind != KindThread || st.MergePolicy != MergeAuto {
		return false
	}
	m.lc.goWorker(func() {
		c.opMu.Lock()
		defer c.opMu.Unlock()
		if err := m.mergeAttempt(ctx, st.ID, true, resultText); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("thread: auto-merge failed", "thread", st.ID, "error", err)
		}
	})
	return true
}

// Activate makes a thread's isolated workspace live again without
// dispatching any agent run, moving it to [StatusIdle] while preserving
// the result summary and error of whatever run finished earlier. It is
// what lets a caller attach to a thread whose run is over and keep
// working in it by hand: an unspawned thread can only be viewed
// read-only.
//
// Activating a thread that is already live is a no-op that returns its
// current state. Threads in the merge flow (merging, merged, conflict,
// merge_blocked) are rejected: their branch is being folded into the base
// branch, and reopening the worktree for hand edits underneath that is a
// different feature with its own conflict semantics.
func (m *Manager) Activate(ctx context.Context, idOrName string) (Thread, error) {
	done, err := m.lc.beginOp()
	if err != nil {
		return Thread{}, err
	}
	defer done()
	st, err := m.resolve(ctx, idOrName)
	if err != nil {
		return Thread{}, err
	}
	if st.Kind != KindThread {
		return Thread{}, fmt.Errorf("thread: %q is not a thread", idOrName)
	}

	c := m.lc.control(st.ID)
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.mu.Lock()
	rt := c.runtime
	removed := c.removed
	c.mu.Unlock()
	if removed {
		return Thread{}, fmt.Errorf("thread: %q has been removed", idOrName)
	}
	if rt != nil {
		return st, nil
	}

	switch st.Status {
	case StatusMerging, StatusMerged, StatusConflict, StatusMergeBlocked:
		return Thread{}, fmt.Errorf("thread: %q is in the merge flow (%s) and cannot be reactivated", idOrName, st.Status)
	}
	if _, err := os.Stat(st.WorktreePath); err != nil {
		return Thread{}, fmt.Errorf("thread: worktree for %q is unavailable: %w", idOrName, err)
	}

	handle, err := m.spawner.Spawn(m.ctx, st.WorktreePath)
	if err != nil {
		return Thread{}, fmt.Errorf("thread: respawn workspace: %w", err)
	}
	if err := m.ctx.Err(); err != nil {
		_ = m.spawner.Release(context.Background(), handle.ID())
		return Thread{}, err
	}

	// Preserve the earlier run's outcome: SetStatus rewrites all four
	// columns, so the summary/error/timestamp have to be carried across
	// explicitly or reactivating would erase the record of what ran.
	st, err = m.lc.setStatus(ctx, st.ID, StatusIdle, st.Error, st.ResultSummary, st.CompletedAt)
	if err != nil {
		_ = m.spawner.Release(ctx, handle.ID())
		return Thread{}, err
	}
	m.lc.installRuntime(m.ctx, handle, m.spawner, st.ID)
	return st, nil
}

// Send re-dispatches message into a thread's session, resuming it if its
// workspace is not currently spawned (e.g. after an interrupted run — the
// worktree is still on disk, so the workspace is simply respawned).
func (m *Manager) Send(ctx context.Context, idOrName, message string) error {
	done, err := m.lc.beginOp()
	if err != nil {
		return err
	}
	defer done()
	st, err := m.resolve(ctx, idOrName)
	if err != nil {
		return err
	}
	if st.Kind != KindThread {
		return fmt.Errorf("thread: %q is not a thread", idOrName)
	}
	// The "queue into a live runtime" / "respawn from spawnPath, then
	// dispatch" logic itself has nothing thread-specific in it — see
	// lifecycle.send's doc comment — so it lives there, shared with
	// TaskManager.Send.
	return m.lc.send(ctx, m.ctx, st.ID, m.spawner, st.WorktreePath, st.SessionID, message)
}

// Merge runs (or retries) the merge flow for a thread. Manual-policy
// threads are merged this way once their run completes; auto-policy
// threads use this to retry after a conflict has been resolved.
func (m *Manager) Merge(ctx context.Context, idOrName string) error {
	done, err := m.lc.beginOp()
	if err != nil {
		return err
	}
	defer done()
	st, err := m.resolve(ctx, idOrName)
	if err != nil {
		return err
	}
	if st.Kind != KindThread {
		return fmt.Errorf("thread: %q is not a thread", idOrName)
	}
	// Empty resultSummary tells mergeAttempt to keep whatever is already
	// on the row (e.g. from the run that led to the current conflict or
	// merge_blocked state) instead of clobbering it.
	c := m.lc.control(st.ID)
	c.opMu.Lock()
	defer c.opMu.Unlock()
	return m.mergeAttempt(ctx, st.ID, true, "")
}

// mergeAttempt folds a thread's branch back into its base branch.
// allowRetry permits one recursive retry from the top of the merge
// (re-merging base into the thread branch) when the base has moved on
// concurrently; the retry itself passes allowRetry=false so a persistently
// moving base ends in merge_blocked rather than looping. resultSummary, if
// non-empty, is the agent's final text to record alongside every status
// transition this attempt makes; pass "" to preserve whatever is already
// stored (see Merge).
func (m *Manager) mergeAttempt(ctx context.Context, threadID string, allowRetry bool, resultSummary string) error {
	st, err := m.store.Get(ctx, threadID)
	if err != nil {
		return err
	}
	if resultSummary == "" {
		resultSummary = st.ResultSummary
	}

	st, err = m.lc.setStatus(ctx, threadID, StatusMerging, "", resultSummary, 0)
	if err != nil {
		return err
	}

	// A prior attempt may have left the worktree mid-merge. If conflicts
	// are still unresolved, report that state rather than re-attempting;
	// the caller resolves them (via Send) and calls Merge again.
	if conflicts, err := git.ConflictedFiles(ctx, st.WorktreePath); err != nil {
		return m.blockMerge(ctx, threadID, resultSummary, err.Error())
	} else if len(conflicts) > 0 {
		return m.setConflict(ctx, threadID, resultSummary, conflicts)
	}

	commitMsg := fmt.Sprintf("thread(%s): %s", st.Name, firstLine(st.Goal))
	if _, err := git.CommitAll(ctx, st.WorktreePath, commitMsg); err != nil {
		return m.blockMerge(ctx, threadID, resultSummary, err.Error())
	}

	result, err := git.MergeIntoWorktree(ctx, st.WorktreePath, st.BaseBranch)
	if err != nil {
		return m.blockMerge(ctx, threadID, resultSummary, err.Error())
	}
	if !result.Merged {
		return m.setConflict(ctx, threadID, resultSummary, result.Conflicts)
	}

	ffErr := git.FastForward(ctx, m.repoRoot, st.Branch, st.BaseBranch)
	switch {
	case ffErr == nil:
		return m.finishMerge(ctx, threadID, resultSummary)

	case errors.Is(ffErr, git.ErrBranchCheckedOut):
		dirty, err := git.IsDirty(ctx, m.repoRoot)
		if err != nil {
			return m.blockMerge(ctx, threadID, resultSummary, err.Error())
		}
		if dirty {
			return m.blockMerge(ctx, threadID, resultSummary, "base branch is checked out and the main worktree is dirty: "+ffErr.Error())
		}
		if err := git.MergeFFOnly(ctx, m.repoRoot, st.Branch); err != nil {
			return m.blockMerge(ctx, threadID, resultSummary, err.Error())
		}
		return m.finishMerge(ctx, threadID, resultSummary)

	case errors.Is(ffErr, git.ErrNonFastForward):
		if allowRetry {
			return m.mergeAttempt(ctx, threadID, false, resultSummary)
		}
		return m.blockMerge(ctx, threadID, resultSummary, "base branch moved concurrently: "+ffErr.Error())

	default:
		return m.blockMerge(ctx, threadID, resultSummary, ffErr.Error())
	}
}

func (m *Manager) finishMerge(ctx context.Context, threadID, resultSummary string) error {
	st, err := m.lc.setStatus(ctx, threadID, StatusMerged, "", resultSummary, time.Now().Unix())
	if err != nil {
		return err
	}
	if c := m.lc.existingControl(threadID); c != nil {
		c.mu.Lock()
		rt := c.runtime
		c.runtime = nil
		c.mu.Unlock()
		if rt != nil {
			rt.watchCancel()
			// Cancel this thread's own session, not the whole App's
			// coordinator. Merge is guarded to threads only (see Merge's
			// Kind check), whose App is their own, so this is equivalent
			// to CancelAll today — but CancelAll on a kind that shares its
			// App with something else (a task's parent) would cancel
			// unrelated work, so this stays correct if that guard is ever
			// loosened.
			if a := rt.handle.App(); a != nil && a.AgentCoordinator != nil {
				a.AgentCoordinator.Cancel(st.SessionID)
			}
			if err := rt.spawner.Release(ctx, rt.handle.ID()); err != nil {
				slog.Error("thread: release merged workspace failed", "thread", threadID, "error", err)
			}
		}
	}
	m.lc.publish(EventMerged, st)
	return nil
}

func (m *Manager) setConflict(ctx context.Context, threadID, resultSummary string, conflicts []string) error {
	_, err := m.lc.setStatus(ctx, threadID, StatusConflict, "merge conflicts: "+strings.Join(conflicts, ", "), resultSummary, 0)
	return err
}

func (m *Manager) blockMerge(ctx context.Context, threadID, resultSummary, reason string) error {
	_, err := m.lc.setStatus(ctx, threadID, StatusMergeBlocked, reason, resultSummary, 0)
	return err
}

// Remove tears a thread down: it cancels and releases its workspace (if
// force), removes its git worktree (and branch, if deleteBranch), and
// deletes its store row. It refuses to run/merging threads unless force,
// and refuses unmerged threads with a dirty worktree unless force.
func (m *Manager) Remove(ctx context.Context, idOrName string, force, deleteBranch bool) error {
	done, err := m.lc.beginOp()
	if err != nil {
		return err
	}
	defer done()
	st, err := m.resolve(ctx, idOrName)
	if err != nil {
		return err
	}
	if st.Kind != KindThread {
		return fmt.Errorf("thread: %q is not a thread", idOrName)
	}

	if (st.Status == StatusRunning || st.Status == StatusMerging) && !force {
		return fmt.Errorf("thread: %q is active (status=%s); use force to remove", st.Name, st.Status)
	}
	if st.Status != StatusMerged {
		if dirty, err := git.IsDirty(ctx, st.WorktreePath); err == nil && dirty && !force {
			return fmt.Errorf("thread: %q has unmerged, uncommitted changes; use force to remove", st.Name)
		}
	}

	c := m.lc.control(st.ID)
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.mu.Lock()
	c.removed = true
	rt := c.runtime
	c.runtime = nil
	c.mu.Unlock()

	if rt != nil {
		rt.watchCancel()
		if force {
			// Cancel this delegation's own session, not the whole App's
			// coordinator — see finishMerge's identical comment.
			if a := rt.handle.App(); a != nil && a.AgentCoordinator != nil {
				a.AgentCoordinator.Cancel(st.SessionID)
			}
		}
		if err := rt.spawner.Release(ctx, rt.handle.ID()); err != nil {
			slog.Error("thread: release spawner handle on remove failed", "thread", st.ID, "error", err)
		}
	}

	if err := git.WorktreeRemove(ctx, m.repoRoot, st.WorktreePath, force); err != nil {
		return fmt.Errorf("thread: remove worktree: %w", err)
	}
	if deleteBranch {
		if err := git.DeleteBranch(ctx, m.repoRoot, st.Branch, force); err != nil {
			return fmt.Errorf("thread: delete branch: %w", err)
		}
	}
	if err := m.store.Delete(ctx, st.ID); err != nil {
		return fmt.Errorf("thread: delete record: %w", err)
	}
	m.lc.publish(EventRemoved, st)
	return nil
}

// Handle returns the spawned workspace handle for threadID, or nil if the
// thread's workspace is not currently spawned (e.g. it finished, or is
// between runs after an interrupt).
func (m *Manager) Handle(threadID string) Handle {
	c := m.lc.existingControl(threadID)
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	rt := c.runtime
	if rt == nil {
		return nil
	}
	return rt.handle
}

// WorkspaceID returns the backend/runtime identifier of threadID's
// currently-spawned workspace (Handle.ID()), or "" if not spawned. In
// client/server mode this is the backend workspace ID the thread's
// workspace was created with (see internal/backend/thread_spawner.go's
// threadHandle), letting a client attach to it directly over HTTP; in
// local (single-process) mode it is an opaque spawner-internal ID with no
// meaning outside the process.
func (m *Manager) WorkspaceID(threadID string) string {
	if h := m.Handle(threadID); h != nil {
		return h.ID()
	}
	return ""
}

// Shutdown stops admission, cancels manager work, releases live runtimes, and
// waits for manager-owned goroutines. It is idempotent and safe concurrently.
//
// m.cancel cancels m.ctx, the base context every runtime's watch loop and
// in-flight run derive from — including a [TaskManager]'s, if one was
// constructed sharing this Manager's lifecycle and ctx (see NewManager),
// since m.lc.snapshotControls below walks that same shared controls map
// regardless of which kind registered each entry.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.shutdownOnce.Do(func() {
		m.lc.closeAdmission()
		close(m.shutdownStarted)
		m.cancel()
		go func() {
			// beginOp/Add are serialized with closeAdmission above. Existing
			// workers only add children before their own Done, so this join
			// cannot observe zero before all manager work has finished.
			m.lc.wait()
			controls := m.lc.snapshotControls()
			for threadID, c := range controls {
				c.opMu.Lock()
				c.mu.Lock()
				rt := c.runtime
				c.runtime = nil
				c.mu.Unlock()
				if rt != nil {
					rt.watchCancel()
					// Fetched up front, before Cancel: this entity's own
					// SessionID is what Cancel needs (a task's App is its
					// parent's, so CancelAll here would cancel unrelated
					// work), and the status check below reuses the same
					// row instead of reading it twice. If the row can't be
					// read, skip both — there is no unrelated-work-safe
					// fallback for "cancel something, we don't know what".
					st, getErr := m.store.Get(context.Background(), threadID)
					if getErr == nil {
						if a := rt.handle.App(); a != nil && a.AgentCoordinator != nil {
							a.AgentCoordinator.Cancel(st.SessionID)
						}
					}
					if err := rt.spawner.Release(context.Background(), rt.handle.ID()); err != nil {
						slog.Error("thread: release workspace on shutdown failed", "error", err)
					}
					// The workspace DB remains live until this method returns to
					// its cleanup caller, so record the interrupted terminal
					// state before the connection is released.
					if getErr == nil && st.Status == StatusRunning {
						_, _ = m.lc.setStatus(context.Background(), st.ID, StatusInterrupted, "", "", 0)
					}
				}
				c.opMu.Unlock()
			}
			close(m.shutdownDone)
		}()
	})
	select {
	case <-m.shutdownDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Recover reconciles store state against reality after a process restart:
// threads left pending/running/merging (their goroutines are gone with
// the old process) become interrupted, and threads whose worktree has
// vanished from disk become failed.
func (m *Manager) Recover(ctx context.Context) error {
	done, err := m.lc.beginOp()
	if err != nil {
		return err
	}
	defer done()
	return m.lc.recover(ctx)
}

// recoverWorktree is the [recoverHook] that fails threads whose worktree
// has vanished from disk, ahead of the generic active-status sweep in
// lifecycle.recover. A thread already StatusFailed or StatusMerged is
// left alone: re-failing it would be a no-op at best and clobber a merged
// thread's outcome at worst. A Stat error other than "not exist" (e.g. a
// permission problem) is treated as "worktree present" and falls through
// to the generic sweep, matching the pre-hook behavior.
func (m *Manager) recoverWorktree(ctx context.Context, st Thread) (bool, error) {
	// This hook only knows how to judge threads: a worktree check makes no
	// sense for a delegation kind with no worktree, and an empty
	// WorktreePath would otherwise read as "missing" and get marked
	// failed before the generic active-status sweep ever sees it.
	if st.Kind != KindThread {
		return false, nil
	}
	if _, statErr := os.Stat(st.WorktreePath); !os.IsNotExist(statErr) {
		return false, nil
	}
	if st.Status == StatusFailed || st.Status == StatusMerged {
		return true, nil
	}
	if _, err := m.lc.setStatus(ctx, st.ID, StatusFailed, "worktree missing on recovery", "", 0); err != nil {
		return false, err
	}
	return true, nil
}

// Wait blocks until none of the threads named by ids (all threads, when
// ids is empty) are pending, running, or merging, or until ctx is
// canceled or timeout elapses (timeout <= 0 means no timeout beyond ctx).
func (m *Manager) Wait(ctx context.Context, ids []string, timeout time.Duration) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	for {
		active, err := m.anyActive(ctx, ids)
		if err != nil {
			return err
		}
		if !active {
			return nil
		}
		select {
		case <-m.lc.waitChan():
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (m *Manager) anyActive(ctx context.Context, ids []string) (bool, error) {
	threads, err := m.waitTargets(ctx, ids)
	if err != nil {
		return false, err
	}
	for _, st := range threads {
		if st.Status.Active() {
			return true, nil
		}
	}
	return false, nil
}

// waitTargets resolves the entities Wait should watch. With ids empty this
// deliberately uses the kind = 'thread'-scoped store.List, not
// store.ListAll: Manager's whole public surface (Create, Merge, Wait's own
// doc comment) is stated in terms of threads, so "wait for everything"
// means "wait for every thread" here, unlike the kind-agnostic sweep
// lifecycle.recover needs. A future Task-flavored manager over this same
// table would make the analogous choice for its own kind.
func (m *Manager) waitTargets(ctx context.Context, ids []string) ([]Thread, error) {
	if len(ids) == 0 {
		return m.store.List(ctx)
	}
	threads := make([]Thread, 0, len(ids))
	for _, id := range ids {
		st, err := m.resolve(ctx, id)
		if err != nil {
			return nil, err
		}
		threads = append(threads, st)
	}
	return threads, nil
}

func validateName(name string) (string, error) {
	if !nameRe.MatchString(name) {
		return "", fmt.Errorf("thread: invalid name %q: must be a lowercase alphanumeric slug (hyphens allowed, not leading or trailing)", name)
	}
	return name, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
