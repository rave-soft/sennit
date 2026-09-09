// Package thread implements threads: parallel agent work streams, each
// running in its own git worktree and branch with a fully isolated
// workspace (own .sennit data directory, database, and agent
// coordinator), and by default safely cleaned up after completion when it has no retained work.
package thread

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/filepathext"
	"github.com/rave-soft/sennit/internal/git"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/question"
)

// defaultDataDirName is the project-local data directory a workspace uses
// when nothing else is configured; thread worktrees live in "threads"
// inside it.
const defaultDataDirName = brand.DataDir

// nameRe restricts thread names to values safe to embed in a branch name
// and a worktree directory: lowercase alphanumeric slugs, hyphen
// separated, not leading/trailing with a hyphen.
var nameRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)

// CreateArgs holds the inputs to [Manager.Create]. BaseBranch defaults to
// the repository's currently checked-out branch when empty.
type CreateArgs struct {
	Name            string
	Goal            string
	BaseBranch      string
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
	// (<repo>/.sennit by default), which is where thread worktrees live
	// unless WorktreeDir says otherwise. That directory carries a
	// "*" .gitignore of its own (see app.ensureDataDir), which is
	// what lets worktrees sit inside the repository without the repo
	// seeing a second copy of itself as untracked files. Empty falls
	// back to RepoRoot/.sennit.
	DataDir string
	// Context is the base context background thread goroutines (agent
	// runs, RunComplete watchers) are bound to. Defaults to
	// context.Background().
	Context context.Context
	// RollbackTimeout bounds cleanup after a failed Create or Activate.
	// The cleanup context preserves caller values but not cancellation, so
	// resources can still be released after a request aborts. Zero defaults
	// to terminalBookkeepingTimeout.
	RollbackTimeout time.Duration
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
	ParentApp      Workspace
	WorktreeRemove func(context.Context, string, string, bool) error
	DeleteBranch   func(context.Context, string, string, bool) error
}

// Manager is the core of the threads feature: it drives thread creation,
// dispatches and tracks each thread's agent run in its isolated
// workspace, and safely cleans up redundant completed work. The
// generic admission, per-entity serialization, worker tracking, and event
// plumbing it needs to do that live in the [lifecycle] it holds; Manager
// itself is the git/worktree-specific overlay on top.
type Manager struct {
	store           Store
	spawner         Spawner
	repoRoot        string
	worktreeDir     string
	ctx             context.Context
	cancel          context.CancelFunc
	rollbackTimeout time.Duration
	// parentApp is the workspace this Manager is attached to — see
	// ManagerOptions.ParentApp and resolveDeliveryTarget.
	parentApp      Workspace
	worktreeRemove func(context.Context, string, string, bool) error
	deleteBranch   func(context.Context, string, string, bool) error

	lc *lifecycle

	shutdownOnce    sync.Once
	shutdownStarted chan struct{}
	shutdownDone    chan struct{}
	shutdownMu      sync.Mutex
	shutdownWaiters int
	shutdownCancel  context.CancelFunc
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
	case !filepathext.SmartIsAbs(worktreeDir):
		// Use SmartIsAbs, not filepath.IsAbs: a WorktreeDir may come from
		// a config file written with Unix-style paths (portable across
		// platforms), and filepath.IsAbs alone rejects those on Windows
		// (no drive letter), silently anchoring an already-absolute path
		// under the repo instead of using it as-is.
		worktreeDir = filepath.Join(filepath.Dir(opts.RepoRoot), worktreeDir)
	}
	rollbackTimeout := opts.RollbackTimeout
	if rollbackTimeout <= 0 {
		rollbackTimeout = terminalBookkeepingTimeout
	}
	m := &Manager{
		store:           opts.Store,
		spawner:         opts.Spawner,
		repoRoot:        opts.RepoRoot,
		worktreeDir:     worktreeDir,
		ctx:             ctx,
		rollbackTimeout: rollbackTimeout,
		parentApp:       opts.ParentApp,
		worktreeRemove:  opts.WorktreeRemove,
		deleteBranch:    opts.DeleteBranch,
		shutdownStarted: make(chan struct{}),
		shutdownDone:    make(chan struct{}),
	}
	if m.worktreeRemove == nil {
		m.worktreeRemove = git.WorktreeRemove
	}
	if m.deleteBranch == nil {
		m.deleteBranch = git.DeleteBranch
	}
	// onCompletedCleanup/recoverWorktree/resolveDeliveryTarget are this package's
	// git/worktree overlay on the generic lifecycle; a lighter, worktree-less
	// delegation kind would pass nil for the first two (see TaskManager,
	// which supplies neither — it shares this same lifecycle instead). A
	// TaskManager sharing this lifecycle (see NewTaskManager) must be
	// constructed with this same m.lc and m.ctx, not fresh ones, or
	// recovery and shutdown would only ever see threads.
	m.lc = newLifecycle(opts.Store, m.onCompletedCleanup, m.recoverWorktree, m.resolveDeliveryTarget, opts.ParentApp)
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

