package workspace

import (
	"context"
	"fmt"
	"log/slog"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/braid/internal/app/threadspawn"
	"github.com/rave-soft/braid/internal/log"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/thread"
)

// threadEventPubsubType maps a thread lifecycle event's semantic type
// (created/status_changed/merged/removed, see proto.ThreadEventType) onto
// the coarser pubsub.EventType the TUI's thread state machines
// (threads_cache.go, threads_dock.go, thread_indicator.go) key their
// upsert/remove logic off. AppWorkspace.translateEvent funnels through
// this so its mapping stays centralized.
func threadEventPubsubType(t proto.ThreadEventType) pubsub.EventType {
	switch t {
	case proto.ThreadEventCreated:
		return pubsub.CreatedEvent
	case proto.ThreadEventRemoved:
		return pubsub.DeletedEvent
	default: // status_changed, merged
		return pubsub.UpdatedEvent
	}
}

// -- AppWorkspace: Threads --

// threadManager returns this workspace's *thread.Manager and whether one
// is attached. app.ThreadManager() returns `any` because internal/app
// (core, imported by internal/agent) cannot import internal/thread
// directly — see internal/app/app.go's SetThreadManager/ThreadManager
// doc comments for the layering reason.
func (w *AppWorkspace) threadManager() (*thread.Manager, bool) {
	mgr, ok := w.app.ThreadManager().(*thread.Manager)
	return mgr, ok && mgr != nil
}

func (w *AppWorkspace) SupportsThreads() bool {
	_, ok := w.threadManager()
	return ok
}

func (w *AppWorkspace) ListThreads(ctx context.Context) ([]proto.Thread, error) {
	mgr, ok := w.threadManager()
	if !ok {
		return nil, ErrThreadsNotSupported
	}
	sts, err := mgr.List(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]proto.Thread, len(sts))
	for i, st := range sts {
		result[i] = threadspawn.ThreadToProto(mgr, st)
	}
	return result, nil
}

func (w *AppWorkspace) GetThread(ctx context.Context, id string) (proto.Thread, error) {
	mgr, ok := w.threadManager()
	if !ok {
		return proto.Thread{}, ErrThreadsNotSupported
	}
	st, err := mgr.Get(ctx, id)
	if err != nil {
		return proto.Thread{}, err
	}
	return threadspawn.ThreadToProto(mgr, st), nil
}

func (w *AppWorkspace) CreateThread(ctx context.Context, req proto.CreateThreadRequest) (proto.Thread, error) {
	mgr, ok := w.threadManager()
	if !ok {
		return proto.Thread{}, ErrThreadsNotSupported
	}
	st, err := mgr.Create(ctx, thread.CreateArgs{
		Name:            req.Name,
		Goal:            req.Goal,
		BaseBranch:      req.BaseBranch,
		MergePolicy:     thread.MergePolicy(req.MergePolicy),
		ParentSessionID: req.ParentSessionID,
	})
	if err != nil {
		return proto.Thread{}, err
	}
	return threadspawn.ThreadToProto(mgr, st), nil
}

func (w *AppWorkspace) SendThread(ctx context.Context, id, message string) error {
	mgr, ok := w.threadManager()
	if !ok {
		return ErrThreadsNotSupported
	}
	return mgr.Send(ctx, id, message)
}

func (w *AppWorkspace) ActivateThread(ctx context.Context, id string) (proto.Thread, error) {
	mgr, ok := w.threadManager()
	if !ok {
		return proto.Thread{}, ErrThreadsNotSupported
	}
	st, err := mgr.Activate(ctx, id)
	if err != nil {
		return proto.Thread{}, err
	}
	return threadspawn.ThreadToProto(mgr, st), nil
}

func (w *AppWorkspace) MergeThread(ctx context.Context, id string) (proto.Thread, error) {
	mgr, ok := w.threadManager()
	if !ok {
		return proto.Thread{}, ErrThreadsNotSupported
	}
	// Merge returns the outcome directly: a thread that merged cleanly is
	// discarded, so re-fetching it here would find nothing.
	st, err := mgr.Merge(ctx, id)
	if err != nil {
		return proto.Thread{}, err
	}
	return threadspawn.ThreadToProto(mgr, st), nil
}

