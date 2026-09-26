package grpcws

import (
	"context"
	"log/slog"

	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc"
)

// Connect takes a Snapshot to seed this Client's cache, then starts the
// single internal event pump every Subscribe/SubscribeWith attachment and
// every class-C getter rides from then on (CLIENT-SERVER.md, PR 1.4b,
// build step 1). Calling it more than once does nothing beyond the first
// call: a caller that never calls it explicitly gets the same effect
// implicitly the first time it calls Subscribe/SubscribeWith
// (ensureConnected), and callHandles calls it eagerly for every child
// Client it mints (EnterWorktree/ExitWorktree/AttachThread).
func (c *Client) Connect(ctx context.Context) error {
	c.connectOnce.Do(func() {
		c.connectErr = c.connect(ctx)
	})
	return c.connectErr
}

// ensureConnected is Connect for a caller (Subscribe/SubscribeWith) that
// doesn't itself report a connection failure through its own signature: it
// logs instead, once, and lets the caller proceed -- attaching to a pump
// that never started still works, it just never delivers anything beyond
// the synthesized ConnectionLost this same failure would otherwise produce
// on a real stream.
func (c *Client) ensureConnected(ctx context.Context) {
	if err := c.Connect(ctx); err != nil {
		slog.Error("Wsrpc client failed to connect", "handle", c.handle, "error", err)
	}
}

// connect is Connect's actual body, run at most once by connectOnce.
func (c *Client) connect(ctx context.Context) error {
	snap, err := c.Snapshot(ctx)
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.state = snap.State
	c.haveState = true
	c.pendingPermissions = permissionMap(snap.PendingPermissions)
	c.pendingQuestions = questionMap(snap.PendingQuestions)
	if c.subs == nil {
		c.subs = map[*clientSub]struct{}{}
	}
	pumpCtx, cancel := context.WithCancel(c.lifeCtx)
	done := make(chan struct{})
	c.pumpCancel = cancel
	c.pumpDone = done
	c.mu.Unlock()

	go func() {
		defer close(done)
		c.runPump(pumpCtx, snap.Seq+1)
	}()
	return nil
}

// runPump is Connect's event loop: open the Events service's Subscribe
// stream at fromSeq, decode every EventFrame through wsrpc's event
// registry and dispatch the result to the cache and every attached
// subscriber, and on any stream error reconnect with capped backoff --
// FromSeq = lastSeen+1, so a reconnect after a short blip replays exactly
// what was missed instead of either losing events or redelivering ones
// already seen. A Resync frame (the server's buffer didn't have what was
// asked for) is handled by resync: a fresh Snapshot replaces the cache and
// pending set wholesale, subscribers get ConnectionEvent{Resync} followed
// by the fresh snapshot's pending prompts, and fromSeq picks up from the
// new snapshot's own Seq+1.
func (c *Client) runPump(ctx context.Context, fromSeq uint64) {
	backoff := initialReconnectBackoff
	// connect already reached the server once (the Snapshot call that
	// seeded fromSeq), so the pump starts in the "connected" state -- the
	// first real failure is what should announce ConnectionLost.
	connected := true
	for ctx.Err() == nil {
		stream, err := c.openEventStream(ctx, fromSeq)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if connected {
				c.dispatch(connectionEvent(workspace.ConnectionLost))
				connected = false
			}
			if !sleepBackoff(ctx, &backoff) {
				return
			}
			continue
		}

		resyncing := false
		for {
			frame, err := stream.Recv()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				if connected {
					c.dispatch(connectionEvent(workspace.ConnectionLost))
				}
				connected = false
				break
			}
			backoff = initialReconnectBackoff
			if !connected {
				c.dispatch(connectionEvent(workspace.ConnectionRecovered))
				connected = true
			}
			if frame.Resync {
				resyncing = true
				break
			}
			if frame.Event == nil {
				continue
			}
			v, err := wsrpc.DecodeEvent(*frame.Event)
			if err != nil {
				slog.Error("Wsrpc client failed to decode event frame, dropping it", "error", err)
				continue
			}
			c.dispatch(v)
			fromSeq = frame.Seq + 1
		}

		if resyncing {
			if newSeq, ok := c.resync(ctx); ok {
				fromSeq = newSeq
				continue
			}
			if ctx.Err() != nil {
				return
			}
			connected = false
		}

		if !sleepBackoff(ctx, &backoff) {
			return
		}
	}
}

