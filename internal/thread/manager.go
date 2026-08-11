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
	"github.com/rave-soft/braid/internal/agent"
	"github.com/rave-soft/braid/internal/agent/notify"
	"github.com/rave-soft/braid/internal/git"
	"github.com/rave-soft/braid/internal/pubsub"
)

// nameRe restricts thread names to values safe to embed in a branch name
// and a worktree directory: lowercase alphanumeric slugs, hyphen
// separated, not leading/trailing with a hyphen.
var nameRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)

// CreateArgs holds the inputs to [Manager.Create]. BaseBranch defaults to
// the repository's currently checked-out branch when empty; MergePolicy
// defaults to [MergeAuto].
type CreateArgs struct {
	Name        string
	Goal        string
	BaseBranch  string
	MergePolicy MergePolicy
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

// runtimeState tracks the in-memory bookkeeping for a thread whose
// workspace is currently spawned: the handle to release on removal, and
// the cancel function for its RunComplete watcher goroutine.
type runtimeState struct {
	handle      Handle
	watchCancel context.CancelFunc
	runID       string
}

// threadControl is permanent while a thread is known to the manager. opMu
// serializes lifecycle operations for one thread without serializing unrelated
// threads or holding the manager map lock across I/O.
type threadControl struct {
	opMu    sync.Mutex
	mu      sync.Mutex
	runtime *runtimeState
	removed bool
}

// ErrManagerClosed is returned by mutating manager operations once shutdown
// has started.
var ErrManagerClosed = errors.New("thread: manager is closed")

// Manager is the core of the threads feature: it drives thread creation,
// dispatches and tracks each thread's agent run in its isolated
// workspace, and folds completed work back into the base branch.
type Manager struct {
	store       Store
	spawner     Spawner
	repoRoot    string
	worktreeDir string
	ctx         context.Context
	cancel      context.CancelFunc

	broker *pubsub.Broker[Event]

	mu           sync.Mutex
	controls     map[string]*threadControl
	closed       bool
	shutdownOnce sync.Once
	shutdownDone chan struct{}
	workers      sync.WaitGroup

	// changeCh is closed and replaced on every status-affecting event,
	// giving Wait a broadcast condition to select on without polling.
	changeMu sync.Mutex
	changeCh chan struct{}
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
		store:        opts.Store,
		spawner:      opts.Spawner,
		repoRoot:     opts.RepoRoot,
		worktreeDir:  worktreeDir,
		ctx:          ctx,
		broker:       pubsub.NewBroker[Event](),
		controls:     make(map[string]*threadControl),
		shutdownDone: make(chan struct{}),
		changeCh:     make(chan struct{}),
	}
	m.ctx, m.cancel = context.WithCancel(ctx)
	return m
}

// beginOp admits one mutating operation. Shutdown closes admission and then
// waits for this count, so no operation can attach a runtime after teardown.
func (m *Manager) beginOp() (func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrManagerClosed
	}
	m.workers.Add(1)
	return m.workers.Done, nil
}

func (m *Manager) control(threadID string) *threadControl {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.controls[threadID]
	if c == nil {
		c = &threadControl{}
		m.controls[threadID] = c
	}
	return c
}

func (m *Manager) existingControl(threadID string) *threadControl {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.controls[threadID]
}

// goWorker is called only while an admitted operation or existing worker is
// counted. Consequently its Add cannot race Shutdown's Wait after zero.
func (m *Manager) goWorker(fn func()) {
	m.workers.Add(1)
	go func() { defer m.workers.Done(); fn() }()
}