func (m *Manager) WorktreeDir() string {
	return m.worktreeDir
}

// Get resolves idOrName (an ID or a name) to a thread.
func (m *Manager) Get(ctx context.Context, idOrName string) (Thread, error) {
	return m.resolve(ctx, idOrName)
}

// resolve looks a thread up by ID first, falling back to name.
//
// A miss is reported as [ErrNotFound] rather than whatever the store said.
// The store's own "sql: no rows in result set" is an implementation detail
// that means nothing to the caller. A completed thread may have been removed
// automatically, so asking about one by name is ordinary and must return a
// domain error rather than a database message.
func (m *Manager) resolve(ctx context.Context, idOrName string) (Thread, error) {
	st, err := m.store.Get(ctx, idOrName)
	if err == nil {
		return st, nil
	}
	if !isNotFound(err) {
		return Thread{}, err
	}
	st, err = m.store.GetByName(ctx, idOrName)
	if isNotFound(err) {
		return Thread{}, fmt.Errorf("%w: %q", ErrNotFound, idOrName)
	}
	if err != nil {
		return Thread{}, err
	}
	return st, nil
}

func isNotFound(err error) bool {
	return errors.Is(err, ErrNotFound)
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

	worktreePath := filepath.Join(m.worktreeDir, name)

	st, err := m.store.Create(ctx, CreateParams{
		Name:            name,
		Goal:            args.Goal,
		BaseBranch:      base,
		Branch:          branch,
		WorktreePath:    worktreePath,
		ParentSessionID: args.ParentSessionID,
	})
	if err != nil {
		// The GetByName check above is check-then-act: two concurrent
		// Creates for the same name can both pass it and race into this
		// insert, so the store's contract for this case (Create must
		// return an error wrapping ErrNameTaken on a name collision —
		// see ErrNameTaken's doc comment) is the real guard. Map that
		// into the same message the check above gives.
		if errors.Is(err, ErrNameTaken) {
			return Thread{}, fmt.Errorf("thread: name %q is already in use", name)
		}
		return Thread{}, fmt.Errorf("thread: create record: %w", err)
	}
	m.lc.publish(EventCreated, st)

	// A thread's parent link is optional, unlike a task's required one;
	// resolveDeliveryTarget reads it back once this thread reaches a
	// terminal status, treating an empty link as "nobody to deliver to"
	// rather than an error. depth is always 0 - a thread has no cascade.
	c, removed := m.lc.beginControlledCreate(st.ID, args.ParentSessionID, 0)
	defer c.opMu.Unlock()
	if removed {
		return Thread{}, fmt.Errorf("thread: %q was removed during creation", name)
	}

	// rb unwinds the worktree/spawn steps below if Create returns before
	// the thread reaches somewhere safe to rest — see [unwinder].
	var rb unwinder
	defer rb.unwind()
	released := true

	if err := git.WorktreeAdd(ctx, m.repoRoot, worktreePath, branch, base); err != nil {
		return Thread{}, m.failCreate(ctx, st, err)
	}
	// WorktreeAdd creates the branch as well as its checkout. If a later
	// creation step fails, remove both; leaving the branch behind makes a
	// retry collide with stale state even though Create reported failure.
	rb.push(func() {
		if !released {
			return
		}
		cleanupCtx, cancel := m.detachForRollback(ctx)
		defer cancel()
		dirty, err := git.IsDirty(cleanupCtx, worktreePath)
		if err != nil || dirty {
			return
		}
		contained, err := git.IsAncestor(cleanupCtx, m.repoRoot, branch, base)
		if err != nil || !contained {
			return
		}
		if err := git.WorktreeRemove(cleanupCtx, m.repoRoot, worktreePath, false); err != nil {
			return
		}
		if err := git.DeleteBranch(cleanupCtx, m.repoRoot, branch, true); err != nil {
			slog.Warn("Failed to remove branch after thread creation failure", "branch", branch, "error", err)
		}
	})

	handle, err := m.spawner.Spawn(m.ctx, worktreePath)
	if handle != nil {
		rb.push(func() {
			cleanupCtx, cancel := m.detachForRollback(ctx)
			defer cancel()
			if err := m.spawner.Release(cleanupCtx, handle.ID()); err != nil {
				released = false
				c.mu.Lock()
				c.runtime = &runtimeState{handle: handle, spawner: m.spawner, watchCancel: func() {}, releaseFailed: true}
				c.mu.Unlock()
				slog.Error("Failed to release workspace during rollback", "thread", st.ID, "error", err)
			}
		})
	}
	if err != nil {
		return Thread{}, m.failCreate(ctx, st, err)
	}
	if err := m.ctx.Err(); err != nil {
		return Thread{}, m.failCreate(ctx, st, err)
	}

	var sess Session
	if args.ParentSessionID == "" {
		sess, err = handle.Workspace().Sessions().Create(ctx, args.Goal)
	} else {
		sess, err = handle.Workspace().Sessions().CreateTaskSession(ctx, uuid.NewString(), args.ParentSessionID, args.Goal)
	}
	if err != nil {
		return Thread{}, m.failCreate(ctx, st, err)
	}

	newSt, err := m.store.SetSession(ctx, st.ID, sess.ID)
	if err != nil {
		return Thread{}, m.failCreate(ctx, st, err)
	}
	st = newSt
	// Register the parent on the thread's own coordinator - its
	// dispatcher is what the thread's own turns run through, so that is
	// where a mid-run ask must be looked up by session id - but with
	// Parent pointing at m.parentApp's coordinator, mirroring
	// resolveDeliveryTarget's KindThread branch: a thread's own App is
	// wholly isolated, so its coordinator is not where a completion (or
	// an ask) is ever delivered to. Placed here so both the idle
	// (empty-Goal) and dispatched-Goal paths below register it - an idle
	// thread activated by hand later must still be able to ask.
	m.registerThreadParent(handle, st)

	if args.Goal == "" {
		idleSt, err := m.lc.setStatus(ctx, st.ID, StatusIdle, "", "", 0)
		if err != nil {
			return Thread{}, m.failCreate(ctx, st, err)
		}
		st = idleSt
		// Keep the workspace live so attaching to the thread lands in a
		// writable session rather than the read-only view reserved for
		// unspawned threads. Shutdown and Remove release it like any
		// other runtime.
		m.lc.installRuntime(m.ctx, handle, m.spawner, st.ID)
		rb.commit()
		return st, nil
	}

	runningSt, err := m.lc.setStatus(ctx, st.ID, StatusRunning, "", "", 0)
	if err != nil {
		return Thread{}, m.failCreate(ctx, st, err)
	}
	st = runningSt
	m.lc.startRun(WithAgentDispatch(m.ctx), handle, m.spawner, st.ID, st.SessionID, args.Goal)
	rb.commit()
	return st, nil
}

