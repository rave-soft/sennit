package thread

import (
	"context"

	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/pubsub"
)

// forwardPermissions relays a delegation workspace's permission traffic
// into the parent workspace's event stream, for as long as that workspace
// is live.
//
// A thread runs in an isolated app.App with its own permission service and
// its own event broker. The user's TUI is subscribed to the parent
// workspace, so without this relay a prompt raised inside a thread reaches
// nobody: permission.Service.Request blocks on its response channel with
// no timeout, and the thread hangs there forever with no visible sign of
// why. Anything that needs approval - bash, above all - stops the thread
// dead.
//
// Requests carry their delegation's identity (see withDelegation), which
// is what lets the parent both label the prompt and route the answer back
// to the service that is actually waiting for it - see
// [Manager.PermissionsFor].
//
// Bound to the runtime's watch context, so the relay stops when the
// workspace is released. A task needs none of this: its handle wraps the
// parent's own App, so its prompts are already on the stream the user is
// watching.
func (l *lifecycle) forwardPermissions(ctx context.Context, handle Handle) {
	if l.parentApp == nil {
		return
	}
	a := handle.Workspace()
	if a == nil || a == l.parentApp {
		return
	}
	forwardInto(ctx, l, a.Permissions().Subscribe)
	forwardInto(ctx, l, a.Permissions().SubscribeNotifications)

	// A request already waiting when the relay starts was published
	// before anything was listening, and a request is announced exactly
	// once - so without this it would sit there unanswerable, which is
	// the failure this whole relay exists to prevent. Reachable whenever
	// a workspace is re-installed under a delegation that still has a
	// prompt outstanding.
	if req, ok := a.Permissions().ActiveRequest(); ok {
		l.parentApp.SendEvent(pubsub.Event[permission.PermissionRequest]{
			Type:    pubsub.CreatedEvent,
			Payload: req,
		})
	}
}

// forwardInto pumps one of a delegation workspace's event sources onto the
// parent App's fan-in, republishing each event unchanged so a subscriber
// cannot tell it apart from one the parent raised itself - which is the
// point: the TUI's permission handling should not need to know that the
// asking workspace is somewhere else.
//
// A free function rather than a method because Go has no generic methods.
func forwardInto[T any](ctx context.Context, l *lifecycle, subscribe func(context.Context) <-chan pubsub.Event[T]) {
	sub := subscribe(ctx)
	l.goWorker(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-sub:
				if !ok {
					return
				}
				l.parentApp.SendEvent(ev)
			}
		}
	})
}