// Subscribe returns a per-caller channel of thread lifecycle events.
func (m *Manager) Subscribe(ctx context.Context) <-chan pubsub.Event[Event] {
	return m.broker.Subscribe(ctx)
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
func (m *Manager) Create(ctx context.Context, args CreateArgs) (Thread, error) {
	done, err := m.beginOp()
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
	m.publish(EventCreated, st)

	// The thread is resolvable from here on, so a concurrent Remove can
	// race the rest of creation. Hold the per-thread lifecycle lock across
	// worktree/spawn/startRun: Remove takes the same lock, so it either
	// runs before this point (nothing beyond the row exists yet) or waits
	// until the runtime is fully installed and tears it down normally.
	c := m.control(st.ID)
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

	sess, err := handle.App().Sessions.Create(ctx, args.Goal)
	if err != nil {
		m.abortSpawn(ctx, handle, worktreePath)
		return Thread{}, m.failCreate(ctx, st, err)
	}

	st, err = m.store.SetSession(ctx, st.ID, sess.ID)
	if err != nil {
		m.abortSpawn(ctx, handle, worktreePath)
		return Thread{}, m.failCreate(ctx, st, err)
	}

	st, err = m.setStatus(ctx, st.ID, StatusRunning, "", "", 0)
	if err != nil {
		m.abortSpawn(ctx, handle, worktreePath)
		return Thread{}, err
	}
	m.startRun(handle, st.ID, st.SessionID, args.Goal)

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
	if _, err := m.setStatus(ctx, st.ID, StatusFailed, cause.Error(), "", 0); err != nil {
		slog.Error("thread: recording create failure failed", "thread", st.ID, "error", err)
	}
	return cause
}

// startRun registers handle as the running workspace for threadID and
// starts its RunComplete watcher goroutine. The subscription itself is
// established synchronously, before startRun returns: callers (Create,
// Send) dispatch the agent run right after startRun returns, and a
// RunComplete published before the subscription is registered would
// otherwise be silently missed by the broker's fan-out.
//
// Callers must hold the thread's opMu: installing c.runtime is a
// lifecycle transition, and without the lock a concurrent Remove could
// observe "no runtime", delete the thread, and leave this runtime
// stranded on a thread that no longer exists.
func (m *Manager) startRun(handle Handle, threadID, sessionID, prompt string) {
	c := m.control(threadID)
	runID := uuid.NewString()
	watchCtx, cancel := context.WithCancel(m.ctx)
	sub := handle.App().RunCompletions().Subscribe(watchCtx)
	c.mu.Lock()
	c.runtime = &runtimeState{handle: handle, watchCancel: cancel, runID: runID}
	c.mu.Unlock()

	m.goWorker(func() {
		for {
			select {
			case <-watchCtx.Done():
				return
			case ev, ok := <-sub:
				if !ok {
					return
				}
				m.onRunComplete(threadID, ev.Payload)
			}
		}
	})

	// Reserve acceptance before dispatch so cancellation cannot leave a run
	// unaccounted for between goroutine scheduling and coordinator admission.
	accept := handle.App().AgentCoordinator.BeginAccepted(sessionID)
	m.goWorker(func() {
		if _, err := handle.App().AgentCoordinator.RunAccepted(agent.WithRunID(m.ctx, runID), accept, sessionID, prompt); err != nil {
			slog.Error("thread: agent run returned an error", "session_id", sessionID, "error", err)
			// backend.runAgent documents this fallback for pre-execution
			// failures. Local coordinators do not provide that wrapper.
			m.onRunComplete(threadID, notify.RunComplete{SessionID: sessionID, RunID: runID, Error: err.Error(), Cancelled: errors.Is(err, context.Canceled)})
		}
	})
}

// onRunComplete is the RunComplete handler: it reacts to the authoritative
// end-of-run signal for a thread's session by recording the outcome and,
// on success with an auto merge policy, kicking off the merge flow.
func (m *Manager) onRunComplete(threadID string, rc notify.RunComplete) {
	c := m.existingControl(threadID)
	if c == nil {
		return
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.mu.Lock()
	rt := c.runtime
	if rt == nil || rc.RunID == "" || rc.RunID != rt.runID {
		c.mu.Unlock()
		return
	}
	c.runtime = nil
	c.mu.Unlock()
	rt.watchCancel()
	if err := m.spawner.Release(m.ctx, rt.handle.ID()); err != nil {
		slog.Error("thread: release completed workspace failed", "thread", threadID, "error", err)
	}
	st, err := m.store.Get(m.ctx, threadID)
	if err != nil {
		return
	}
	// Only react to the session this thread currently owns, and only
	// while a run is actually in flight: Remove or a completed merge can
	// race a straggling RunComplete from a run that no longer matters.
	if rc.SessionID != st.SessionID || st.Status != StatusRunning {
		return
	}

	if rc.Cancelled {
		if _, err := m.setStatus(m.ctx, threadID, StatusInterrupted, "", "", 0); err != nil {
			slog.Error("thread: recording run cancellation failed", "thread", threadID, "error", err)
		}
		return
	}
	if rc.Error != "" {
		if _, err := m.setStatus(m.ctx, threadID, StatusFailed, rc.Error, "", 0); err != nil {
			slog.Error("thread: recording run failure failed", "thread", threadID, "error", err)
		}
		return
	}

	// Auto-merge threads go straight from running into the merge flow
	// without resting at "completed" in between: setting a terminal
	// "completed" status first would give [Manager.Wait] a window where
	// it observes a non-active status and returns before the merge that
	// is about to start has even begun. Manual threads have no such
	// follow-up, so they rest at "completed" for real.
	if st.MergePolicy == MergeAuto {
		m.goWorker(func() {
			c.opMu.Lock()
			defer c.opMu.Unlock()
			if err := m.mergeAttempt(m.ctx, threadID, true, rc.Text); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("thread: auto-merge failed", "thread", threadID, "error", err)
			}
		})
		return
	}

	if _, err := m.setStatus(m.ctx, threadID, StatusCompleted, "", rc.Text, time.Now().Unix()); err != nil {
		slog.Error("thread: recording run completion failed", "thread", threadID, "error", err)
	}
}

// Send re-dispatches message into a thread's session, resuming it if its
// workspace is not currently spawned (e.g. after an interrupted run — the
// worktree is still on disk, so the workspace is simply respawned).
func (m *Manager) Send(ctx context.Context, idOrName, message string) error {
	done, err := m.beginOp()
	if err != nil {
		return err
	}
	defer done()
	st, err := m.resolve(ctx, idOrName)
	if err != nil {
		return err
	}

	c := m.control(st.ID)
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.mu.Lock()
	rt := c.runtime
	removed := c.removed
	c.mu.Unlock()
	if removed {
		return fmt.Errorf("thread: %q has been removed", idOrName)
	}

	if rt != nil {
		// A run is already in flight. Queue the follow-up as its own
		// RunID-bearing turn (the dispatcher gives every RunID-bearing
		// queued prompt its own turn and terminal RunComplete) and hand
		// workspace ownership to it: rt.runID is advanced under c.mu, so
		// the in-flight run's completion no longer matches in
		// onRunComplete and cannot release the workspace out from under
		// the queued turn.
		st, err = m.setStatus(ctx, st.ID, StatusRunning, "", "", 0)
		if err != nil {
			return err
		}
		runID := uuid.NewString()
		c.mu.Lock()
		rt.runID = runID
		c.mu.Unlock()
		sessionID := st.SessionID
		m.goWorker(func() {
			if _, err := rt.handle.App().AgentCoordinator.Run(agent.WithRunID(m.ctx, runID), sessionID, message); err != nil {
				slog.Error("thread: queued agent run returned an error", "session_id", sessionID, "error", err)
				// Mirror startRun's fallback for pre-execution failures
				// so the workspace is not stranded on a run that never
				// published its own RunComplete.
				m.onRunComplete(st.ID, notify.RunComplete{SessionID: sessionID, RunID: runID, Error: err.Error(), Cancelled: errors.Is(err, context.Canceled)})
			}
		})
		return nil
	}

	handle, err := m.spawner.Spawn(m.ctx, st.WorktreePath)
	if err != nil {
		return fmt.Errorf("thread: respawn workspace: %w", err)
	}
	if err := m.ctx.Err(); err != nil {
		_ = m.spawner.Release(context.Background(), handle.ID())
		return err
	}
	// This call owns the freshly spawned handle until startRun installs it
	// as the manager runtime; release it on every earlier exit.
	owned := true
	defer func() {
		if owned {
			_ = m.spawner.Release(ctx, handle.ID())
		}
	}()

	st, err = m.setStatus(ctx, st.ID, StatusRunning, "", "", 0)
	if err != nil {
		return err
	}
	m.startRun(handle, st.ID, st.SessionID, message)
	owned = false // Ownership transferred to the manager runtime state.
	return nil
}

// Merge runs (or retries) the merge flow for a thread. Manual-policy
// threads are merged this way once their run completes; auto-policy
// threads use this to retry after a conflict has been resolved.
func (m *Manager) Merge(ctx context.Context, idOrName string) error {
	done, err := m.beginOp()
	if err != nil {
		return err
	}
	defer done()
	st, err := m.resolve(ctx, idOrName)
	if err != nil {
		return err
	}
	// Empty resultSummary tells mergeAttempt to keep whatever is already
	// on the row (e.g. from the run that led to the current conflict or
	// merge_blocked state) instead of clobbering it.
	c := m.control(st.ID)
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

	st, err = m.setStatus(ctx, threadID, StatusMerging, "", resultSummary, 0)
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
	st, err := m.setStatus(ctx, threadID, StatusMerged, "", resultSummary, time.Now().Unix())
	if err != nil {
		return err
	}
	m.publish(EventMerged, st)
	return nil
}

func (m *Manager) setConflict(ctx context.Context, threadID, resultSummary string, conflicts []string) error {
	_, err := m.setStatus(ctx, threadID, StatusConflict, "merge conflicts: "+strings.Join(conflicts, ", "), resultSummary, 0)
	return err
}

func (m *Manager) blockMerge(ctx context.Context, threadID, resultSummary, reason string) error {
	_, err := m.setStatus(ctx, threadID, StatusMergeBlocked, reason, resultSummary, 0)
	return err
}

// Remove tears a thread down: it cancels and releases its workspace (if
// force), removes its git worktree (and branch, if deleteBranch), and
// deletes its store row. It refuses to run/merging threads unless force,
// and refuses unmerged threads with a dirty worktree unless force.
func (m *Manager) Remove(ctx context.Context, idOrName string, force, deleteBranch bool) error {
	done, err := m.beginOp()
	if err != nil {
		return err
	}
	defer done()
	st, err := m.resolve(ctx, idOrName)
	if err != nil {
		return err
	}

	if (st.Status == StatusRunning || st.Status == StatusMerging) && !force {
		return fmt.Errorf("thread: %q is active (status=%s); use force to remove", st.Name, st.Status)
	}
	if st.Status != StatusMerged {
		if dirty, err := git.IsDirty(ctx, st.WorktreePath); err == nil && dirty && !force {
			return fmt.Errorf("thread: %q has unmerged, uncommitted changes; use force to remove", st.Name)
		}
	}

	c := m.control(st.ID)
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
			if a := rt.handle.App(); a != nil && a.AgentCoordinator != nil {
				a.AgentCoordinator.CancelAll()
			}
		}
		if err := m.spawner.Release(ctx, rt.handle.ID()); err != nil {
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
	m.publish(EventRemoved, st)
	return nil
}

// Handle returns the spawned workspace handle for threadID, or nil if the
// thread's workspace is not currently spawned (e.g. it finished, or is
// between runs after an interrupt).
func (m *Manager) Handle(threadID string) Handle {
	m.mu.Lock()
	c := m.controls[threadID]
	m.mu.Unlock()
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
func (m *Manager) Shutdown(ctx context.Context) error {
	m.shutdownOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.mu.Unlock()
		m.cancel()
		go func() {
			// beginOp/Add are serialized with closed above. Existing workers
			// only add children before their own Done, so this join cannot
			// observe zero before all manager work has finished.
			m.workers.Wait()
			m.mu.Lock()
			controls := make(map[string]*threadControl, len(m.controls))
			for id, c := range m.controls {
				controls[id] = c
			}
			m.mu.Unlock()
			for threadID, c := range controls {
				c.opMu.Lock()
				c.mu.Lock()
				rt := c.runtime
				c.runtime = nil
				c.mu.Unlock()
				if rt != nil {
					rt.watchCancel()
					if a := rt.handle.App(); a != nil && a.AgentCoordinator != nil {
						a.AgentCoordinator.CancelAll()
					}
					if err := m.spawner.Release(context.Background(), rt.handle.ID()); err != nil {
						slog.Error("thread: release workspace on shutdown failed", "error", err)
					}
					// The workspace DB remains live until this method returns to
					// its cleanup caller, so record the interrupted terminal
					// state before the connection is released.
					if st, err := m.store.Get(context.Background(), threadID); err == nil && st.Status == StatusRunning {
						_, _ = m.setStatus(context.Background(), st.ID, StatusInterrupted, "", "", 0)
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
	done, err := m.beginOp()
	if err != nil {
		return err
	}
	defer done()
	threads, err := m.store.List(ctx)
	if err != nil {
		return err
	}
	for _, st := range threads {
		if _, statErr := os.Stat(st.WorktreePath); os.IsNotExist(statErr) {
			if st.Status == StatusFailed || st.Status == StatusMerged {
				continue
			}
			if _, err := m.setStatus(ctx, st.ID, StatusFailed, "worktree missing on recovery", "", 0); err != nil {
				return err
			}
			continue
		}
		if st.Status.Active() {
			if _, err := m.setStatus(ctx, st.ID, StatusInterrupted, "", "", 0); err != nil {
				return err
			}
		}
	}
	return nil
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
		case <-m.waitChan():
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

// publish emits a lifecycle event and wakes any Wait callers blocked on a
// status change.
func (m *Manager) publish(t EventType, st Thread) {
	m.broker.Publish(pubsub.UpdatedEvent, Event{Type: t, Thread: st})
	m.notifyChange()
}

// setStatus is the shared SetStatus + publish helper used by every status
// transition in this file.
func (m *Manager) setStatus(ctx context.Context, threadID string, status Status, errText, resultSummary string, completedAt int64) (Thread, error) {
	st, err := m.store.SetStatus(ctx, threadID, SetStatusParams{
		Status:        status,
		Error:         errText,
		ResultSummary: resultSummary,
		CompletedAt:   completedAt,
	})
	if err != nil {
		return Thread{}, err
	}
	m.publish(EventStatusChanged, st)
	return st, nil
}

func (m *Manager) notifyChange() {
	m.changeMu.Lock()
	close(m.changeCh)
	m.changeCh = make(chan struct{})
	m.changeMu.Unlock()
}

func (m *Manager) waitChan() chan struct{} {
	m.changeMu.Lock()
	defer m.changeMu.Unlock()
	return m.changeCh
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