// removeWorktree and releaseHandle are the two rollbacks Create/Activate
// push onto an [unwinder]. Their callers pass a detached, deadline-bound
// context so cleanup survives cancellation of the failed operation without
// letting a wedged git or spawner operation hold the caller indefinitely.
// detachForRollback preserves request values while allowing rollback to
// outlive request cancellation, and bounds every cleanup operation with the
// manager's configured timeout.
func (m *Manager) detachForRollback(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), m.rollbackTimeout)
}

// failCreate records cause as the thread's terminal failure and returns it
// to Create's caller. st must be the row Create's own store.Create call
// produced — never the zero-value return of a failed store call — or the
// write below targets an empty ID and marks nothing.
//
// Every early return in Create from the point its row exists calls this,
// without exception: leaving the row at its last transient status would
// misrepresent an abandoned thread as pending or running, since
// lifecycle.recover's sweep only reconciles active statuses on a restart,
// not a still-live process. A setStatus failure recursing into this
// method is a redundant write at worst — never a way to turn a
// successful write into a failure, since failCreate's own error is only
// logged.
func (m *Manager) failCreate(ctx context.Context, st Thread, cause error) error {
	// detachForTerminalWork, not ctx directly: cause is very often ctx
	// having been cancelled, and a status write built on that same dead
	// ctx would fail too, leaving the row stuck at its transient status
	// (StatusPending or the short-lived StatusIdle/StatusRunning) until a
	// restart's Recover reconciles it. See handleRunComplete, which
	// solves this identical problem the same way.
	writeCtx, cancel := detachForTerminalWork(ctx)
	defer cancel()
	if _, err := m.lc.setStatus(writeCtx, st.ID, StatusFailed, cause.Error(), "", 0); err != nil {
		slog.Error("Failed to record create failure", "component", "thread", "thread", st.ID, "error", err)
	}
	return cause
}