// resync takes a fresh Snapshot (bounded by this Client's own call
// timeout) and replaces the cache and pending set with it wholesale,
// reporting ConnectionEvent{Resync} to every subscriber followed by the
// new snapshot's own pending permission/question requests as request
// events -- the same shape a subscriber attaching fresh would see (see
// attachSubscriber). Reports ok=false (leaving the cache and pending set
// untouched) when the Snapshot call itself fails; the caller falls back to
// its own reconnect-with-backoff loop in that case.
func (c *Client) resync(ctx context.Context) (nextSeq uint64, ok bool) {
	callCtx, cancel := context.WithTimeout(ctx, c.callTimeout)
	defer cancel()
	snap, err := c.Snapshot(callCtx)
	if err != nil {
		slog.Error("Wsrpc client failed to resync, will retry", "handle", c.handle, "error", err)
		return 0, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = snap.State
	c.haveState = true
	c.pendingPermissions = permissionMap(snap.PendingPermissions)
	c.pendingQuestions = questionMap(snap.PendingQuestions)
	for s := range c.subs {
		s.send(connectionEvent(workspace.ConnectionResync))
	}
	for s := range c.subs {
		deliverPending(s, snap.PendingPermissions, snap.PendingQuestions)
	}
	return snap.Seq + 1, true
}

// dispatch applies v to the cache (applyLocked) and fans it out to every
// currently attached subscriber, both under c.mu -- see the Client struct's
// own doc comment for why attach/detach share this same lock.
func (c *Client) dispatch(v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.applyLocked(v)
	for s := range c.subs {
		s.send(v)
	}
}

// applyLocked updates the cached workspace.ClientState and the pending
// permission/question sets from one decoded event. Called with c.mu held.
//
//   - workspace.ClientState: applied wholesale, but only if its Version is
//     strictly greater than the cached one -- the idempotency guarantee
//     ClientState's own doc comment describes, covering both a replayed
//     event and one that raced a resync's own Snapshot.
//   - permission.PermissionRequest / question.Request: recorded as
//     pending, keyed by ToolCallID / ID (== the question batch ID).
//   - permission.PermissionNotification / question.Notification: the
//     matching pending entry is resolved, so it is removed.
//
// Any other event type (message.Message, session.Session, ...) passes
// through dispatch unchanged; applyLocked has nothing to do with it.
func (c *Client) applyLocked(v any) {
	switch e := v.(type) {
	case pubsub.Event[workspace.ClientState]:
		if e.Payload.Version > c.state.Version {
			c.state = e.Payload
		}
	case pubsub.Event[permission.PermissionRequest]:
		if c.pendingPermissions == nil {
			c.pendingPermissions = map[string]permission.PermissionRequest{}
		}
		c.pendingPermissions[e.Payload.ToolCallID] = e.Payload
	case pubsub.Event[permission.PermissionNotification]:
		delete(c.pendingPermissions, e.Payload.ToolCallID)
	case pubsub.Event[question.Request]:
		if c.pendingQuestions == nil {
			c.pendingQuestions = map[string]question.Request{}
		}
		c.pendingQuestions[e.Payload.ID] = e.Payload
	case pubsub.Event[question.Notification]:
		delete(c.pendingQuestions, e.Payload.BatchID)
	}
}

// attachSubscriber registers sub and immediately replays every currently
// pending permission/question request to it as a request event, under the
// same lock dispatch uses -- so a live event racing this attach either
// lands entirely before (sub sees it in the replay, since applyLocked
// already folded it into the pending set) or entirely after (sub is
// already registered by the time dispatch iterates c.subs), never
// interleaved with the replay itself. This is what lets a UI attaching to
// a daemon see a prompt that has been waiting with no client
// (CLIENT-SERVER.md, PR 1.4b, build step 2).
func (c *Client) attachSubscriber(sub *clientSub) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.subs == nil {
		c.subs = map[*clientSub]struct{}{}
	}
	c.subs[sub] = struct{}{}
	for _, p := range c.pendingPermissions {
		sub.send(pubsub.Event[permission.PermissionRequest]{Type: pubsub.CreatedEvent, Payload: p})
	}
	for _, q := range c.pendingQuestions {
		sub.send(pubsub.Event[question.Request]{Type: pubsub.CreatedEvent, Payload: q})
	}
}

// detachSubscriber removes sub from the fan-out set. Safe to call even if
// sub was never attached (or already detached).
func (c *Client) detachSubscriber(sub *clientSub) {
	c.mu.Lock()
	delete(c.subs, sub)
	c.mu.Unlock()
}

// deliverPending sends every entry of perms/questions to sub as a request
// event -- the shape attachSubscriber and resync both need, factored out
// so they can't drift apart on it.
func deliverPending(sub *clientSub, perms []permission.PermissionRequest, questions []question.Request) {
	for _, p := range perms {
		sub.send(pubsub.Event[permission.PermissionRequest]{Type: pubsub.CreatedEvent, Payload: p})
	}
	for _, q := range questions {
		sub.send(pubsub.Event[question.Request]{Type: pubsub.CreatedEvent, Payload: q})
	}
}

// permissionMap/questionMap key a Snapshot's pending slices the way the
// cache does (permission.PermissionRequest by ToolCallID,
// question.Request by ID -- the question batch ID that
// question.Notification.BatchID later matches against).
func permissionMap(reqs []permission.PermissionRequest) map[string]permission.PermissionRequest {
	m := make(map[string]permission.PermissionRequest, len(reqs))
	for _, r := range reqs {
		m[r.ToolCallID] = r
	}
	return m
}

func questionMap(reqs []question.Request) map[string]question.Request {
	m := make(map[string]question.Request, len(reqs))
	for _, r := range reqs {
		m[r.ID] = r
	}
	return m
}
