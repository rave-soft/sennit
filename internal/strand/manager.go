// Package strand implements strands: parallel agent work streams, each
// running in its own git worktree and branch with a fully isolated
// workspace (own .braid data directory, database, and agent
// coordinator), and by default auto-merged back into its base branch on
// completion.
package strand

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

	"github.com/rave-soft/braid/internal/agent/notify"
	"github.com/rave-soft/braid/internal/git"
	"github.com/rave-soft/braid/internal/pubsub"
)

// nameRe restricts strand names to values safe to embed in a branch name
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
	// Spawner bootstraps and tears down each strand's isolated
	// workspace.
	Spawner Spawner
	// RepoRoot is the top-level directory of the git repository hosting
	// strands. The caller is responsible for constructing a Manager
	// only for git-toplevel workspaces.
	RepoRoot string
	// WorktreeDir is the parent directory under which each strand's
	// worktree is created (at WorktreeDir/<name>). Empty defaults to a
	// "<repo>-strands" sibling of RepoRoot; a relative value is resolved
	// against RepoRoot's parent directory (the same place the default
	// lives), not against the process's working directory; an absolute
	// value is used as-is.
	WorktreeDir string
	// Context is the base context background strand goroutines (agent
	// runs, RunComplete watchers) are bound to. Defaults to
	// context.Background().
	Context context.Context
}

// runtimeState tracks the in-memory bookkeeping for a strand whose
// workspace is currently spawned: the handle to release on removal, and
// the cancel function for its RunComplete watcher goroutine.
type runtimeState struct {
	handle      Handle
	watchCancel context.CancelFunc
}

// Manager is the core of the strands feature: it drives strand creation,
// dispatches and tracks each strand's agent run in its isolated
// workspace, and folds completed work back into the base branch.
type Manager struct {
	store       Store
	spawner     Spawner
	repoRoot    string
	worktreeDir string
	ctx         context.Context

	broker *pubsub.Broker[Event]

	mu      sync.Mutex
	running map[string]*runtimeState

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
		worktreeDir = filepath.Join(filepath.Dir(opts.RepoRoot), filepath.Base(opts.RepoRoot)+"-strands")
	case !filepath.IsAbs(worktreeDir):
		worktreeDir = filepath.Join(filepath.Dir(opts.RepoRoot), worktreeDir)
	}
	return &Manager{
		store:       opts.Store,
		spawner:     opts.Spawner,
		repoRoot:    opts.RepoRoot,
		worktreeDir: worktreeDir,
		ctx:         ctx,
		broker:      pubsub.NewBroker[Event](),
		running:     make(map[string]*runtimeState),
		changeCh:    make(chan struct{}),
	}
}

// Subscribe returns a per-caller channel of strand lifecycle events.
func (m *Manager) Subscribe(ctx context.Context) <-chan pubsub.Event[Event] {
	return m.broker.Subscribe(ctx)
}

// List returns every known strand.
func (m *Manager) List(ctx context.Context) ([]Strand, error) {
	return m.store.List(ctx)
}

// Get resolves idOrName (an ID or a name) to a strand.
func (m *Manager) Get(ctx context.Context, idOrName string) (Strand, error) {
	return m.resolve(ctx, idOrName)
}

// resolve looks a strand up by ID first, falling back to name.
func (m *Manager) resolve(ctx context.Context, idOrName string) (Strand, error) {
	if st, err := m.store.Get(ctx, idOrName); err == nil {
		return st, nil
	}
	return m.store.GetByName(ctx, idOrName)
}

