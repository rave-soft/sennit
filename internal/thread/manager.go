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
	"github.com/rave-soft/braid/internal/permission"
	"github.com/rave-soft/braid/internal/pubsub"
)

// defaultDataDirName is the project-local data directory a workspace uses
// when nothing else is configured; thread worktrees live in "threads"
// inside it. Kept as a literal rather than imported from internal/config
// to avoid a dependency edge for one string — the fallback only applies
// when a caller supplies no DataDir at all.
const defaultDataDirName = ".braid"

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
	// worktree is created (at WorktreeDir/<name>). Empty defaults to
	// "threads" inside DataDir. A relative value is resolved against
	// RepoRoot's parent directory, not against the process's working
	// directory; an absolute value is used as-is.
	WorktreeDir string
	// DataDir is the workspace's project-local data directory
	// (<repo>/.braid by default), which is where thread worktrees live
	// unless WorktreeDir says otherwise. That directory carries a
	// "*" .gitignore of its own (see app.ensureDotBraidDir), which is
	// what lets worktrees sit inside the repository without the repo
	// seeing a second copy of itself as untracked files. Empty falls
	// back to RepoRoot/.braid.
	DataDir string
	// Context is the base context background thread goroutines (agent
	// runs, RunComplete watchers) are bound to. Defaults to
	// context.Background().
	Context context.Context
	// ParentApp is the workspace this Manager is attached to (see
	// Attach) — the one a thread's own CreateArgs.ParentSessionID refers
	// to a session in. A thread spawns its own isolated workspace
	// (Spawner), completely separate from this one, so its
	// terminal-completion delivery target has to be captured explicitly
	// here rather than derived from the thread's own workspace the way a
	// task's is (see Manager.resolveDeliveryTarget). Optional: nil
	// disables thread delivery without otherwise affecting the manager
	// (Attach always supplies it in production; a test building a bare
	// Manager may not need thread delivery at all).
	ParentApp Workspace
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
	// parentApp is the workspace this Manager is attached to — see
	// ManagerOptions.ParentApp and resolveDeliveryTarget.
	parentApp Workspace

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
		dataDir := opts.DataDir
		if dataDir == "" {
			dataDir = filepath.Join(opts.RepoRoot, defaultDataDirName)
		}
		worktreeDir = filepath.Join(dataDir, "threads")
	case !filepath.IsAbs(worktreeDir):
		worktreeDir = filepath.Join(filepath.Dir(opts.RepoRoot), worktreeDir)
	}
	m := &Manager{
		store:           opts.Store,
		spawner:         opts.Spawner,
		repoRoot:        opts.RepoRoot,
		worktreeDir:     worktreeDir,
		ctx:             ctx,
		parentApp:       opts.ParentApp,
		shutdownStarted: make(chan struct{}),
		shutdownDone:    make(chan struct{}),
	}
	// onAutoMerge/recoverWorktree/resolveDeliveryTarget are this package's
	// git/merge overlay on the generic lifecycle; a lighter, worktree-less
	// delegation kind would pass nil for the first two (see TaskManager,
	// which supplies neither — it shares this same lifecycle instead). A
	// TaskManager sharing this lifecycle (see NewTaskManager) must be
	// constructed with this same m.lc and m.ctx, not fresh ones, or
	// recovery and shutdown would only ever see threads.
	m.lc = newLifecycle(opts.Store, m.onAutoMerge, m.recoverWorktree, m.resolveDeliveryTarget, opts.ParentApp)
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
//
// A miss is reported as [ErrNotFound] rather than whatever the store said.
// The store's own "sql: no rows in result set" is an implementation detail
// that means nothing to the caller, and since a merged thread is removed
// (see discardMerged), asking about one by name is now an ordinary thing
// to do — the answer has to be a sentence, not a database message.
func (m *Manager) resolve(ctx context.Context, idOrName string) (Thread, error) {
	if st, err := m.store.Get(ctx, idOrName); err == nil {
		return st, nil
	}
	st, err := m.store.GetByName(ctx, idOrName)
	if err != nil {
		return Thread{}, fmt.Errorf("%w: %q", ErrNotFound, idOrName)
	}
	return st, nil
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
		Name:            name,
		Goal:            args.Goal,
		BaseBranch:      base,
		Branch:          branch,
		WorktreePath:    worktreePath,
		MergePolicy:     mergePolicy,
		ParentSessionID: args.ParentSessionID,
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
	// Stash the parent link now, while nothing else can be reading it yet
	// - resolveDeliveryTarget reads it back once this thread reaches a
	// terminal status. Empty when args.ParentSessionID is empty (a thread
	// created with no parent, unlike a task's required one), which
	// resolveDeliveryTarget then correctly treats as "nobody to deliver
	// to" rather than an error.
	c.parentSessionID = args.ParentSessionID
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

	var sess Session
	if args.ParentSessionID == "" {
		sess, err = handle.Workspace().Sessions().Create(ctx, args.Goal)
	} else {
		sess, err = handle.Workspace().Sessions().CreateTaskSession(ctx, uuid.NewString(), args.ParentSessionID, args.Goal)
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
	// Register the parent on the thread's own coordinator - its
	// dispatcher is what the thread's own turns run through, so that is
	// where a mid-run ask must be looked up by session id - but with
	// Parent pointing at m.parentApp's coordinator, mirroring
	// resolveDeliveryTarget's KindThread branch: a thread's own App is
	// wholly isolated, so its coordinator is not where a completion (or
	// an ask) is ever delivered to. A thread's parent is optional
	// (unlike a task's), and guarded on m.parentApp the same way
	// resolveDeliveryTarget is, since a Manager built without one (see
	// ManagerOptions.ParentApp) must not panic. Placed here so both the
	// idle (empty-Goal) and dispatched-Goal paths below register it - an
	// idle thread activated by hand later must still be able to ask.
	if args.ParentSessionID != "" && m.parentApp != nil {
		handle.Workspace().Coordinator().RegisterDelegationParent(sess.ID, DelegationParent{
			Parent:          m.parentApp.Coordinator(),
			ParentSessionID: args.ParentSessionID,
			DelegationID:    st.ID,
			Kind:            string(KindThread),
			Name:            st.Name,
			Depth:           0, // deliverCompletion is always called with depth 0 for a thread; see its onAutoMerge call above.
		})
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
	m.lc.startRun(WithAgentDispatch(m.ctx), handle, m.spawner, st.ID, st.SessionID, args.Goal)

	return st, nil
}

// abortSpawn tears a just-spawned workspace and worktree back down after a
// failure partway through Create. Errors are logged, not returned: the
// caller already has the error that triggered the rollback and that is
// what gets surfaced.
func (m *Manager) abortSpawn(ctx context.Context, handle Handle, worktreePath string) {
	if err := m.spawner.Release(ctx, handle.ID()); err != nil {
		slog.Error("Failed to release spawner handle during create rollback", "component", "thread", "error", err)
	}
	if err := git.WorktreeRemove(ctx, m.repoRoot, worktreePath, true); err != nil {
		slog.Error("Failed to remove worktree during create rollback", "component", "thread", "error", err)
	}
}

// failCreate records cause as the thread's terminal failure and returns it
// to Create's caller.
func (m *Manager) failCreate(ctx context.Context, st Thread, cause error) error {
	if _, err := m.lc.setStatus(ctx, st.ID, StatusFailed, cause.Error(), "", 0); err != nil {
		slog.Error("Failed to record create failure", "component", "thread", "thread", st.ID, "error", err)
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
			slog.Error("Auto-merge failed", "component", "thread", "thread", st.ID, "error", err)
		}
		// The run finishing is not this thread's useful terminal event —
		// an auto-merge thread hands straight from running into the merge
		// flow (see this method's own doc comment), and the parent can't
		// act on "the run finished" when the outcome that actually matters
		// (merged cleanly, hit a conflict, or got blocked) is still
		// pending. Deliver once mergeAttempt has landed on whichever of
		// those three it reached instead.
		m.deliverMergeOutcome(ctx, st.ID)
		// Strictly after delivery: discardMerged deletes the store row,
		// and deliverMergeOutcome re-reads that row to learn which of the
		// three outcomes the attempt reached.
		m.discardMerged(ctx, st.ID)
	})
	return true
}