// Activate makes a thread's isolated workspace live again without
// dispatching any agent run, moving it to [StatusIdle] while preserving
// the result summary and error of whatever run finished earlier. It is
// what lets a caller attach to a thread whose run is over and keep
// working in it by hand: an unspawned thread can only be viewed
// read-only.
//
// Activating a thread that is already live is a no-op that returns its
// current state.
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
	if rt != nil && rt.releaseFailed {
		if err := releaseRuntime(ctx, rt, st.SessionID, true); err != nil {
			return Thread{}, fmt.Errorf("thread: release previous workspace: %w", err)
		}
		c.mu.Lock()
		c.runtime = nil
		c.mu.Unlock()
		rt = nil
	}
	if rt != nil {
		return st, nil
	}

	if _, err := os.Stat(st.WorktreePath); err != nil {
		return Thread{}, fmt.Errorf("thread: worktree for %q is unavailable: %w", idOrName, err)
	}

	handle, err := m.spawner.Spawn(m.ctx, st.WorktreePath)
	var rb unwinder
	defer rb.unwind()
	if handle != nil {
		rb.push(func() {
			cleanupCtx, cancel := m.detachForRollback(ctx)
			defer cancel()
			if err := m.spawner.Release(cleanupCtx, handle.ID()); err != nil {
				c.mu.Lock()
				c.runtime = &runtimeState{handle: handle, spawner: m.spawner, watchCancel: func() {}, releaseFailed: true}
				c.mu.Unlock()
				slog.Error("Failed to release activated workspace", "thread", st.ID, "error", err)
			}
		})
	}
	if err != nil {
		return Thread{}, fmt.Errorf("thread: respawn workspace: %w", err)
	}
	if err := m.ctx.Err(); err != nil {
		return Thread{}, err
	}

	// Preserve the earlier run's outcome: SetStatus rewrites all four
	// columns, so the summary/error/timestamp have to be carried across
	// explicitly or reactivating would erase the record of what ran.
	st, err = m.lc.setStatus(ctx, st.ID, StatusIdle, st.Error, st.ResultSummary, st.CompletedAt)
	if err != nil {
		return Thread{}, err
	}
	m.lc.installRuntime(m.ctx, handle, m.spawner, st.ID)
	// The dispatcher's DelegationParent registry lives per coordinator
	// instance and is empty on a freshly-started process (see
	// resolveDeliveryTarget's doc comment on the persisted column it reads
	// from). Re-register here, on the freshly-installed handle, so a
	// thread resumed after a restart can still ask its parent mid-run — not
	// just report its eventual completion.
	// Depth is not persisted (a pre-existing gap - see threadControl.depth,
	// also in-memory-only); 0 is the safe default a resumed entity's
	// cascade depth already silently falls back to today.
	m.registerThreadParent(handle, st)
	rb.commit()
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
func (m *Manager) Cancel(ctx context.Context, idOrName, reason string) error {
	// See TaskManager.Cancel: the resolve below is part of the terminal
	// work and has to survive the context the cancel arrived on.
	ctx, released := detachForTerminalWork(ctx)
	defer released()
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
	return m.lc.cancel(ctx, st, reason)
}

// Send re-dispatches message into a thread's session, resuming it if its
// workspace is not currently spawned (e.g. after an interrupted run — the
// worktree is still on disk, so the workspace is simply respawned).
//
// The returned [SendDisposition] tells the caller whether the thread's
// agent picks the message up now or only after the turn it is currently
// running — see the type's own doc for why that difference matters enough
// to report.
//
// This is the agent-facing entry (thread_send); [Manager.SendFromPerson]
// is the person's own.
func (m *Manager) Send(ctx context.Context, idOrName, message string) (SendDisposition, error) {
	return m.send(ctx, idOrName, message, SenderAgent, nil)
}

// SendFromPerson is [Manager.Send] for a message the person typed
// themselves, in the TUI's view of the thread's session. It differs in the
// only two ways authorship can matter: the message is persisted as their
// own words rather than as an agent dispatch, and it folds into the
// thread's turn in flight instead of queueing behind it — see
// [lifecycle.steer] for why the person gets that and another agent does
// not.
func (m *Manager) SendFromPerson(ctx context.Context, idOrName, message string) (SendDisposition, error) {
	return m.send(ctx, idOrName, message, SenderPerson, nil)
}

// RunFromPerson dispatches a turn the person is driving by hand in the
// thread's own session — the TUI drilled into a thread and typed. It is
// [Manager.SendFromPerson] plus attachments, and it exists as its own
// entry point because of what it is for: routing this typing through the
// manager, rather than straight to the thread's coordinator, makes it the
// one owner of every turn in a thread's session — without that, the
// manager never learns a turn started, an untracked run has no RunID to
// match on completion, and a thread revived by hand could never settle or
// report again.
//
// What it does not do is treat such a turn as the thread's work being
// finished: it rests at idle with its workspace live. See
// lifecycle.handleRunComplete's person branch.
func (m *Manager) RunFromPerson(ctx context.Context, idOrName, message string, attachments []Attachment) (SendDisposition, error) {
	return m.send(ctx, idOrName, message, SenderPerson, attachments)
}