// Create validates and dispatches a new strand: it records the strand,
// creates its git worktree and branch, spawns its isolated workspace, and
// launches its goal prompt in the background. It returns once the strand
// is running (or has failed to get there); it does not wait for the
// agent run itself to finish — subscribe or use [Manager.Wait] for that.
func (m *Manager) Create(ctx context.Context, args CreateArgs) (Strand, error) {
	name, err := validateName(args.Name)
	if err != nil {
		return Strand{}, err
	}
	if _, err := m.store.GetByName(ctx, name); err == nil {
		return Strand{}, fmt.Errorf("strand: name %q is already in use", name)
	}

	base := args.BaseBranch
	if base == "" {
		base, err = git.CurrentBranch(ctx, m.repoRoot)
		if err != nil {
			return Strand{}, fmt.Errorf("strand: resolve base branch: %w", err)
		}
	}

	branch := "strand/" + name
	if exists, err := git.BranchExists(ctx, m.repoRoot, branch); err != nil {
		return Strand{}, fmt.Errorf("strand: check branch: %w", err)
	} else if exists {
		return Strand{}, fmt.Errorf("strand: branch %q already exists", branch)
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
		return Strand{}, fmt.Errorf("strand: create record: %w", err)
	}
	m.publish(EventCreated, st)

	if err := git.WorktreeAdd(ctx, m.repoRoot, worktreePath, branch, base); err != nil {
		return m.failCreate(ctx, st, err)
	}

	handle, err := m.spawner.Spawn(ctx, worktreePath)
	if err != nil {
		_ = git.WorktreeRemove(ctx, m.repoRoot, worktreePath, true)
		return m.failCreate(ctx, st, err)
	}

	sess, err := handle.App().Sessions.Create(ctx, args.Goal)
	if err != nil {
		m.abortSpawn(ctx, handle, worktreePath)
		return m.failCreate(ctx, st, err)
	}

	st, err = m.store.SetSession(ctx, st.ID, sess.ID)
	if err != nil {
		m.abortSpawn(ctx, handle, worktreePath)
		return m.failCreate(ctx, st, err)
	}

	m.watch(handle, st.ID)

	st, err = m.setStatus(ctx, st.ID, StatusRunning, "", "", 0)
	if err != nil {
		return Strand{}, err
	}

	m.dispatch(handle, st.SessionID, args.Goal)

	return st, nil
}

// abortSpawn tears a just-spawned workspace and worktree back down after a
// failure partway through Create. Errors are logged, not returned: the
// caller already has the error that triggered the rollback and that is
// what gets surfaced.
func (m *Manager) abortSpawn(ctx context.Context, handle Handle, worktreePath string) {
	if err := m.spawner.Release(ctx, handle.ID()); err != nil {
		slog.Error("strand: release spawner handle during create rollback failed", "error", err)
	}
	if err := git.WorktreeRemove(ctx, m.repoRoot, worktreePath, true); err != nil {
		slog.Error("strand: worktree removal during create rollback failed", "error", err)
	}
}

// failCreate records cause as the strand's terminal failure and returns it
// to Create's caller.
func (m *Manager) failCreate(ctx context.Context, st Strand, cause error) (Strand, error) {
	if _, err := m.setStatus(ctx, st.ID, StatusFailed, cause.Error(), "", 0); err != nil {
		slog.Error("strand: recording create failure failed", "strand", st.ID, "error", err)
	}
	return Strand{}, cause
}

// watch registers handle as the running workspace for strandID and starts
// its RunComplete watcher goroutine. The subscription itself is
// established synchronously, before watch returns: callers (Create, Send)
// dispatch the agent run right after watch returns, and a RunComplete
// published before the subscription is registered would otherwise be
// silently missed by the broker's fan-out.
func (m *Manager) watch(handle Handle, strandID string) {
	watchCtx, cancel := context.WithCancel(m.ctx)
	sub := handle.App().RunCompletions().Subscribe(watchCtx)

	m.mu.Lock()
	m.running[strandID] = &runtimeState{handle: handle, watchCancel: cancel}
	m.mu.Unlock()

	go func() {
		for {
			select {
			case <-watchCtx.Done():
				return
			case ev, ok := <-sub:
				if !ok {
					return
				}
				m.onRunComplete(strandID, ev.Payload)
			}
		}
	}()
}

// dispatch runs prompt on session sessionID inside handle's workspace in
// the background. This is the fire-and-forget path shared by Create and
// Send; the terminal outcome reaches the manager through the RunComplete
// watcher started by watch, not through this call's return value.
func (m *Manager) dispatch(handle Handle, sessionID, prompt string) {
	go func() {
		if _, err := handle.App().AgentCoordinator.Run(m.ctx, sessionID, prompt); err != nil {
			slog.Error("strand: agent run returned an error", "session_id", sessionID, "error", err)
		}
	}()
}

