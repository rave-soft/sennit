package workspace

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/braid/internal/log"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/strand"
)

// -- AppWorkspace: Strands --

// strandManager returns this workspace's *strand.Manager and whether one
// is attached. app.StrandManager() returns `any` because internal/app
// (core, imported by internal/agent) cannot import internal/strand
// directly — see internal/app/app.go's SetStrandManager/StrandManager
// doc comments for the layering reason.
func (w *AppWorkspace) strandManager() (*strand.Manager, bool) {
	mgr, ok := w.app.StrandManager().(*strand.Manager)
	return mgr, ok && mgr != nil
}

func (w *AppWorkspace) SupportsStrands() bool {
	_, ok := w.strandManager()
	return ok
}

func (w *AppWorkspace) ListStrands(ctx context.Context) ([]proto.Strand, error) {
	mgr, ok := w.strandManager()
	if !ok {
		return nil, ErrStrandsNotSupported
	}
	sts, err := mgr.List(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]proto.Strand, len(sts))
	for i, st := range sts {
		result[i] = mgr.ToProto(st)
	}
	return result, nil
}

func (w *AppWorkspace) GetStrand(ctx context.Context, id string) (proto.Strand, error) {
	mgr, ok := w.strandManager()
	if !ok {
		return proto.Strand{}, ErrStrandsNotSupported
	}
	st, err := mgr.Get(ctx, id)
	if err != nil {
		return proto.Strand{}, err
	}
	return mgr.ToProto(st), nil
}

func (w *AppWorkspace) CreateStrand(ctx context.Context, req proto.CreateStrandRequest) (proto.Strand, error) {
	mgr, ok := w.strandManager()
	if !ok {
		return proto.Strand{}, ErrStrandsNotSupported
	}
	st, err := mgr.Create(ctx, strand.CreateArgs{
		Name:        req.Name,
		Goal:        req.Goal,
		BaseBranch:  req.BaseBranch,
		MergePolicy: strand.MergePolicy(req.MergePolicy),
	})
	if err != nil {
		return proto.Strand{}, err
	}
	return mgr.ToProto(st), nil
}

func (w *AppWorkspace) SendStrand(ctx context.Context, id, message string) error {
	mgr, ok := w.strandManager()
	if !ok {
		return ErrStrandsNotSupported
	}
	return mgr.Send(ctx, id, message)
}

func (w *AppWorkspace) MergeStrand(ctx context.Context, id string) (proto.Strand, error) {
	mgr, ok := w.strandManager()
	if !ok {
		return proto.Strand{}, ErrStrandsNotSupported
	}
	if err := mgr.Merge(ctx, id); err != nil {
		return proto.Strand{}, err
	}
	// Re-fetch after merge to return the latest status, mirroring the
	// server handler.
	st, err := mgr.Get(ctx, id)
	if err != nil {
		return proto.Strand{}, err
	}
	return mgr.ToProto(st), nil
}

func (w *AppWorkspace) RemoveStrand(ctx context.Context, id string, opts proto.RemoveStrandOptions) error {
	mgr, ok := w.strandManager()
	if !ok {
		return ErrStrandsNotSupported
	}
	return mgr.Remove(ctx, id, opts.Force, opts.DeleteBranch)
}

func (w *AppWorkspace) AttachStrand(ctx context.Context, id string) (Workspace, func(), error) {
	mgr, ok := w.strandManager()
	if !ok {
		return nil, nil, ErrStrandsNotSupported
	}
	h := mgr.Handle(id)
	if h == nil {
		return nil, nil, fmt.Errorf("strand %q is not currently running", id)
	}
	a := h.App()
	aw := NewAppWorkspace(a, a.Store())
	// detach is a no-op for local mode: the strand's App is owned by the
	// Manager, not by whoever is viewing it, so there is nothing to
	// release here — teardown happens via Manager.Remove or process
	// shutdown, not via this detach.
	return aw, func() {}, nil
}

// SubscribeWith runs a second, independently stoppable event subscription
// against this workspace's App, for callers (e.g. the TUI attaching to a
// strand's own workspace via Workspace.AttachStrand) that need a plain
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

// -- ClientWorkspace: Strands --

// SupportsStrands does a live round trip to the server: unlike
// AgentIsReady/AgentIsBusy (which read locally-cached state that's kept
// fresh by the subscription stream), there is no cached "does this
// workspace support strands" bit anywhere in proto.Workspace, so the
// only accurate answer requires asking the server; a cheap-but-wrong
// boolean would be worse than a slightly expensive correct one.
//
// SupportsStrands has no ctx parameter (see the StrandController
// interface), so this uses context.Background() internally, meaning the
// call cannot be cancelled by a caller.
func (w *ClientWorkspace) SupportsStrands() bool {
	_, err := w.client.ListStrands(context.Background(), w.workspaceID())
	return err == nil
}

func (w *ClientWorkspace) ListStrands(ctx context.Context) ([]proto.Strand, error) {
	return w.client.ListStrands(ctx, w.workspaceID())
}

func (w *ClientWorkspace) GetStrand(ctx context.Context, id string) (proto.Strand, error) {
	st, err := w.client.GetStrand(ctx, w.workspaceID(), id)
	if err != nil {
		return proto.Strand{}, err
	}
	return *st, nil
}

func (w *ClientWorkspace) CreateStrand(ctx context.Context, req proto.CreateStrandRequest) (proto.Strand, error) {
	st, err := w.client.CreateStrand(ctx, w.workspaceID(), req)
	if err != nil {
		return proto.Strand{}, err
	}
	return *st, nil
}

func (w *ClientWorkspace) SendStrand(ctx context.Context, id, message string) error {
	return w.client.SendStrand(ctx, w.workspaceID(), id, message)
}

func (w *ClientWorkspace) MergeStrand(ctx context.Context, id string) (proto.Strand, error) {
	st, err := w.client.MergeStrand(ctx, w.workspaceID(), id)
	if err != nil {
		return proto.Strand{}, err
	}
	return *st, nil
}

func (w *ClientWorkspace) RemoveStrand(ctx context.Context, id string, opts proto.RemoveStrandOptions) error {
	return w.client.RemoveStrand(ctx, w.workspaceID(), id, opts)
}

// AttachStrand connects to a strand's own backend workspace using a
// DERIVED client identity ("<parent-client-id>/strand/<strand-id>"),
// never the parent ClientWorkspace's own client ID. This matters
// because the returned Workspace's detach func calls Shutdown, which
// retires the client (or deletes the workspace on an older server) —
// if it shared the parent's client ID, detaching an attached strand
// view would tear down the PARENT workspace's own claim too. The
// derived client shares the same underlying *http.Client/connection
// pool (see client.Client.WithClientID), so this costs nothing beyond
// one extra small struct.
func (w *ClientWorkspace) AttachStrand(ctx context.Context, id string) (Workspace, func(), error) {
	st, err := w.client.GetStrand(ctx, w.workspaceID(), id)
	if err != nil {
		return nil, nil, err
	}
	if st.WorkspaceID == "" {
		return nil, nil, fmt.Errorf("strand %q is not currently running", id)
	}
	derived := w.client.WithClientID(w.client.ClientID() + "/strand/" + id)
	ws, err := derived.GetWorkspace(ctx, st.WorkspaceID)
	if err != nil {
		return nil, nil, err
	}
	cw := NewClientWorkspace(derived, *ws)
	return cw, cw.Shutdown, nil
}

// SubscribeWith runs this workspace's event subscription with a plain
// send callback instead of a *tea.Program, for callers (e.g. the TUI
// attaching to a strand's own workspace via Workspace.AttachStrand) that
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