// deliverMergeOutcome delivers an auto-merge thread's own terminal event —
// merged, conflict, or merge_blocked — to its parent session, once,
// right after mergeAttempt concludes from onAutoMerge. It is deliberately
// not wired into mergeAttempt/finishMerge/setConflict/blockMerge
// themselves: those four are shared with Manager.Merge's manual retry
// path (resolving a conflict, or retrying after merge_blocked), and a
// manually-triggered retry must not re-deliver an event its caller
// already observes synchronously through Merge's own return value — only
// the original, automatic attempt this method is called from should ever
// notify the parent. That is also what keeps delivery at-most-once
// across a thread's two terminal moments: handleRunComplete's own
// delivery call only ever reaches a thread that failed or was cancelled,
// or a manual-policy thread that completed — never one whose
// onAutoMerge just returned true, which is precisely the case that
// lands here instead.
func (m *Manager) deliverMergeOutcome(ctx context.Context, threadID string) {
	st, err := m.store.Get(ctx, threadID)
	if err != nil {
		slog.Error("Failed to re-fetch thread for merge-outcome delivery", "component", "thread", "thread", threadID, "error", err)
		return
	}
	if !st.Status.Terminal() {
		// mergeAttempt returned without ever reaching a terminal write —
		// a pure infrastructure error (e.g. the initial store.Get inside
		// mergeAttempt itself failing) before the merge flow properly
		// started. Nothing to report yet.
		return
	}
	// handle is nil: a thread's delivery target never comes from its own
	// workspace handle (see resolveDeliveryTarget's KindThread branch),
	// and by the time an auto-merge lands on Merged the workspace may
	// already be released (finishMerge's own teardown) regardless.
	m.lc.deliverCompletion(ctx, nil, st, 0)
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
	// The dispatcher's DelegationParent registry lives per coordinator
	// instance and is empty on a freshly-started process (see
	// resolveDeliveryTarget's doc comment on the persisted column this now
	// reads from). Re-register here, on the freshly-installed handle, so a
	// thread resumed after a restart can still ask its parent mid-run — not
	// just report its eventual completion.
	if st.ParentSessionID != "" && m.parentApp != nil {
		handle.Workspace().Coordinator().RegisterDelegationParent(st.SessionID, DelegationParent{
			Parent:          m.parentApp.Coordinator(),
			ParentSessionID: st.ParentSessionID,
			DelegationID:    st.ID,
			Kind:            string(KindThread),
			Name:            st.Name,
			Depth:           0, // Depth is not persisted (a pre-existing gap - see threadControl.depth, also in-memory-only); 0 is the safe default a resumed entity's cascade depth already silently falls back to today.
		})
	}
	return st, nil
}