// onRunComplete is the RunComplete handler: it reacts to the authoritative
// end-of-run signal for a strand's session by recording the outcome and,
// on success with an auto merge policy, kicking off the merge flow.
func (m *Manager) onRunComplete(strandID string, rc notify.RunComplete) {
	st, err := m.store.Get(m.ctx, strandID)
	if err != nil {
		return
	}
	// Only react to the session this strand currently owns, and only
	// while a run is actually in flight: Remove or a completed merge can
	// race a straggling RunComplete from a run that no longer matters.
	if rc.SessionID != st.SessionID || st.Status != StatusRunning {
		return
	}

	if rc.Error != "" && !rc.Cancelled {
		if _, err := m.setStatus(m.ctx, strandID, StatusFailed, rc.Error, "", 0); err != nil {
			slog.Error("strand: recording run failure failed", "strand", strandID, "error", err)
		}
		return
	}
	if rc.Cancelled {
		if _, err := m.setStatus(m.ctx, strandID, StatusInterrupted, "", "", 0); err != nil {
			slog.Error("strand: recording run cancellation failed", "strand", strandID, "error", err)
		}
		return
	}

	// Auto-merge strands go straight from running into the merge flow
	// without resting at "completed" in between: setting a terminal
	// "completed" status first would give [Manager.Wait] a window where
	// it observes a non-active status and returns before the merge that
	// is about to start has even begun. Manual strands have no such
	// follow-up, so they rest at "completed" for real.
	if st.MergePolicy == MergeAuto {
		if err := m.mergeAttempt(m.ctx, strandID, true, rc.Text); err != nil {
			slog.Error("strand: auto-merge failed", "strand", strandID, "error", err)
		}
		return
	}

	if _, err := m.setStatus(m.ctx, strandID, StatusCompleted, "", rc.Text, time.Now().Unix()); err != nil {
		slog.Error("strand: recording run completion failed", "strand", strandID, "error", err)
	}
}

// Send re-dispatches message into a strand's session, resuming it if its
// workspace is not currently spawned (e.g. after an interrupted run — the
// worktree is still on disk, so the workspace is simply respawned).
func (m *Manager) Send(ctx context.Context, idOrName, message string) error {
	st, err := m.resolve(ctx, idOrName)
	if err != nil {
		return err
	}

	m.mu.Lock()
	rt, ok := m.running[st.ID]
	m.mu.Unlock()

	if !ok {
		handle, err := m.spawner.Spawn(ctx, st.WorktreePath)
		if err != nil {
			return fmt.Errorf("strand: respawn workspace: %w", err)
		}
		m.watch(handle, st.ID)
		m.mu.Lock()
		rt = m.running[st.ID]
		m.mu.Unlock()
	}

	st, err = m.setStatus(ctx, st.ID, StatusRunning, "", "", 0)
	if err != nil {
		return err
	}

	m.dispatch(rt.handle, st.SessionID, message)
	return nil
}

// Merge runs (or retries) the merge flow for a strand. Manual-policy
// strands are merged this way once their run completes; auto-policy
// strands use this to retry after a conflict has been resolved.
func (m *Manager) Merge(ctx context.Context, idOrName string) error {
	st, err := m.resolve(ctx, idOrName)
	if err != nil {
		return err
	}
	// Empty resultSummary tells mergeAttempt to keep whatever is already
	// on the row (e.g. from the run that led to the current conflict or
	// merge_blocked state) instead of clobbering it.
	return m.mergeAttempt(ctx, st.ID, true, "")
}