func (m *Manager) send(ctx context.Context, idOrName, message string, from Sender, attachments []Attachment) (SendDisposition, error) {
	done, err := m.lc.beginOp()
	if err != nil {
		return SendDisposition{}, err
	}
	defer done()
	st, err := m.resolve(ctx, idOrName)
	if err != nil {
		return SendDisposition{}, err
	}
	if st.Kind != KindThread {
		return SendDisposition{}, fmt.Errorf("thread: %q is not a thread", idOrName)
	}
	// Checked before either branch below, not inside the respawn one:
	// a thread whose worktree is gone has nowhere to do the work being
	// sent, whether or not its App still happens to be up. lifecycle.send
	// can't host this check, since TaskManager.Send shares that code with
	// an empty spawnPath. Without it, a failed thread whose worktree the
	// unwinder already removed (failCreate) resurrects through
	// Bootstrap's deliberately permissive MkdirAll and starts running
	// detached from git, in a directory that shouldn't exist. See
	// Activate's identical check.
	if _, err := os.Stat(st.WorktreePath); err != nil {
		return SendDisposition{}, fmt.Errorf("thread: worktree for %q is unavailable: %w", idOrName, err)
	}
	// The "queue into a live runtime" / "respawn from spawnPath, then
	// dispatch" logic itself has nothing thread-specific in it — see
	// lifecycle.send's doc comment — so it lives there, shared with
	// TaskManager.Send. beforeDispatch re-registers the parent link (and,
	// via registerThreadParent, the auto-approval grant) on whichever
	// handle ends up dispatching — the live one or a freshly respawned
	// one — right before the run is actually dispatched, so a headless
	// follow-up's first permission request never races a grant that
	// hasn't landed yet. TaskManager.Send passes a hook of its own for
	// the parent link; it needs no grant, sharing its parent's App and
	// therefore its permission service.
	disp, err := m.lc.send(ctx, m.ctx, st.ID, m.spawner, st.WorktreePath, st.SessionID, message, from, attachments, func(handle Handle) {
		m.registerThreadParent(handle, st)
	})
	if err != nil {
		return SendDisposition{}, err
	}
	return disp, nil
}

