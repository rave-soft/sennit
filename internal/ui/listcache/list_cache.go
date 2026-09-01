// Package listcache holds the machinery two cached lists in the TUI share:
// DispatchRefresh fetches a []proto.Thread list off-thread, ApplyLoaded
// writes it through on the Update goroutine (discarding and re-dispatching
// stale generations, backing off after a failure), ApplyEvent reacts to a
// pubsub thread event by upserting or removing its row, and StaleRefreshCmd
// is the TTL backstop.
//
// Its callers are internal/ui/model's threadListCache and agentListCache.
// They used to type the whole sequence out twice, differing only in which
// off-thread call fetches the list, whether the workspace Supports it,
// which proto.ThreadKind their own pubsub events belong to, and the Label
// used in the "failed to list" log line — which is what Ops carries.
//
// It lives outside internal/ui/model because it is not about any one
// screen: the threads dashboard, the delegations panel and the workspace
// cache all use it, and a feature that wants to move to its own package
// should not have to take this with it or leave it behind.
package listcache

import (
	"context"
	"log/slog"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
)

// Ops supplies what differs between the panel's two live thread-
// list caches — see the package doc comment above. Built Fresh (cheaply: a
// handful of function values and constants) on every call rather than
// stored on the cache struct, so threadListCache/agentListCache stay
// zero-Value-safe the way TTLCache itself is.
// RefreshBackoff is how long a failed list or activity refresh waits before being retried. Without it a refresh that fails every
// time re-dispatches on every Update — and since the failure's own result
// message is itself an Update, the loop feeds itself and pins the event
// loop (observed: ~830 attempts a second, 10MB of identical error lines
// every half minute, a UI that looks frozen and background work that looks
// like it stopped on its own).
//
// It is the default every list picks in its own Ops (see Backoff there),
// not something the shared machinery reaches for: a list that wants a
// different one should say so rather than inherit this by accident.
//
// Longer than any of the TTLs it backs: a repeatedly failing probe is worth
// far less than a successful one, and the states that produce a permanent
// failure (a read-only workspace, a removed worktree) do not resolve on
// their own in seconds.
var RefreshBackoff = 30 * time.Second

type Ops[Msg any, Workspace any] struct {
	Label string        // for slog.Error's "Failed to list <label>"
	TTL   time.Duration // StaleRefreshCmd's freshness window
	// Backoff is how long to wait before re-probing after a failed fetch.
	// It used to be read from a threads-specific package variable inside
	// StaleRefreshCmd, which meant every other list — the agents cache
	// today, anything added tomorrow — silently inherited the threads
	// backoff without saying so anywhere.
	Backoff   time.Duration
	Kind      proto.ThreadKind // which pubsub events this cache's ApplyEvent accepts
	Available func(Workspace) bool
	Supports  func(Workspace) bool
	Fetch     func(context.Context, Workspace) ([]proto.Thread, error)
	Wrap      func(gen uint64, items []proto.Thread, err error) Msg
	Unwrap    func(Msg) (gen uint64, items []proto.Thread, err error)
}

// DispatchRefresh returns a command that lists ops' items off the
// Update goroutine, delivering a Msg. It returns nil while a Fetch is
// already in flight, or if the workspace doesn't support this list. The
// closure captures only locals (never *common.Common) so it is safe
// off-thread; state is applied by ApplyLoaded on the Update goroutine.
func DispatchRefresh[Msg any, Workspace any](cache *TTLCache[[]proto.Thread], ws Workspace, ctx context.Context, ops Ops[Msg, Workspace]) tea.Cmd {
	if cache.InFlight || !ops.Available(ws) || !ops.Supports(ws) {
		return nil
	}
	gen, started := cache.Begin()
	if !started {
		return nil
	}
	return func() tea.Msg {
		items, err := ops.Fetch(ctx, ws)
		if err != nil {
			slog.Error("Failed to list "+ops.Label, "error", err)
		}
		return ops.Wrap(gen, items, err)
	}
}