// Cancel stops a thread's in-flight run and rests it at [StatusCancelled]
// with reason recorded as its Error, leaving its worktree and branch on
// disk for the user to inspect or resume by hand later (via Activate or
// Send) — unlike Remove, which tears everything down. See
// [lifecycle.cancel] for the mechanics, shared with [TaskManager.Cancel]:
// a thread's own runtime lives in its own isolated App (LocalSpawner),
// never a parent's, so cancelling never risks reaching anyone else's
// work — the same safety TaskManager.Cancel gets from scoping to the
// task's own session, just for a different structural reason (a thread's
// App genuinely has nothing else in it, rather than a task sharing its
// parent's).
//
// Refuses a thread already in the merge flow (merging, merged, conflict,
// merge_blocked) the same way Activate refuses to reactivate one:
// mergeAttempt holds the thread's opMu for its entire duration (see
// onAutoMerge's doc comment), so by the time this call's own status read
// can matter the merge has either not started (an active run is exactly
// what Cancel exists to stop) or has already landed on one of its own
// terminal outcomes — at which point there is no run left in flight to
// cancel, and folding a branch back into its base is not a step to
// interrupt partway.
func (m *Manager) Cancel(ctx context.Context, idOrName, reason string) error {
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
	switch st.Status {
	case StatusMerging, StatusMerged, StatusConflict, StatusMergeBlocked:
		return fmt.Errorf("thread: %q is in the merge flow (%s) and cannot be cancelled", idOrName, st.Status)
	}
	return m.lc.cancel(ctx, st, reason)
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
	if err := m.lc.send(ctx, m.ctx, st.ID, m.spawner, st.WorktreePath, st.SessionID, message); err != nil {
		return err
	}
	// l.send does not hand the (possibly freshly respawned) handle back to
	// its caller, so re-register from here instead, reading the
	// now-installed runtime off the entity's control — see Activate's
	// identical re-registration for why this has to happen on every resume,
	// not just at Create. Runs on every Send, including when the workspace
	// was already live (never actually restarted): that is fine, it is an
	// idempotent Set, not a spawn.
	if st.ParentSessionID != "" && m.parentApp != nil {
		if c := m.lc.existingControl(st.ID); c != nil {
			c.mu.Lock()
			rt := c.runtime
			c.mu.Unlock()
			if rt != nil {
				rt.handle.Workspace().Coordinator().RegisterDelegationParent(st.SessionID, DelegationParent{
					Parent:          m.parentApp.Coordinator(),
					ParentSessionID: st.ParentSessionID,
					DelegationID:    st.ID,
					Kind:            string(KindThread),
					Name:            st.Name,
					Depth:           0,
				})
			}
		}
	}
	return nil
}