// Remove tears a thread down: it cancels and releases its workspace (if
// force), removes its git worktree (and branch, if deleteBranch), and
// deletes its store row. It refuses active threads unless force, and
// refuses threads with a dirty worktree unless force.
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
	if st.Kind != KindThread && st.WorktreePath == "" {
		return fmt.Errorf("thread: %q is not a thread", idOrName)
	}

	if st.Status == StatusRunning && !force {
		return fmt.Errorf("thread: %q is active (status=%s); use force to remove", st.Name, st.Status)
	}
	if dirty, err := git.IsDirty(ctx, st.WorktreePath); err == nil && dirty && !force {
		return fmt.Errorf("thread: %q has uncommitted changes; use force to remove", st.Name)
	}
	// Checked here, before anything below tears down the live workspace:
	// this reads only m.repoRoot and st's own Branch/BaseBranch, so it
	// costs nothing to ask first, and a refusal here must leave the
	// runtime — and the screen attached to it — untouched. Asking after
	// the teardown below made a refusal here indistinguishable from
	// success to the person still looking at that screen: the row stayed
	// idle (workspace is live, per the type's own contract) while the App
	// backing it was already dead, so the screen stopped updating and the
	// next input would have spawned a new App whose events it could never
	// see.
	if deleteBranch && !force {
		if merged, err := git.IsAncestor(ctx, m.repoRoot, st.Branch, st.BaseBranch); err != nil {
			return fmt.Errorf("thread: check branch merge state: %w", err)
		} else if !merged {
			return fmt.Errorf("thread: delete branch: %q is not merged into %q; use force to remove", st.Branch, st.BaseBranch)
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
		// Cancel this delegation's own session, not the whole workspace's
		// coordinator. Only on force:
		// an unforced remove has already refused any active status above,
		// so there is nothing left running to cancel.
		if err := releaseRuntime(ctx, rt, st.SessionID, force); err != nil {
			c.mu.Lock()
			rt.releaseFailed = true
			c.runtime = rt
			c.removed = false
			c.mu.Unlock()
			return fmt.Errorf("thread: release workspace before removal: %w", err)
		}
	}

	// c.removed was set above so a Create racing this teardown cannot
	// resurrect the entity mid-removal. It has to come back off if the
	// removal then fails: the row is still there and still resolvable,
	// but every later operation consults this flag and answers "has been
	// removed" — a thread that could not be deleted became one nothing
	// could touch either, with no way back short of a restart.
	abort := func(err error) error {
		c.mu.Lock()
		c.removed = false
		c.mu.Unlock()
		return err
	}

	// Branch-merge state was already checked above, before teardown; force
	// deletion still removes the worktree first because git cannot delete a
	// branch currently checked out by one.
	if err := git.WorktreeRemove(ctx, m.repoRoot, st.WorktreePath, force); err != nil {
		return abort(fmt.Errorf("thread: remove worktree: %w", err))
	}
	if deleteBranch {
		if err := git.DeleteBranch(ctx, m.repoRoot, st.Branch, force); err != nil {
			return abort(fmt.Errorf("thread: delete branch: %w", err))
		}
	}
	if err := m.store.Delete(ctx, st.ID); err != nil {
		return abort(fmt.Errorf("thread: delete record: %w", err))
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

// QuestionServices returns the question service of every delegation
// currently live under this manager, for routing an answer whose batch ID
// does not by itself say which delegation raised it.
//
// question.Request carries no delegation tag the way permission.Request
// carries Delegation, so unlike PermissionsFor this cannot look one
// service up by ID — it hands back every candidate and lets the caller try
// each (see answerPermission in appws, which the caller reuses): a
// service that is not holding the given batch ID does nothing at all, so
// trying the wrong ones first is wasted work, never a wrong answer.
func (m *Manager) QuestionServices() []question.Service {
	controls := m.lc.snapshotControls()
	services := make([]question.Service, 0, len(controls))
	for _, c := range controls {
		c.mu.Lock()
		rt := c.runtime
		c.mu.Unlock()
		if rt == nil {
			continue
		}
		a := rt.handle.Workspace()
		if a == nil || a == m.parentApp {
			continue
		}
		services = append(services, a.Questions())
	}
	return services
}

// Shutdown stops admission, cancels manager work, releases live runtimes, and
// waits for manager-owned goroutines. It is idempotent and safe concurrently.
//
// m.cancel cancels m.ctx, the base context every runtime's watch loop and
// in-flight run derive from — including a [TaskManager]'s, if one was
// constructed sharing this Manager's lifecycle and ctx (see NewManager),
// since m.lc.snapshotControls below walks that same shared controls map
// regardless of which kind registered each entry.
//
// The cleanup below runs on its own context rather than ctx, since ctx
// belongs to whichever caller happens to be waiting and m.ctx is already
// cancelled by the time the goroutine starts.
func (m *Manager) retryShutdownReleases(ctx context.Context) error {
	var failures []error
	for id, control := range m.lc.snapshotControls() {
		control.opMu.Lock()
		control.mu.Lock()
		runtime := control.runtime
		control.mu.Unlock()
		if runtime != nil && runtime.releaseFailed {
			if err := releaseRuntime(ctx, runtime, "", false); err != nil {
				failures = append(failures, fmt.Errorf("release workspace %s: %w", id, err))
			} else {
				control.mu.Lock()
				control.runtime = nil
				control.mu.Unlock()
			}
		}
		control.opMu.Unlock()
	}
	return errors.Join(failures...)
}

func (m *Manager) Shutdown(ctx context.Context) error {
	m.shutdownOnce.Do(func() {
		shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
		m.shutdownCancel = shutdownCancel
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
					// Fetched up front, before Cancel: this entity's own
					// SessionID is what Cancel needs (a task's App is its
					// parent's, so CancelAll here would cancel unrelated
					// work), and the status check below reuses the same
					// row instead of reading it twice. If the row can't be
					// read, skip both — there is no unrelated-work-safe
					// fallback for "cancel something, we don't know what".
					st, getErr := m.store.Get(shutdownCtx, threadID)
					if err := releaseRuntime(shutdownCtx, rt, st.SessionID, getErr == nil); err != nil {
						c.mu.Lock()
						rt.releaseFailed = true
						c.runtime = rt
						c.mu.Unlock()
						slog.Error("Failed to release workspace on shutdown", "component", "thread", "error", err)
					}
					// The workspace DB remains live until this method returns
					// to its cleanup caller, so this normally records the
					// interrupted terminal state before the connection is
					// released. But if every waiting caller has given up,
					// shutdownCtx is cancelled and this write is skipped —
					// the run is being abandoned, not tidily finalized.
					if getErr == nil && st.Status == StatusRunning {
						if st.Kind == KindTask {
							final, err := m.lc.finalizeTask(shutdownCtx, st, StatusInterrupted, "", "", 0, st.CompletionDepth)
							if err != nil {
								slog.Error("Failed to finalize task on shutdown", "component", "thread", "id", st.ID, "error", err)
							} else if final.ID != "" {
								m.lc.deliverStoredCompletion(shutdownCtx, rt.handle, final, final.CompletionDepth)
							}
						} else if _, err := m.lc.setStatus(shutdownCtx, st.ID, StatusInterrupted, "", "", 0); err != nil {
							slog.Error("Failed to interrupt delegation on shutdown", "component", "thread", "id", st.ID, "error", err)
						}
					}
				}
				c.opMu.Unlock()
			}
			close(m.shutdownDone)
		}()
	})
	m.shutdownMu.Lock()
	m.shutdownWaiters++
	m.shutdownMu.Unlock()
	defer func() {
		m.shutdownMu.Lock()
		m.shutdownWaiters--
		if m.shutdownWaiters == 0 {
			m.shutdownCancel()
		}
		m.shutdownMu.Unlock()
	}()
	select {
	case <-m.shutdownDone:
		return m.retryShutdownReleases(ctx)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Recover reconciles store state against reality after a process restart:
// threads left pending or running (their goroutines are gone with the old
// process) become interrupted, and threads whose worktree has
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
// lifecycle.recover. A thread already StatusFailed is left alone because
// re-failing it would be a no-op. A Stat error other than "not exist" (e.g. a
// permission problem) is treated as "worktree present" and falls through
// to the generic sweep, matching the pre-hook behavior.
func (m *Manager) recoverWorktree(ctx context.Context, st Thread) (bool, error) {
	// This hook only knows how to judge threads: a worktree check makes no
	// sense for a delegation kind with no worktree, and an empty
	// WorktreePath would otherwise read as "missing" and get marked
	// failed before the generic active-status sweep ever sees it.
	if st.Kind != KindThread && st.WorktreePath == "" {
		return false, nil
	}
	if _, statErr := os.Stat(st.WorktreePath); !os.IsNotExist(statErr) {
		return false, nil
	}
	if st.Status == StatusFailed {
		return true, nil
	}
	if st.Kind == KindTask && st.Status.Active() {
		final, err := m.lc.finalizeTask(ctx, st, StatusFailed, "worktree missing on recovery", "", 0, st.CompletionDepth)
		if err != nil {
			return false, err
		}
		if final.ID != "" {
			m.lc.deliverStoredCompletion(ctx, nil, final, final.CompletionDepth)
		}
		return true, nil
	}
	if _, err := m.lc.setStatus(ctx, st.ID, StatusFailed, "worktree missing on recovery", "", 0); err != nil {
		return false, err
	}
	return true, nil
}

// resolveDeliveryTarget is the lifecycle's deliveryResolver hook (see
// lifecycle.deliverStoredCompletion, the only caller): it finds where a
// delegation's terminal completion should go, branching on Kind the same
// way onCompletedCleanup and recoverWorktree do, since both kinds sharing this
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
		// Recovery has no runtime handle; live delivery keeps supporting test
		// and alternate managers that do not configure ParentApp explicitly.
		if m.parentApp != nil {
			return m.parentApp, st.ParentSessionID, true
		}
		if handle == nil || handle.Workspace() == nil {
			return nil, "", false
		}
		return handle.Workspace(), st.ParentSessionID, true
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
// ids is empty) are pending or running, or until ctx is
// canceled or timeout elapses (timeout <= 0 means no timeout beyond ctx).
func (m *Manager) Wait(ctx context.Context, ids []string, timeout time.Duration) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	for {
		// Take the change channel *before* reading status, not after: a
		// transition landing between the read and the select closes the
		// channel this iteration would otherwise never look at, and
		// notifyChange immediately installs a fresh one. Waiting on that
		// fresh channel means waiting for the *next* transition — which,
		// for a thread that just reached a terminal status, never comes,
		// so Wait blocks until its own deadline. Holding the channel
		// first makes the wakeup impossible to miss: any change after
		// this point closes exactly the channel being waited on.
		changed := m.lc.waitChan()

		active, err := m.anyActive(ctx, ids)
		if err != nil {
			return err
		}
		if !active {
			return nil
		}
		select {
		case <-changed:
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
// store.ListAll: Manager's whole public surface and Wait's own doc comment
// are stated in terms of threads, so "wait for everything"
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
			// A thread that is no longer there finished and was cleaned up or
			// was explicitly removed. Waiting for
			// it is satisfied, not failed — reporting not-found turned
			// "wait for these three" into an error whenever one of them
			// completed while the call was blocked, which is the very
			// thing being waited for.
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, err
		}
		threads = append(threads, st)
	}
	return threads, nil
}