// ApplyLoaded stores an off-thread Fetch result. Runs on the Update
// goroutine. applied reports whether msg was actually written through
// (true) as opposed to discarded for a stale Generation or a failure
// (false).
func ApplyLoaded[Msg any, Workspace any](cache *TTLCache[[]proto.Thread], ws Workspace, ctx context.Context, ops Ops[Msg, Workspace], msg Msg) (cmds []tea.Cmd, applied bool) {
	gen, items, err := ops.Unwrap(msg)
	if err != nil {
		if !cache.Fail(gen) {
			// Started before a newer state transition; discard and
			// re-dispatch so the authoritative refresh isn't lost.
			if cmd := DispatchRefresh(cache, ws, ctx, ops); cmd != nil {
				return []tea.Cmd{cmd}, false
			}
		}
		return nil, false
	}
	if !cache.Complete(gen) {
		// This Fetch started before a newer state transition (invalidation,
		// event edge). Discard its result and re-dispatch so the
		// authoritative refresh is not lost merely because this older
		// request was in flight.
		if cmd := DispatchRefresh(cache, ws, ctx, ops); cmd != nil {
			return []tea.Cmd{cmd}, false
		}
		return nil, false
	}
	cache.Set(items)
	return nil, true
}

// ApplyEvent reacts to a thread pubsub event belonging to Kind: it
// upserts (Created, Updated) or removes (Deleted) the event's row in the
// cached list, then invalidates the TTL so a background refresh eventually
// reconciles with the authoritative list. An event for a different Kind is
// ignored — see threadEventMatchesKind — except a Deleted event, which is
// never filtered: dropping a row this cache does not hold is already a
// no-op, and refusing to drop one it somehow does hold would strand it.
func ApplyEvent(cache *TTLCache[[]proto.Thread], Kind proto.ThreadKind, evt pubsub.Event[proto.Thread]) {
	if evt.Type != pubsub.DeletedEvent && !threadEventMatchesKind(proto.ThreadKind(evt.Payload.Kind), Kind) {
		return
	}
	switch evt.Type {
	case pubsub.DeletedEvent:
		for i := range cache.Value {
			if cache.Value[i].ID == evt.Payload.ID {
				cache.Value = append(cache.Value[:i], cache.Value[i+1:]...)
				break
			}
		}
	default: // CreatedEvent, UpdatedEvent
		found := false
		for i := range cache.Value {
			if cache.Value[i].ID == evt.Payload.ID {
				cache.Value[i] = evt.Payload
				found = true
				break
			}
		}
		if !found {
			cache.Value = append(cache.Value, evt.Payload)
		}
	}
	cache.Invalidate()
}

// threadEventMatchesKind reports whether a pubsub event payload's Kind
// belongs to target. Kind is additive (see proto.Thread.Kind), so a payload
// that predates it carries an empty Kind and describes a thread — an empty
// Kind therefore matches only proto.ThreadKindThread, never
// proto.ThreadKindTask.
func threadEventMatchesKind(payloadKind, target proto.ThreadKind) bool {
	if payloadKind == "" {
		return target == proto.ThreadKindThread
	}
	return payloadKind == target
}

// StaleRefreshCmd is the TTL backstop: while active and the memoized
// list has outlived ops.TTL, it schedules an off-thread re-probe. It never
// does IO itself.
//
// A fetched-and-empty list stays empty until a matching event invalidates
// it (the Timestamp is zeroed then) — don't re-poll forever for a workspace
// with nothing in this particular list.
func StaleRefreshCmd[Msg any, Workspace any](cache *TTLCache[[]proto.Thread], ws Workspace, ctx context.Context, active bool, ops Ops[Msg, Workspace]) tea.Cmd {
	if !active || cache.Fresh(ops.TTL) || cache.BackingOff(ops.Backoff) {
		return nil
	}
	if len(cache.Value) == 0 && !cache.Timestamp.IsZero() {
		return nil
	}
	return DispatchRefresh(cache, ws, ctx, ops)
}