// Merge runs (or retries) the merge flow for a thread. Manual-policy
// threads are merged this way once their run completes; auto-policy
// threads use this to retry after a conflict has been resolved.
//
// It returns the thread as the attempt left it. Conflict and
// merge_blocked are outcomes, not errors (see mergeAttempt), so the
// returned status is how a caller tells them from a clean landing — and
// the value is returned rather than re-read afterwards because a thread
// that merged cleanly has already been discarded by the time this
// returns, and there is no row left to read.
func (m *Manager) Merge(ctx context.Context, idOrName string) (Thread, error) {
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
	// Empty resultSummary tells mergeAttempt to keep whatever is already
	// on the row (e.g. from the run that led to the current conflict or
	// merge_blocked state) instead of clobbering it.
	c := m.lc.control(st.ID)
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if err := m.mergeAttempt(ctx, st.ID, true, ""); err != nil {
		return Thread{}, err
	}
	// Read the outcome before discarding: this is the caller's only
	// chance to see it.
	final, err := m.store.Get(ctx, st.ID)
	if err != nil {
		return Thread{}, err
	}
	// A hand-merged thread is as spent as an auto-merged one, so it is
	// discarded on the same terms. Nothing about who triggered the merge
	// changes what is left behind. Not folded into mergeAttempt itself:
	// that is also the retry path, and only a merge that actually
	// concluded may discard anything — discardMerged's own status check
	// is what makes a conflict or a block survive this call untouched.
	m.discardMerged(ctx, st.ID)
	return final, nil
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
			if a := rt.handle.Workspace(); a != nil && a.Coordinator() != nil {
				a.Coordinator().Cancel(st.SessionID)
			}
			if err := rt.spawner.Release(ctx, rt.handle.ID()); err != nil {
				slog.Error("Failed to release merged workspace", "component", "thread", "thread", threadID, "error", err)
			}
		}
	}
	m.lc.publish(EventMerged, st)
	return nil
}