// registerThreadParent registers st's parent link on handle's own
// coordinator, guarded on m.parentApp being configured at all — see
// registerParent's own doc comment for why every field but depth comes
// from st, and depth is always 0 here: the cascade limiter this stamps for
// a resumed entity only ever matters for tasks created through the "agent"
// tool, never for a thread.
//
// It also carries the auto-approval grant across, for the same reason it
// registers the parent link at all: this is the one place every path that
// installs a runtime for a thread — Create, Activate, and lifecycle.send's
// beforeDispatch hook (covering both the live-runtime and the
// respawn-after-Spawn branches) — funnels through. A thread's own
// workspace carries a permission service wholly separate from its
// parent's — see ManagerOptions.ParentApp — so the blanket grant a
// headless run gives its own session (see
// permission.Service.AutoApproveSession) does not reach a thread it
// dispatches; without this, the thread's first permission request blocks
// forever with no UI subscribed to answer it, the same deadlock
// TaskManager.Create closes for a task delegation (see
// permission_inheritance_test.go). Extending the grant here, keyed on the
// parent already holding it, is the thread analogue of that same fix:
// both sides of the propagation are session-scoped, not
// directory-scoped, so crossing from the parent's service to the
// thread's own is granting the same thing the thread's own service would
// otherwise be asked to prompt for. Granting on every install (not just
// the first) is what makes the grant survive a runtime release: the
// thread's App — and the permission service living in it — is rebuilt
// from scratch each time (finalizeRunComplete → releaseRuntime →
// LocalSpawner.Release → App.Shutdown), so nothing about the previous
// App's grant carries forward on its own; AutoApproveSession is
// idempotent, so re-granting here on an already-approved session is
// harmless.
func (m *Manager) registerThreadParent(handle Handle, st Thread) {
	if m.parentApp == nil {
		return
	}
	coord := handle.Workspace().Coordinator()
	registerParent(coord, m.parentApp.Coordinator(), st, 0)

	// A thread runs in an App instance of its own, and that instance
	// works in one session the same way any other does - this one,
	// created because the person's session asked for a thread. Saying so
	// is what makes it wake-eligible over there; without it a thread that
	// ended a turn waiting on delegations of its own would park at
	// StatusRunning forever. It grants nothing to the workspace the
	// person is working in, whose coordinator is a different one
	// entirely.
	if coord != nil {
		coord.SetLiveSession(st.SessionID)
	}

	if st.ParentSessionID != "" {
		if parentPerms := m.parentApp.Permissions(); parentPerms != nil && parentPerms.IsAutoApproveSession(st.ParentSessionID) {
			if perms := handle.Workspace().Permissions(); perms != nil {
				perms.AutoApproveSession(st.SessionID)
			}
		}
	}
}