// mergeAttempt folds a strand's branch back into its base branch.
// allowRetry permits one recursive retry from the top of the merge
// (re-merging base into the strand branch) when the base has moved on
// concurrently; the retry itself passes allowRetry=false so a persistently
// moving base ends in merge_blocked rather than looping. resultSummary, if
// non-empty, is the agent's final text to record alongside every status
// transition this attempt makes; pass "" to preserve whatever is already
// stored (see Merge).
func (m *Manager) mergeAttempt(ctx context.Context, strandID string, allowRetry bool, resultSummary string) error {
	st, err := m.store.Get(ctx, strandID)
	if err != nil {
		return err
	}
	if resultSummary == "" {
		resultSummary = st.ResultSummary
	}

	st, err = m.setStatus(ctx, strandID, StatusMerging, "", resultSummary, 0)
	if err != nil {
		return err
	}

	// A prior attempt may have left the worktree mid-merge. If conflicts
	// are still unresolved, report that state rather than re-attempting;
	// the caller resolves them (via Send) and calls Merge again.
	if conflicts, err := git.ConflictedFiles(ctx, st.WorktreePath); err != nil {
		return m.blockMerge(ctx, strandID, resultSummary, err.Error())
	} else if len(conflicts) > 0 {
		return m.setConflict(ctx, strandID, resultSummary, conflicts)
	}

	commitMsg := fmt.Sprintf("strand(%s): %s", st.Name, firstLine(st.Goal))
	if _, err := git.CommitAll(ctx, st.WorktreePath, commitMsg); err != nil {
		return m.blockMerge(ctx, strandID, resultSummary, err.Error())
	}

	result, err := git.MergeIntoWorktree(ctx, st.WorktreePath, st.BaseBranch)
	if err != nil {
		return m.blockMerge(ctx, strandID, resultSummary, err.Error())
	}
	if !result.Merged {
		return m.setConflict(ctx, strandID, resultSummary, result.Conflicts)
	}

	ffErr := git.FastForward(ctx, m.repoRoot, st.Branch, st.BaseBranch)
	switch {
	case ffErr == nil:
		return m.finishMerge(ctx, strandID, resultSummary)

	case errors.Is(ffErr, git.ErrBranchCheckedOut):
		dirty, err := git.IsDirty(ctx, m.repoRoot)
		if err != nil {
			return m.blockMerge(ctx, strandID, resultSummary, err.Error())
		}
		if dirty {
			return m.blockMerge(ctx, strandID, resultSummary, "base branch is checked out and the main worktree is dirty: "+ffErr.Error())
		}
		if err := git.MergeFFOnly(ctx, m.repoRoot, st.Branch); err != nil {
			return m.blockMerge(ctx, strandID, resultSummary, err.Error())
		}
		return m.finishMerge(ctx, strandID, resultSummary)

	case errors.Is(ffErr, git.ErrNonFastForward):
		if allowRetry {
			return m.mergeAttempt(ctx, strandID, false, resultSummary)
		}
		return m.blockMerge(ctx, strandID, resultSummary, "base branch moved concurrently: "+ffErr.Error())

	default:
		return m.blockMerge(ctx, strandID, resultSummary, ffErr.Error())
	}
}

func (m *Manager) finishMerge(ctx context.Context, strandID, resultSummary string) error {
	st, err := m.setStatus(ctx, strandID, StatusMerged, "", resultSummary, time.Now().Unix())
	if err != nil {
		return err
	}
	m.publish(EventMerged, st)
	return nil
}

func (m *Manager) setConflict(ctx context.Context, strandID, resultSummary string, conflicts []string) error {
	_, err := m.setStatus(ctx, strandID, StatusConflict, "merge conflicts: "+strings.Join(conflicts, ", "), resultSummary, 0)
	return err
}

func (m *Manager) blockMerge(ctx context.Context, strandID, resultSummary, reason string) error {
	_, err := m.setStatus(ctx, strandID, StatusMergeBlocked, reason, resultSummary, 0)
	return err
}

// Remove tears a strand down: it cancels and releases its workspace (if
// force), removes its git worktree (and branch, if deleteBranch), and
// deletes its store row. It refuses to run/merging strands unless force,
// and refuses unmerged strands with a dirty worktree unless force.
func (m *Manager) Remove(ctx context.Context, idOrName string, force, deleteBranch bool) error {
	st, err := m.resolve(ctx, idOrName)
	if err != nil {
		return err
	}

	if (st.Status == StatusRunning || st.Status == StatusMerging) && !force {
		return fmt.Errorf("strand: %q is active (status=%s); use force to remove", st.Name, st.Status)
	}
	if st.Status != StatusMerged {
		if dirty, err := git.IsDirty(ctx, st.WorktreePath); err == nil && dirty && !force {
			return fmt.Errorf("strand: %q has unmerged, uncommitted changes; use force to remove", st.Name)
		}
	}

	m.mu.Lock()
	rt, ok := m.running[st.ID]
	delete(m.running, st.ID)
	m.mu.Unlock()

	if ok {
		rt.watchCancel()
		if force {
			if a := rt.handle.App(); a != nil && a.AgentCoordinator != nil {
				a.AgentCoordinator.CancelAll()
			}
		}
		if err := m.spawner.Release(ctx, rt.handle.ID()); err != nil {
			slog.Error("strand: release spawner handle on remove failed", "strand", st.ID, "error", err)
		}
	}

	if err := git.WorktreeRemove(ctx, m.repoRoot, st.WorktreePath, force); err != nil {
		return fmt.Errorf("strand: remove worktree: %w", err)
	}
	if deleteBranch {
		if err := git.DeleteBranch(ctx, m.repoRoot, st.Branch, force); err != nil {
			return fmt.Errorf("strand: delete branch: %w", err)
		}
	}
	if err := m.store.Delete(ctx, st.ID); err != nil {
		return fmt.Errorf("strand: delete record: %w", err)
	}
	m.publish(EventRemoved, st)
	return nil
}