// discardMerged clears away a thread whose branch has landed: its work is
// in the base branch, so the worktree and branch are duplicates and the
// row is a row about nothing. Both merge paths call it — the automatic one
// and Manager.Merge — and it leaves a note in the parent session's history
// so the thread's disappearance from the panel is something the user can
// read about rather than something they have to notice.
//
// Callers must hold the thread's opMu; both do. Every step is best-effort
// and only reports through the log: the merge itself has already
// succeeded and been delivered by the time this runs, so nothing here may
// turn a landed merge into a failure. If the worktree cannot be removed,
// the row deliberately stays — a thread still visible as merged is a far
// better outcome than a row-less directory nothing knows how to clean up.
//
// The thread's own session and its messages are untouched. They are the
// record of how the work was done, and dropping them would make the
// history entry point at nothing.
func (m *Manager) discardMerged(ctx context.Context, threadID string) {
	st, err := m.store.Get(ctx, threadID)
	if err != nil {
		slog.Error("Failed to re-fetch merged thread for discard", "component", "thread", "thread", threadID, "error", err)
		return
	}
	// Only a merge that landed. A conflict or a block still owns its
	// worktree — that is exactly where the user resolves it.
	if st.Kind != KindThread || st.Status != StatusMerged {
		return
	}

	if err := git.WorktreeRemove(ctx, m.repoRoot, st.WorktreePath, true); err != nil {
		slog.Error("Failed to remove merged worktree", "component", "thread", "thread", threadID, "error", err)
		return
	}
	// Verify the branch really is contained in the base before deleting
	// it, and only then force. "git branch -d" is not that check: it asks
	// whether the branch is merged into HEAD, so it refuses a properly
	// merged branch whenever the repo has something else checked out —
	// which for a thread's base branch is the normal case, not the
	// exception. Ask about the two branches that matter, then act on the
	// answer.
	branchKept := true
	merged, err := git.IsAncestor(ctx, m.repoRoot, st.Branch, st.BaseBranch)
	switch {
	case err != nil:
		slog.Error("Failed to verify merged branch before delete", "component", "thread", "thread", threadID, "branch", st.Branch, "error", err)
	case !merged:
		slog.Error("Refusing to delete a branch not contained in its base", "component", "thread", "thread", threadID, "branch", st.Branch, "base", st.BaseBranch)
	default:
		if err := git.DeleteBranch(ctx, m.repoRoot, st.Branch, true); err != nil {
			slog.Error("Failed to delete merged branch", "component", "thread", "thread", threadID, "branch", st.Branch, "error", err)
		} else {
			branchKept = false
		}
	}
	if err := m.store.Delete(ctx, st.ID); err != nil {
		slog.Error("Failed to delete merged thread record", "component", "thread", "thread", threadID, "error", err)
		return
	}
	m.lc.publish(EventRemoved, st)
	m.recordDiscardNotice(ctx, st, branchKept)
}

