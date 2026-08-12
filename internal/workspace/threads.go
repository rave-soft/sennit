package workspace

import (
	"context"

	"github.com/google/uuid"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/braid/internal/log"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/thread"
)

// threadEventPubsubType maps a thread lifecycle event's semantic type
// (created/status_changed/merged/removed, see proto.ThreadEventType) onto
// the coarser pubsub.EventType the TUI's thread state machines
// (threads_cache.go, threads_dock.go, thread_indicator.go) key their
// upsert/remove logic off. Both AppWorkspace.translateEvent (local mode)
// and ClientWorkspace.translateEvent (client/server mode, decoding SSE)
// funnel through this so the two modes agree on the mapping.
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
		result[i] = mgr.ToProto(st)
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
	return mgr.ToProto(st), nil
}

func (w *AppWorkspace) CreateThread(ctx context.Context, req proto.CreateThreadRequest) (proto.Thread, error) {
	mgr, ok := w.threadManager()
	if !ok {
		return proto.Thread{}, ErrThreadsNotSupported
	}
	st, err := mgr.Create(ctx, thread.CreateArgs{
		Name:        req.Name,
		Goal:        req.Goal,
		BaseBranch:  req.BaseBranch,
		MergePolicy: thread.MergePolicy(req.MergePolicy),
	})
	if err != nil {
		return proto.Thread{}, err
	}
	return mgr.ToProto(st), nil
}

func (w *AppWorkspace) SendThread(ctx context.Context, id, message string) error {
	mgr, ok := w.threadManager()
	if !ok {
		return ErrThreadsNotSupported
	}
	return mgr.Send(ctx, id, message)
}

func (w *AppWorkspace) MergeThread(ctx context.Context, id string) (proto.Thread, error) {
	mgr, ok := w.threadManager()
	if !ok {
		return proto.Thread{}, ErrThreadsNotSupported
	}
	if err := mgr.Merge(ctx, id); err != nil {
		return proto.Thread{}, err
	}
	// Re-fetch after merge to return the latest status, mirroring the
	// server handler.
	st, err := mgr.Get(ctx, id)
	if err != nil {
		return proto.Thread{}, err
	}
	return mgr.ToProto(st), nil
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
		a := h.App()
		aw := NewAppWorkspace(a, a.Store())
		return aw, func() {}, nil
	}
	// Thread is not currently spawned (completed, interrupted, failed).
	// Verify the thread actually exists before returning a workspace —
	// an unknown ID should still fail.
	st, err := mgr.Get(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	// Return a read-only workspace bound to the main app with the
	// thread's worktree as WorkingDir, so the caller can inspect
	// persisted session data but cannot mutate anything.
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

// -- ClientWorkspace: Threads --

// SupportsThreads does a live round trip to the server: unlike
// AgentIsReady/AgentIsBusy (which read locally-cached state that's kept
// fresh by the subscription stream), there is no cached "does this
// workspace support threads" bit anywhere in proto.Workspace, so the
// only accurate answer requires asking the server; a cheap-but-wrong
// boolean would be worse than a slightly expensive correct one.
//
// SupportsThreads has no ctx parameter (see the ThreadController
// interface), so this uses context.Background() internally, meaning the
// call cannot be cancelled by a caller.
func (w *ClientWorkspace) SupportsThreads() bool {
	_, err := w.client.ListThreads(context.Background(), w.workspaceID())
	return err == nil
}

func (w *ClientWorkspace) ListThreads(ctx context.Context) ([]proto.Thread, error) {
	return w.client.ListThreads(ctx, w.workspaceID())
}

func (w *ClientWorkspace) GetThread(ctx context.Context, id string) (proto.Thread, error) {
	st, err := w.client.GetThread(ctx, w.workspaceID(), id)
	if err != nil {
		return proto.Thread{}, err
	}
	return *st, nil
}

func (w *ClientWorkspace) CreateThread(ctx context.Context, req proto.CreateThreadRequest) (proto.Thread, error) {
	st, err := w.client.CreateThread(ctx, w.workspaceID(), req)
	if err != nil {
		return proto.Thread{}, err
	}
	return *st, nil
}

func (w *ClientWorkspace) SendThread(ctx context.Context, id, message string) error {
	return w.client.SendThread(ctx, w.workspaceID(), id, message)
}

func (w *ClientWorkspace) MergeThread(ctx context.Context, id string) (proto.Thread, error) {
	st, err := w.client.MergeThread(ctx, w.workspaceID(), id)
	if err != nil {
		return proto.Thread{}, err
	}
	return *st, nil
}

func (w *ClientWorkspace) RemoveThread(ctx context.Context, id string, opts proto.RemoveThreadOptions) error {
	return w.client.RemoveThread(ctx, w.workspaceID(), id, opts)
}

// AttachThread connects to a thread's own backend workspace using a
// separate client identity, never the parent ClientWorkspace's own
// client ID. This matters because the returned Workspace's detach func
// calls Shutdown, which retires the client (or deletes the workspace on
// an older server) — if it shared the parent's client ID, detaching an
// attached thread view would tear down the PARENT workspace's own claim
// too. The client shares the same underlying *http.Client/connection pool
// (see client.Client.WithClientID), so this costs nothing beyond one
// extra small struct.
//
// If the thread's workspace is not currently spawned (WorkspaceID is
// empty), AttachThread returns a read-only workspace over the parent
// ClientWorkspace so the caller can still inspect persisted session data
// without risking mutations or tearing down the parent.
func (w *ClientWorkspace) AttachThread(ctx context.Context, id string) (Workspace, func(), error) {
	st, err := w.client.GetThread(ctx, w.workspaceID(), id)
	if err != nil {
		return nil, nil, err
	}
	if st.WorkspaceID == "" {
		// Thread is not currently spawned (completed, interrupted, failed).
		// Return a read-only workspace over the parent so the caller
		// can still inspect persisted session data.
		ro := newReadOnlyWorkspace(w, st.WorktreePath, st.SessionID)
		return ro, func() {}, nil
	}
	derived := w.client.WithClientID(uuid.NewString())
	ws, err := derived.GetWorkspace(ctx, st.WorkspaceID)
	if err != nil {
		return nil, nil, err
	}
	cw := NewClientWorkspace(derived, *ws)
	return cw, cw.Shutdown, nil
}

// SubscribeWith runs this workspace's event subscription with a plain
// send callback instead of a *tea.Program, for callers (e.g. the TUI
// attaching to a thread's own workspace via Workspace.AttachThread) that
// need a second, independently stoppable subscription rather than the
// *tea.Program-bound Subscribe/Shutdown lifecycle. The returned stop func
// cancels the subscription and waits for its loop to exit; call it
// exactly once.
func (w *ClientWorkspace) SubscribeWith(send func(tea.Msg)) func() {
	go func() {
		// No *tea.Program to Quit on panic here (unlike Subscribe); log
		// and let the subscription simply stop.
		defer log.RecoverPanic("ClientWorkspace.SubscribeWith", func() {})
		w.runSubscription(send)
	}()
	return func() {
		w.subCancel()
		w.awaitSubscription()
	}
}
