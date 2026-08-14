package workspace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/braid/internal/app/threadspawn"
	"github.com/rave-soft/braid/internal/client"
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

// InitializeThreadsCapability is deliberately a cheap no-op for local
// workspaces: their thread manager is already available in-process.
func (w *AppWorkspace) InitializeThreadsCapability(context.Context) error { return nil }

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

// -- ClientWorkspace: Threads --

type threadsSupport uint8

const (
	threadsUnknown threadsSupport = iota
	threadsUnsupported
	threadsSupported
)

// SupportsThreads returns only the cached capability state. It never performs
// I/O: callers that must resolve an older server's unknown snapshot first use
// InitializeThreadsCapability.
func (w *ClientWorkspace) SupportsThreads() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.supportsThreads == threadsSupported
}

// InitializeThreadsCapability resolves an unknown capability snapshot with one
// bounded request. A 409 is a definitive unsupported result; other request
// failures leave the capability unknown and are returned to the caller.
func (w *ClientWorkspace) InitializeThreadsCapability(ctx context.Context) error {
	for {
		w.mu.Lock()
		if w.supportsThreads != threadsUnknown {
			w.mu.Unlock()
			return nil
		}
		if w.threadsProbeAdmissionOff {
			w.mu.Unlock()
			return context.Canceled
		}
		if w.threadsProbeComplete {
			err := w.threadsProbeErr
			w.mu.Unlock()
			return err
		}
		if w.threadsProbeStarted {
			done := w.threadsProbeDone
			w.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		w.threadsProbeStarted = true
		w.threadsProbeDone = make(chan struct{})
		done := w.threadsProbeDone
		workspaceID, generation := w.ws.ID, w.threadsProbeGeneration
		w.threadsProbes.Add(1)
		w.mu.Unlock()

		attemptCtx, cancel := context.WithTimeout(ctx, threadsProbeTimeout)
		stopCancel := context.AfterFunc(w.subCtx, cancel)
		_, err := w.client.ListThreads(attemptCtx, workspaceID)
		stopCancel()
		cancel()

		w.mu.Lock()
		current := w.ws.ID == workspaceID &&
			w.threadsProbeGeneration == generation &&
			w.threadsProbeDone == done
		if current {
			if err == nil {
				w.supportsThreads = threadsSupported
				err = nil
			} else if errors.Is(err, client.ErrThreadsUnsupported) {
				w.supportsThreads = threadsUnsupported
				err = nil
			} else {
				err = fmt.Errorf("determine thread capability: %w", err)
			}
			w.threadsProbeErr = err
			w.threadsProbeComplete = true
			w.threadsProbeStarted = false
		} else if w.threadsProbeDone == done {
			w.threadsProbeStarted = false
		}
		close(done)
		w.mu.Unlock()
		w.threadsProbes.Done()
		if current {
			return err
		}
	}
}

func (w *ClientWorkspace) setThreadsSupported(supported *bool) {
	switch {
	case supported == nil:
		w.supportsThreads = threadsUnknown
	case *supported:
		w.supportsThreads = threadsSupported
	default:
		w.supportsThreads = threadsUnsupported
	}
}

func (w *ClientWorkspace) startThreadsProbe() {
	if w.client == nil {
		return
	}
	w.mu.Lock()
	if w.threadsProbeAdmissionOff || w.supportsThreads != threadsUnknown || w.threadsProbeStarted {
		w.mu.Unlock()
		return
	}
	w.threadsProbeStarted = true
	w.threadsProbeDone = make(chan struct{})
	workspaceID, generation := w.ws.ID, w.threadsProbeGeneration
	w.threadsProbes.Add(1)
	w.mu.Unlock()

	done := w.threadsProbeDone
	go func() {
		defer w.threadsProbes.Done()
		w.probeThreads(w.subCtx, workspaceID, generation, done)
	}()
}

func (w *ClientWorkspace) probeThreads(ctx context.Context, workspaceID string, generation uint64, done chan struct{}) {
	attemptCtx, cancel := context.WithTimeout(ctx, threadsProbeTimeout)
	_, err := w.client.ListThreads(attemptCtx, workspaceID)
	cancel()

	w.mu.Lock()
	current := w.ws.ID == workspaceID &&
		w.threadsProbeGeneration == generation &&
		w.threadsProbeDone == done
	if !current {
		if w.threadsProbeDone == done {
			w.threadsProbeStarted = false
		}
		close(done)
		w.mu.Unlock()
		w.startThreadsProbe()
		return
	}
	if err == nil {
		w.supportsThreads = threadsSupported
		err = nil
	} else if errors.Is(err, client.ErrThreadsUnsupported) {
		w.supportsThreads = threadsUnsupported
		err = nil
	} else if ctx.Err() == nil && !w.threadsProbeAdmissionOff {
		err = fmt.Errorf("determine thread capability: %w", err)
	} else {
		err = nil
	}
	w.threadsProbeErr = err
	w.threadsProbeComplete = true
	w.threadsProbeStarted = false
	close(done)
	w.mu.Unlock()
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

func (w *ClientWorkspace) ActivateThread(ctx context.Context, id string) (proto.Thread, error) {
	st, err := w.client.ActivateThread(ctx, w.workspaceID(), id)
	if err != nil {
		return proto.Thread{}, err
	}
	return *st, nil
}

func (w *ClientWorkspace) MergeThread(ctx context.Context, id string) (proto.Thread, error) {
	st, err := w.client.MergeThread(ctx, w.workspaceID(), id)
	if err != nil {
		return proto.Thread{}, err
	}
	return *st, nil
}

func (w *ClientWorkspace) CancelThread(ctx context.Context, id, reason string) error {
	return w.client.CancelThread(ctx, w.workspaceID(), id, reason)
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
		// Thread is not currently spawned (completed, interrupted,
		// failed). Ask the server to reactivate it so attaching lands in
		// a writable session, mirroring local mode.
		activated, err := w.client.ActivateThread(ctx, w.workspaceID(), id)
		if err != nil {
			// Reactivation is refused for threads in the merge flow and
			// fails if the worktree is gone. Fall back to a read-only
			// workspace over the parent so the caller can still inspect
			// persisted session data.
			slog.Debug("Thread reactivation unavailable during attach, falling back to read-only", "thread", id, "error", err)
			ro := newReadOnlyWorkspace(w, st.WorktreePath, st.SessionID)
			return ro, func() {}, nil
		}
		st = activated
		if st.WorkspaceID == "" {
			// Activation reported success without a live workspace.
			// There is nothing to attach to, so stay read-only rather
			// than requesting an empty workspace ID.
			slog.Warn("Activated thread reported no workspace during attach", "thread", id)
			ro := newReadOnlyWorkspace(w, st.WorktreePath, st.SessionID)
			return ro, func() {}, nil
		}
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