// recordDiscardNotice writes the merged-and-removed note into the parent
// session's history.
//
// It is a system-role message on purpose: that role is dropped when the
// prompt is built (see message.Message conversion), so this is a record
// for the user only. The model has already been told the thread finished,
// through the completion inbox — telling it a second time, in a different
// voice, would be the kind of duplicate that makes an agent repeat itself.
func (m *Manager) recordDiscardNotice(ctx context.Context, st Thread, branchKept bool) {
	a, sessionID, ok := m.resolveDeliveryTarget(ctx, nil, st)
	if !ok || a == nil || a.Messages() == nil {
		return
	}
	text := fmt.Sprintf("Thread %q merged into %s and removed.", st.Name, st.BaseBranch)
	if branchKept {
		text = fmt.Sprintf("Thread %q merged into %s and removed; branch %s kept (git declined to delete it).",
			st.Name, st.BaseBranch, st.Branch)
	}
	if err := a.Messages().Create(ctx, sessionID, RoleSystem, []ContentPart{TextContent{Text: text}}); err != nil {
		slog.Error("Failed to record thread removal in history", "component", "thread", "thread", st.ID, "error", err)
	}
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
			// Cancel this delegation's own session, not the whole workspace's
			// coordinator — see finishMerge's identical comment.
			if a := rt.handle.Workspace(); a != nil && a.Coordinator() != nil {
				a.Coordinator().Cancel(st.SessionID)
			}
		}
		if err := rt.spawner.Release(ctx, rt.handle.ID()); err != nil {
			slog.Error("Failed to release spawner handle on remove", "component", "thread", "thread", st.ID, "error", err)
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

// WorkspaceID returns the runtime identifier of threadID's
// currently-spawned workspace (Handle.ID()), or "" if not spawned. It is
// an opaque spawner-internal ID with no meaning outside the process.
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
// SetPermissionsSkip propagates the parent workspace's permission-bypass
// ("yolo") state to every delegation workspace currently live under this
// manager, threads and tasks alike. Called by the parent App whenever its
// own bypass state changes (see app.App.SetPermissionsSkip), which is the
// single funnel every toggle goes through: the TUI's ctrl+y and a
// permissions.bypass config reload.
//
// Threads spawned after the change inherit it at spawn instead, from the
// parent's live permission service — see the parentYOLO closure in
// internal/cmd/root.go.
func (m *Manager) SetPermissionsSkip(skip bool) {
	m.lc.setPermissionsSkip(skip)
}

// PermissionsFor returns the permission service holding delegationID's
// pending requests, or nil when it names nothing live under this manager.
//
// A thread's prompts are raised against its own isolated workspace's
// service and relayed to the parent for display (see
// lifecycle.forwardPermissions), so the parent must answer them where they
// are actually waiting. Answering against the parent's own service would
// find no such request and silently do nothing — leaving the thread
// blocked on exactly the prompt the user just answered.
//
// nil for a task, whose handle wraps the parent's own App: there is
// nothing to route, and the caller's own service is already correct.
func (m *Manager) PermissionsFor(delegationID string) permission.Service {
	if delegationID == "" {
		return nil
	}
	c := m.lc.existingControl(delegationID)
	if c == nil {
		return nil
	}
	c.mu.Lock()
	rt := c.runtime
	c.mu.Unlock()
	if rt == nil {
		return nil
	}
	a := rt.handle.Workspace()
	if a == nil || a == m.parentApp {
		return nil
	}
	return a.Permissions()
}

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
						if a := rt.handle.Workspace(); a != nil && a.Coordinator() != nil {
							a.Coordinator().Cancel(st.SessionID)
						}
					}
					if err := rt.spawner.Release(context.Background(), rt.handle.ID()); err != nil {
						slog.Error("Failed to release workspace on shutdown", "component", "thread", "error", err)
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
//
// This deliberately does not call RegisterDelegationParent for anything:
// a recovered entity is not live — recover only marks it interrupted, no
// workspace or coordinator instance exists yet to register against.
// Every path that makes a delegation's workspace live again (Activate,
// Send) already re-registers using the persisted ParentSessionID, so
// there is nothing to gain here and a real risk of registering against a
// coordinator that then never gets used.
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

// resolveDeliveryTarget is the lifecycle's deliveryResolver hook (see
// lifecycle.deliverCompletion, the only caller): it finds where a
// delegation's terminal completion should go, branching on Kind the same
// way onAutoMerge and recoverWorktree do, since both kinds sharing this
// lifecycle resolve their delivery target completely differently.
//
// Both branches now read st.ParentSessionID directly — the persisted
// column (see Delegation.ParentSessionID), not any in-memory field — so
// this resolves correctly even for a delegation resumed after a process
// restart, when no in-memory state from the original Create survives.
//
// A task shares its parent's own App (threadspawn's ParentAppSpawner),
// so handle.Workspace() already *is* the parent workspace.
//
// A thread spawns its own isolated App with a wholly separate database
// (threadspawn's LocalSpawner), so the delivery target is instead
// m.parentApp — the workspace this Manager itself was attached to, not
// the thread's own — see ManagerOptions.ParentApp.
func (m *Manager) resolveDeliveryTarget(ctx context.Context, handle Handle, st Thread) (Workspace, string, bool) {
	switch st.Kind {
	case KindTask:
		if st.ParentSessionID == "" {
			return nil, "", false
		}
		a := handle.Workspace()
		if a == nil {
			return nil, "", false
		}
		return a, st.ParentSessionID, true
	case KindThread:
		if m.parentApp == nil {
			return nil, "", false
		}
		if st.ParentSessionID == "" {
			// A thread created with no ParentSessionID (optional, unlike
			// a task's — see CreateArgs and the CLI's own thread create
			// path). Nobody to deliver to; the thread's own terminal
			// status is still recorded and pollable via thread_status.
			return nil, "", false
		}
		return m.parentApp, st.ParentSessionID, true
	default:
		return nil, "", false
	}
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