// registerParent installs st's parent link on registerOn — the coordinator
// that will actually dispatch its turns — so a mid-run ask or its eventual
// completion reaches st.ParentSessionID through parentCoord. Every field it
// registers besides depth (session id, delegation id, name, kind, parent
// session id) already lives on st by the time any call site has one to
// pass, so it reads them from there instead of asking five callers to repeat
// them positionally. depth stays a separate parameter because it is not
// part of Thread/Delegation — see threadControl.depth's doc comment.
//
// It is a no-op when there is nothing to route to yet: registerOn nil (no
// runtime installed), parentCoord nil (no delivery target — for a thread,
// see ManagerOptions.ParentApp; a task always has one, its own coordinator),
// or st.ParentSessionID empty (a thread with no parent — see
// CreateArgs.ParentSessionID; a task's is always non-empty, enforced at
// TaskManager.Create).
func registerParent(registerOn, parentCoord Coordinator, st Thread, depth int) {
	if registerOn == nil || parentCoord == nil || st.ParentSessionID == "" {
		return
	}
	registerOn.RegisterDelegationParent(st.SessionID, DelegationParent{
		Parent:          parentCoord,
		ParentSessionID: st.ParentSessionID,
		DelegationID:    st.ID,
		Kind:            string(st.Kind),
		Name:            st.Name,
		Depth:           depth,
	})
}

func validateName(name string) (string, error) {
	if !nameRe.MatchString(name) {
		return "", fmt.Errorf("thread: invalid name %q: must be a lowercase alphanumeric slug (hyphens allowed, not leading or trailing)", name)
	}
	return name, nil
}
