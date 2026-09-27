package appws

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"github.com/google/uuid"
	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/git"
	sessionstore "github.com/rave-soft/sennit/internal/session/store"
	"github.com/rave-soft/sennit/internal/workspace"
)

var (
	ErrWorktreeUnavailable = errors.New("worktree transfer is not available")
	ErrWorktreeBusy        = errors.New("session is busy and cannot change worktrees")
	transferLocks          sync.Map
	worktreeNameRE         = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)
	bootstrapWorktreeApp   = app.Bootstrap
)

func sessionTransferLock(sessionID string) *sync.Mutex {
	lock, _ := transferLocks.LoadOrStore(sessionID, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func validateWorktreeTarget(ctx context.Context, repo, base, name string) (string, error) {
	if !worktreeNameRE.MatchString(name) || filepath.IsAbs(name) || filepath.Base(name) != name {
		return "", fmt.Errorf("%w: invalid worktree name %q", ErrWorktreeUnavailable, name)
	}
	path := filepath.Join(base, name)
	rel, err := filepath.Rel(base, path)
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator) {
		return "", fmt.Errorf("%w: worktree path escapes its root", ErrWorktreeUnavailable)
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		branch, branchErr := git.CurrentBranch(ctx, repo)
		if branchErr != nil {
			return "", branchErr
		}
		if err := os.MkdirAll(base, 0o755); err != nil {
			return "", err
		}
		if err := git.WorktreeAdd(ctx, repo, path, "worktree/"+name, branch); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	} else {
		top, topErr := git.TopLevel(ctx, path)
		if topErr != nil || fsext.Canonical(top) != fsext.Canonical(path) {
			return "", fmt.Errorf("%w: existing path is not a worktree", ErrWorktreeUnavailable)
		}
		repoCommon, repoErr := git.CommonDir(ctx, repo)
		pathCommon, pathErr := git.CommonDir(ctx, path)
		if repoErr != nil || pathErr != nil || fsext.Canonical(repoCommon) != fsext.Canonical(pathCommon) {
			return "", fmt.Errorf("%w: existing worktree belongs to another repository", ErrWorktreeUnavailable)
		}
	}
	return fsext.Canonical(path), nil
}

// EnterWorktree prepares a new App around the same persisted top-level
// session, then transfers its owner epoch under the source admission gate.
func (w *AppWorkspace) EnterWorktree(ctx context.Context, name string) (workspace.Workspace, func(), error) {
	if w.worktreeRoot != nil {
		return nil, nil, ErrWorktreeUnavailable
	}
	sessionID := w.app.CurrentSessionID()
	if sessionID == "" {
		return nil, nil, fmt.Errorf("%w: no current session", ErrWorktreeUnavailable)
	}
	lock := sessionTransferLock(sessionID)
	lock.Lock()
	defer lock.Unlock()
	sess, err := w.app.Sessions().Get(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	if sess.ParentSessionID != "" {
		return nil, nil, fmt.Errorf("%w: only this project's top-level session can be transferred", ErrWorktreeUnavailable)
	}
	var ownership sessionstore.Ownership
	if err := w.app.WithSessionTransferGate(ctx, sessionID, func() error {
		var claimErr error
		ownership, claimErr = w.app.ClaimSessionOwnership(ctx, sessionID)
		return claimErr
	}); err != nil {
		if errors.Is(err, app.ErrSessionBusy) {
			return nil, nil, ErrWorktreeBusy
		}
		return nil, nil, err
	}
	ownerID, ownerEpoch := w.app.OwnerIdentity()
	if ownership.OwnerID != ownerID || ownership.Epoch != ownerEpoch || ownership.Phase != "stable" {
		return nil, nil, app.ErrSessionOwnershipLost
	}
	path, err := validateWorktreeTarget(ctx, w.store.WorkingDir(), filepath.Join(w.store.Config().Options.DataDirectory, "worktrees"), name)
	if err != nil {
		return nil, nil, err
	}
	targetOwner := uuid.NewString()
	if err := w.app.OwnershipStore().Prepare(ctx, sessionID, ownerID, ownerEpoch, sessionstore.Ownership{OwnerID: targetOwner, OwnerRoot: path, WorktreeName: name, WorktreePath: path}); err != nil {
		return nil, nil, err
	}
	// context.WithoutCancel: the new App's own globalCtx (app.New stores
	// whatever ctx Bootstrap is given verbatim) must outlive this RPC —
	// its AgentDispatcher, background jobs and every other goroutine
	// bound to it need to keep running long after EnterWorktree returns
	// and the incoming gRPC handler's own ctx is done. Passing that ctx
	// through unwrapped left the worktree App's entire lifetime tied to
	// one request: AgentDispatcher.Send's ownership check
	// (app.checkSessionOwnership) reads app.globalCtx, so the very next
	// AgentRun after EnterWorktree returned decoded as context.Canceled
	// end to end (found via
	// TestWorktreeSurvivesDisconnectAndResumesOnReconnect, CLIENT-SERVER.md
	// PR 2.4b).
	boot, err := bootstrapWorktreeApp(context.WithoutCancel(ctx), path, app.BootstrapOptions{ProjectPath: w.app.ProjectPath(), ExistingSessionID: sessionID, ConfineWrites: true, InheritedAgents: w.store.Config().Agents})
	if err != nil {
		_ = w.app.OwnershipStore().Rollback(context.WithoutCancel(ctx), sessionID, ownerID, ownerEpoch)
		return nil, nil, err
	}
	var committed sessionstore.Ownership
	err = w.app.WithSessionTransferGate(ctx, sessionID, func() error {
		var commitErr error
		committed, commitErr = w.app.OwnershipStore().Commit(ctx, sessionID, ownerID, ownerEpoch)
		return commitErr
	})
	if err != nil {
		boot.App.Shutdown()
		_ = w.app.OwnershipStore().Rollback(context.WithoutCancel(ctx), sessionID, ownerID, ownerEpoch)
		if errors.Is(err, app.ErrSessionBusy) {
			return nil, nil, ErrWorktreeBusy
		}
		return nil, nil, err
	}
	boot.App.AdoptSessionOwnership(sessionID, targetOwner, committed.Epoch)
	target := NewAppWorkspace(boot.App, boot.Config)
	target.worktreeRoot = w
	target.worktreeName = committed.WorktreeName
	target.worktreePath = committed.WorktreePath
	w.registerWorktreeChild(sessionID, target)
	release := func() {
		owned, err := boot.App.OwnershipStore().IsOwner(context.WithoutCancel(ctx), sessionID, targetOwner, committed.Epoch)
		if err == nil && !owned {
			boot.App.Shutdown()
			w.unregisterWorktreeChild(sessionID)
		}
	}
	return target, release, nil
}

// registerWorktreeChild records child as the live worktree workspace
// owning sessionID, so a later ResumeWorktree call against w can find it
// again after the client that entered it disconnects (CLIENT-SERVER.md,
// PR 2.4b).
func (w *AppWorkspace) registerWorktreeChild(sessionID string, child *AppWorkspace) {
	w.worktreeChildrenMu.Lock()
	defer w.worktreeChildrenMu.Unlock()
	if w.worktreeChildren == nil {
		w.worktreeChildren = make(map[string]*AppWorkspace)
	}
	w.worktreeChildren[sessionID] = child
}

// unregisterWorktreeChild drops sessionID's entry once its worktree App
// has actually shut down. A no-op if it was never registered or already
// removed -- both release paths that call this (EnterWorktree's own
// release, ExitWorktree's) can race each other harmlessly since a map
// delete of an absent key is a no-op.
func (w *AppWorkspace) unregisterWorktreeChild(sessionID string) {
	w.worktreeChildrenMu.Lock()
	defer w.worktreeChildrenMu.Unlock()
	delete(w.worktreeChildren, sessionID)
}

// lookupWorktreeChild returns the registered worktree workspace for
// sessionID, if any.
func (w *AppWorkspace) lookupWorktreeChild(sessionID string) (*AppWorkspace, bool) {
	w.worktreeChildrenMu.Lock()
	defer w.worktreeChildrenMu.Unlock()
	child, ok := w.worktreeChildren[sessionID]
	return child, ok
}

// ResumeWorktree returns the live worktree workspace that still owns
// sessionID, for a client reconnecting after the client that originally
// called EnterWorktree disconnected without calling ExitWorktree -- the
// worktree App keeps running and keeps owning the session (a turn may
// still be in flight there), but until this call nothing could hand a
// new client a fresh route to it (CLIENT-SERVER.md, PR 2.4b: "the
// worktree gap"). Only a root workspace's own registry is consulted (see
// worktreeChildren's doc comment), so calling this on a worktree or
// thread workspace always returns ErrNoWorktreeForSession, the same
// answer as "no worktree for this session at all".
//
// The release func follows EnterWorktree's own rule: it only shuts the
// child's App down once that App no longer owns sessionID (checked via
// the ownership store, exactly like EnterWorktree's release), so a
// caller resuming a still-active worktree never tears down the App a
// different, still-attached client depends on. app.App.Shutdown is
// idempotent, so a second caller's release running after the first
// already shut it down (because ownership moved on in between) is safe.
func (w *AppWorkspace) ResumeWorktree(ctx context.Context, sessionID string) (workspace.Workspace, func(), error) {
	child, ok := w.lookupWorktreeChild(sessionID)
	if !ok {
		return nil, nil, workspace.ErrNoWorktreeForSession
	}
	ownerID, ownerEpoch := child.app.OwnerIdentity()
	release := func() {
		owned, err := child.app.OwnershipStore().IsOwner(context.WithoutCancel(ctx), sessionID, ownerID, ownerEpoch)
		if err == nil && !owned {
			child.app.Shutdown()
			w.unregisterWorktreeChild(sessionID)
		}
	}
	return child, release, nil
}

// ExitWorktree reverses the transfer while retaining durable worktree
// identity for later reuse. The target App is shut down only after main owns
// the new epoch.
func (w *AppWorkspace) ExitWorktree(ctx context.Context) (workspace.Workspace, func(), error) {
	if w.worktreeRoot == nil {
		return nil, nil, ErrWorktreeUnavailable
	}
	sessionID := w.app.CurrentSessionID()
	if sessionID == "" {
		return nil, nil, ErrWorktreeUnavailable
	}
	lock := sessionTransferLock(sessionID)
	lock.Lock()
	defer lock.Unlock()
	ownerID, ownerEpoch := w.app.OwnerIdentity()
	o, err := w.app.OwnershipStore().Get(ctx, sessionID)
	if err != nil || o.OwnerID != ownerID || o.Epoch != ownerEpoch || o.Phase != "stable" {
		return nil, nil, app.ErrSessionOwnershipLost
	}
	if err := w.app.WithSessionTransferGate(ctx, sessionID, func() error { return nil }); err != nil {
		if errors.Is(err, app.ErrSessionBusy) {
			return nil, nil, ErrWorktreeBusy
		}
		return nil, nil, err
	}
	rootOwner, _ := w.worktreeRoot.app.OwnerIdentity()
	rootPath := fsext.Canonical(w.worktreeRoot.store.WorkingDir())
	if err := w.app.OwnershipStore().Prepare(ctx, sessionID, ownerID, ownerEpoch, sessionstore.Ownership{OwnerID: rootOwner, OwnerRoot: rootPath, WorktreeName: o.WorktreeName, WorktreePath: o.WorktreePath}); err != nil {
		return nil, nil, err
	}
	var committed sessionstore.Ownership
	err = w.app.WithSessionTransferGate(ctx, sessionID, func() error {
		var commitErr error
		committed, commitErr = w.app.OwnershipStore().Commit(ctx, sessionID, ownerID, ownerEpoch)
		return commitErr
	})
	if err != nil {
		_ = w.app.OwnershipStore().Rollback(context.WithoutCancel(ctx), sessionID, ownerID, ownerEpoch)
		if errors.Is(err, app.ErrSessionBusy) {
			return nil, nil, ErrWorktreeBusy
		}
		return nil, nil, err
	}
	w.worktreeRoot.app.AdoptSessionOwnership(sessionID, rootOwner, committed.Epoch)
	w.worktreeRoot.app.ReportCurrentSession(sessionID)
	root := w.worktreeRoot
	var once sync.Once
	return root, func() {
		once.Do(func() {
			w.app.Shutdown()
			root.unregisterWorktreeChild(sessionID)
		})
	}, nil
}

// WorktreeChildren returns every worktree workspace this instance's own
// registry currently tracks -- every EnterWorktree spawn whose App is
// still running, whether or not any client is presently attached to it.
// It exists solely for the daemon's idle check (internal/daemon/idle.go),
// via the worktreeAggregator interface it asserts for: a turn running in
// an orphaned worktree App (one whose original client disconnected
// without ExitWorktree) is otherwise invisible to AgentActivity/
// BackgroundJobCounts/PendingPrompts on the root workspace alone
// (CLIENT-SERVER.md, PR 2.4b). Not part of workspace.Workspace -- it is
// implementation detail, like WorktreeName above, not wire contract.
func (w *AppWorkspace) WorktreeChildren() []workspace.Workspace {
	w.worktreeChildrenMu.Lock()
	defer w.worktreeChildrenMu.Unlock()
	if len(w.worktreeChildren) == 0 {
		return nil
	}
	out := make([]workspace.Workspace, 0, len(w.worktreeChildren))
	for _, child := range w.worktreeChildren {
		out = append(out, child)
	}
	return out
}

func (w *AppWorkspace) WorktreeName() string { return w.worktreeName }

func (w *AppWorkspace) WorktreeState() workspace.WorktreeState {
	return workspace.WorktreeState{Name: w.worktreeName, Path: w.worktreePath, Phase: "stable", Active: w.worktreeRoot != nil}
}