func (w *AppWorkspace) CancelThread(ctx context.Context, id, reason string) error {
	mgr, ok := w.threadManager()
	if !ok {
		return ErrThreadsNotSupported
	}
	return mgr.Cancel(ctx, id, reason)
}

func (w *AppWorkspace) RemoveThread(ctx context.Context, id string, opts proto.RemoveThreadOptions) error {
	mgr, ok := w.threadManager()
	if !ok {
		return ErrThreadsNotSupported
	}
	return mgr.Remove(ctx, id, opts.Force, opts.DeleteBranch)
}

// AttachThread connects to id's own spawned workspace and returns a
// Workspace bound to it, plus a detach func to release that
// connection (NOT the thread itself — the thread keeps running
// regardless of whether anything is attached to view it). Callers
// must call detach exactly once when done.
//
// If the thread's workspace is not currently spawned (it completed
// and was released), AttachThread returns a read-only workspace
// bound to the shared session/message stores so the caller can
// still inspect persisted session data in the database.
func (w *AppWorkspace) AttachThread(ctx context.Context, id string) (Workspace, func(), error) {
	mgr, ok := w.threadManager()
	if !ok {
		return nil, nil, ErrThreadsNotSupported
	}
	h := mgr.Handle(id)
	if h != nil {
		// Live thread: bind to the running workspace.
		// detach is a no-op for local mode: the thread's App is owned by the
		// Manager, not by whoever is viewing it, so there is nothing to
		// release here — teardown happens via Manager.Remove or process
		// shutdown, not via this detach.
		// The handle's Workspace is the domain-facing thread.Workspace
		// seam; the spawners present it as a threadspawn.AppWorkspaceAdapter
		// (see its doc comment), so assert that back here at the boundary
		// to reach the concrete *app.App it wraps — this façade layer is
		// allowed to know it (NewAppWorkspace needs it).
		aw, ok := h.Workspace().(*threadspawn.AppWorkspaceAdapter)
		if !ok || aw.App == nil {
			return nil, nil, fmt.Errorf("thread: workspace handle does not wrap an *app.App")
		}
		ws := NewAppWorkspace(aw.App, aw.App.Store())
		return ws, func() {}, nil
	}
	// Thread is not currently spawned (completed, interrupted, failed).
	// Verify the thread actually exists before returning a workspace —
	// an unknown ID should still fail.
	st, err := mgr.Get(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	// Reactivate it so attaching lands in a writable session: the
	// worktree and branch are still on disk, so the workspace can simply
	// be respawned, and a thread whose run is over is exactly the one a
	// user wants to open and keep working in by hand.
	if _, err := mgr.Activate(ctx, id); err == nil {
		if h := mgr.Handle(id); h != nil {
			if a, ok := h.Workspace().(*threadspawn.AppWorkspaceAdapter); ok && a.App != nil {
				return NewAppWorkspace(a.App, a.App.Store()), func() {}, nil
			}
		}
	} else {
		slog.Debug("Thread reactivation unavailable during attach, falling back to read-only", "thread", id, "error", err)
	}
	// Reactivation is not always possible — threads in the merge flow
	// are deliberately refused, and the worktree may be gone. Fall back
	// to a read-only workspace bound to the main app with the thread's
	// worktree as WorkingDir, so the caller can still inspect persisted
	// session data.
	aw := newReadOnlyWorkspace(w, st.WorktreePath, st.SessionID)
	return aw, func() {}, nil
}

// SubscribeWith runs a second, independently stoppable event subscription
// against this workspace's App, for callers (e.g. the TUI attaching to a
// thread's own workspace via Workspace.AttachThread) that need a plain
// send callback and an explicit stop rather than the *tea.Program-bound
// Subscribe. Unlike Subscribe (which rides app.globalCtx/app.Shutdown),
// this owns its own context so it can be torn down independently of the
// underlying App's lifetime.
func (w *AppWorkspace) SubscribeWith(send func(tea.Msg)) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer log.RecoverPanic("AppWorkspace.SubscribeWith", func() {})
		events := w.app.Events(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-events:
				if !ok {
					return
				}
				send(ev.Payload)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
