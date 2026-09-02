package appws

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/rave-soft/sennit/internal/app/threadspawn"
	"github.com/rave-soft/sennit/internal/log"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/thread"
	"github.com/rave-soft/sennit/internal/workspace"
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
// is attached.
func (w *AppWorkspace) threadManager() (*thread.Manager, bool) {
	mgr := w.app.ThreadManager()
	return mgr, mgr != nil
}

func (w *AppWorkspace) SupportsThreads() bool {
	_, ok := w.threadManager()
	return ok
}

func (w *AppWorkspace) ListThreads(ctx context.Context) ([]proto.Thread, error) {
	mgr, ok := w.threadManager()
	if !ok {
		return nil, workspace.ErrThreadsNotSupported
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
		return proto.Thread{}, workspace.ErrThreadsNotSupported
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
		return proto.Thread{}, workspace.ErrThreadsNotSupported
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

// SendThread is the person's own path into a thread's session (the TUI's
// thread view), so it goes through SendFromPerson: the message is theirs,
// and it reaches the turn the thread is already running rather than
// waiting behind it (see thread.SenderPerson).
//
// It drops the disposition: whoever typed the message is looking at that
// session's transcript and can see for themselves what became of it. Only
// the agent-facing thread_send tool, which has no such view, reports it —
// see tools.SendOutcome.
func (w *AppWorkspace) SendThread(ctx context.Context, id, message string) error {
	mgr, ok := w.threadManager()
	if !ok {
		return workspace.ErrThreadsNotSupported
	}
	_, err := mgr.SendFromPerson(ctx, id, message)
	return err
}

func (w *AppWorkspace) ActivateThread(ctx context.Context, id string) (proto.Thread, error) {
	mgr, ok := w.threadManager()
	if !ok {
		return proto.Thread{}, workspace.ErrThreadsNotSupported
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
		return proto.Thread{}, workspace.ErrThreadsNotSupported
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
		return workspace.ErrThreadsNotSupported
	}
	return mgr.Cancel(ctx, id, reason)
}

func (w *AppWorkspace) RemoveThread(ctx context.Context, id string, opts proto.RemoveThreadOptions) error {
	mgr, ok := w.threadManager()
	if !ok {
		return workspace.ErrThreadsNotSupported
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
func (w *AppWorkspace) AttachThread(ctx context.Context, id string) (workspace.Workspace, func(), error) {
	mgr, ok := w.threadManager()
	if !ok {
		return nil, nil, workspace.ErrThreadsNotSupported
	}
	h := mgr.Handle(id)
	if h != nil {
		// Live thread: bind to the running workspace.
		// detach is a no-op for local mode: the thread's App is owned by the
		// Manager, not by whoever is viewing it, so there is nothing to
		// release here — teardown happens via Manager.Remove or process
		// shutdown, not via this detach.
		ws, err := frontendWorkspace(h)
		if err != nil {
			return nil, nil, err
		}
		// Wrapped so a turn the person starts in the thread's own session
		// goes through the Manager rather than past it — see
		// attachedThreadWorkspace. Reading the row for its session id is
		// what makes that possible; a thread we cannot resolve is handed
		// back unwrapped rather than not at all, since viewing it still
		// works and refusing the attach over bookkeeping would be worse.
		st, err := mgr.Get(ctx, id)
		if err != nil {
			slog.Debug("Attached thread could not be resolved; its turns will not be tracked", "thread", id, "error", err)
			return ws, func() {}, nil
		}
		return &attachedThreadWorkspace{Workspace: ws, mgr: mgr, parent: w, threadID: st.ID, sessionID: st.SessionID}, func() {}, nil
	}
	// Thread is not currently spawned (completed, interrupted, failed).
	// Verify the thread actually exists before returning a workspace —
	// an unknown ID should still fail.
	st, err := mgr.Get(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	// Attaching never revives it on its own — that used to happen here
	// implicitly (via reactivate/mgr.Activate), which meant every caller,
	// including the dock's background activity refresh, silently spawned
	// a full App per idle thread it glanced at. Reviving a thread is now
	// something only a caller that means it does explicitly, via
	// ActivateThread, before calling AttachThread — see
	// ui/model/root.go's attachThreadCmd. This just returns a read-only
	// workspace bound to the main app with the thread's worktree as
	// WorkingDir, which still shows the persisted session data.
	return workspace.NewReadOnlyWorkspace(w, st.WorktreePath, st.SessionID, "thread is not running"), func() {}, nil
}

// frontendWorkspace obtains the frontend-facing view supplied by a handle's
// workspace. The domain Workspace remains independent of the frontend; only
// implementations that can be attached expose this optional capability.
func frontendWorkspace(h thread.Handle) (workspace.Workspace, error) {
	provider, ok := h.Workspace().(interface {
		FrontendWorkspace() workspace.Workspace
	})
	if !ok {
		return nil, fmt.Errorf("thread: workspace handle does not expose a frontend workspace")
	}
	ws := provider.FrontendWorkspace()
	if ws == nil {
		return nil, fmt.Errorf("thread: workspace handle exposed a nil frontend workspace")
	}
	return ws, nil
}

// SubscribeWith runs a second, independently stoppable event subscription
// against this workspace's App, for callers (e.g. the TUI attaching to a
// thread's own workspace via Workspace.AttachThread) that need a plain
// send callback and an explicit stop rather than a UI-bound
// Subscribe. Unlike Subscribe (which rides app.globalCtx/app.Shutdown),
// this owns its own context so it can be torn down independently of the
// underlying App's lifetime.
func (w *AppWorkspace) SubscribeWith(send func(any)) func() {
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
				// Through the same translation the main screen's own
				// subscription uses. Sent raw, an embedded thread's UI
				// received app-internal shapes its Update has no case
				// for — so a thread view showed none of its agent's
				// errors, re-auth prompts, or MCP/LSP state changes.
				if translated := w.translateEvent(ev.Payload); translated != nil {
					send(translated)
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