// Handle returns the spawned workspace handle for strandID, or nil if the
// strand's workspace is not currently spawned (e.g. it finished, or is
// between runs after an interrupt).
func (m *Manager) Handle(strandID string) Handle {
	m.mu.Lock()
	defer m.mu.Unlock()
	rt, ok := m.running[strandID]
	if !ok {
		return nil
	}
	return rt.handle
}

// WorkspaceID returns the backend/runtime identifier of strandID's
// currently-spawned workspace (Handle.ID()), or "" if not spawned. In
// client/server mode this is the backend workspace ID the strand's
// workspace was created with (see internal/backend/strand_spawner.go's
// strandHandle), letting a client attach to it directly over HTTP; in
// local (single-process) mode it is an opaque spawner-internal ID with no
// meaning outside the process.
func (m *Manager) WorkspaceID(strandID string) string {
	if h := m.Handle(strandID); h != nil {
		return h.ID()
	}
	return ""
}

// Recover reconciles store state against reality after a process restart:
// strands left pending/running/merging (their goroutines are gone with
// the old process) become interrupted, and strands whose worktree has
// vanished from disk become failed.
func (m *Manager) Recover(ctx context.Context) error {
	strands, err := m.store.List(ctx)
	if err != nil {
		return err
	}
	for _, st := range strands {
		if _, statErr := os.Stat(st.WorktreePath); os.IsNotExist(statErr) {
			if st.Status == StatusFailed || st.Status == StatusMerged {
				continue
			}
			if _, err := m.setStatus(ctx, st.ID, StatusFailed, "worktree missing on recovery", "", 0); err != nil {
				return err
			}
			continue
		}
		if st.Status == StatusPending || st.Status == StatusRunning || st.Status == StatusMerging {
			if _, err := m.setStatus(ctx, st.ID, StatusInterrupted, "", "", 0); err != nil {
				return err
			}
		}
	}
	return nil
}

// Wait blocks until none of the strands named by ids (all strands, when
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
	strands, err := m.waitTargets(ctx, ids)
	if err != nil {
		return false, err
	}
	for _, st := range strands {
		if st.Status == StatusPending || st.Status == StatusRunning || st.Status == StatusMerging {
			return true, nil
		}
	}
	return false, nil
}

func (m *Manager) waitTargets(ctx context.Context, ids []string) ([]Strand, error) {
	if len(ids) == 0 {
		return m.store.List(ctx)
	}
	strands := make([]Strand, 0, len(ids))
	for _, id := range ids {
		st, err := m.resolve(ctx, id)
		if err != nil {
			return nil, err
		}
		strands = append(strands, st)
	}
	return strands, nil
}

// publish emits a lifecycle event and wakes any Wait callers blocked on a
// status change.
func (m *Manager) publish(t EventType, st Strand) {
	m.broker.Publish(pubsub.UpdatedEvent, Event{Type: t, Strand: st})
	m.notifyChange()
}

// setStatus is the shared SetStatus + publish helper used by every status
// transition in this file.
func (m *Manager) setStatus(ctx context.Context, strandID string, status Status, errText, resultSummary string, completedAt int64) (Strand, error) {
	st, err := m.store.SetStatus(ctx, strandID, SetStatusParams{
		Status:        status,
		Error:         errText,
		ResultSummary: resultSummary,
		CompletedAt:   completedAt,
	})
	if err != nil {
		return Strand{}, err
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
		return "", fmt.Errorf("strand: invalid name %q: must be a lowercase alphanumeric slug (hyphens allowed, not leading or trailing)", name)
	}
	return name, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
