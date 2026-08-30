package model

// Generic machinery shared by threadListCache (threads_cache.go) and
// agentListCache (agents_cache.go): dispatchRefresh fetches a
// []proto.Thread list off-thread, applyLoaded writes it through on the
// Update goroutine (discarding and re-dispatching stale generations,
// backing off after a failure), applyEvent reacts to a pubsub thread event
// by upserting or removing its row, and staleRefreshCmd is the TTL
// backstop. The two caches used to type this whole sequence out twice,
// differing only in which off-thread call fetches the list, whether the
// workspace supports it, which proto.ThreadKind their own pubsub events
// belong to, and the label used in the "failed to list" log line —
// exactly what listCacheOps below carries.
//
// Each cache keeps its own named struct (threadListCache, agentListCache)
// holding just a ttlCache[[]proto.Thread], and its own concrete
// loaded-message type (threadsLoadedMsg, agentsLoadedMsg) — kept concrete
// and distinct, rather than folded into one shared message type, since
// root.go and update_threads.go still need to type-switch on it. A cache's
// five methods are thin wrappers that build a listCacheOps value inline and
// delegate to the functions here.
//
// threads_dock.go's per-thread activity cache is a third, superficially
// similar cache that is deliberately NOT folded in here: it is keyed by
// thread ID rather than holding one slice, its value type is
// threadDockActivity rather than []proto.Thread, and its fetch has its own
// error-delivery contract (always sends a message, even on failure, so a
// per-key inFlight flag is never left stuck) plus a reuse-if-unchanged
// optimization (see dispatchThreadActivityRefresh). Bending this generic to
// also cover that shape would either drop the per-key structure or bloat it
// with dock-only hooks — worse than leaving it as its own small, documented
// piece of code.

import (
	"context"
	"log/slog"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/workspace"
)

// listCacheOps supplies what differs between the panel's two live thread-
// list caches — see the package doc comment above. Built fresh (cheaply: a
// handful of function values and constants) on every call rather than
// stored on the cache struct, so threadListCache/agentListCache stay
// zero-value-safe the way ttlCache itself is.
// listRefreshBackoff is how long a failed list or activity refresh waits before being retried. Without it a refresh that fails every
// time re-dispatches on every Update — and since the failure's own result
// message is itself an Update, the loop feeds itself and pins the event
// loop (observed: ~830 attempts a second, 10MB of identical error lines
// every half minute, a UI that looks frozen and background work that looks
// like it stopped on its own).
//
// It is the default every list picks in its own ops (see backoff there),
// not something the shared machinery reaches for: a list that wants a
// different one should say so rather than inherit this by accident.
//
// Longer than any of the TTLs it backs: a repeatedly failing probe is worth
// far less than a successful one, and the states that produce a permanent
// failure (a read-only workspace, a removed worktree) do not resolve on
// their own in seconds.
var listRefreshBackoff = 30 * time.Second

type listCacheOps[Msg any] struct {
	label string        // for slog.Error's "Failed to list <label>"
	ttl   time.Duration // staleRefreshCmd's freshness window
	// backoff is how long to wait before re-probing after a failed fetch.
	// It used to be read from a threads-specific package variable inside
	// staleListRefreshCmd, which meant every other list — the agents cache
	// today, anything added tomorrow — silently inherited the threads
	// backoff without saying so anywhere.
	backoff  time.Duration
	kind     proto.ThreadKind // which pubsub events this cache's applyEvent accepts
	supports func(workspace.Workspace) bool
	fetch    func(context.Context, workspace.Workspace) ([]proto.Thread, error)
	wrap     func(gen uint64, items []proto.Thread, err error) Msg
	unwrap   func(Msg) (gen uint64, items []proto.Thread, err error)
}

// dispatchListRefresh returns a command that lists ops' items off the
// Update goroutine, delivering a Msg. It returns nil while a fetch is
// already in flight, or if the workspace doesn't support this list. The
// closure captures only locals (never *common.Common) so it is safe
// off-thread; state is applied by applyListLoaded on the Update goroutine.
func dispatchListRefresh[Msg any](cache *ttlCache[[]proto.Thread], com *common.Common, ops listCacheOps[Msg]) tea.Cmd {
	if cache.inFlight || com == nil || com.Workspace == nil || !ops.supports(com.Workspace) {
		return nil
	}
	gen, started := cache.begin()
	if !started {
		return nil
	}
	ws := com.Workspace
	ctx := com.Context()
	return func() tea.Msg {
		items, err := ops.fetch(ctx, ws)
		if err != nil {
			slog.Error("Failed to list "+ops.label, "error", err)
		}
		return ops.wrap(gen, items, err)
	}
}

// applyListLoaded stores an off-thread fetch result. Runs on the Update
// goroutine. applied reports whether msg was actually written through
// (true) as opposed to discarded for a stale generation or a failure
// (false).
func applyListLoaded[Msg any](cache *ttlCache[[]proto.Thread], com *common.Common, ops listCacheOps[Msg], msg Msg) (cmds []tea.Cmd, applied bool) {
	gen, items, err := ops.unwrap(msg)
	if err != nil {
		if !cache.fail(gen) {
			// Started before a newer state transition; discard and
			// re-dispatch so the authoritative refresh isn't lost.
			if cmd := dispatchListRefresh(cache, com, ops); cmd != nil {
				return []tea.Cmd{cmd}, false
			}
		}
		return nil, false
	}
	if !cache.complete(gen) {
		// This fetch started before a newer state transition (invalidation,
		// event edge). Discard its result and re-dispatch so the
		// authoritative refresh is not lost merely because this older
		// request was in flight.
		if cmd := dispatchListRefresh(cache, com, ops); cmd != nil {
			return []tea.Cmd{cmd}, false
		}
		return nil, false
	}
	cache.set(items)
	return nil, true
}

// applyListEvent reacts to a thread pubsub event belonging to kind: it
// upserts (Created, Updated) or removes (Deleted) the event's row in the
// cached list, then invalidates the TTL so a background refresh eventually
// reconciles with the authoritative list. An event for a different kind is
// ignored — see threadEventMatchesKind — except a Deleted event, which is
// never filtered: dropping a row this cache does not hold is already a
// no-op, and refusing to drop one it somehow does hold would strand it.
func applyListEvent(cache *ttlCache[[]proto.Thread], kind proto.ThreadKind, evt pubsub.Event[proto.Thread]) {
	if evt.Type != pubsub.DeletedEvent && !threadEventMatchesKind(proto.ThreadKind(evt.Payload.Kind), kind) {
		return
	}
	switch evt.Type {
	case pubsub.DeletedEvent:
		for i := range cache.value {
			if cache.value[i].ID == evt.Payload.ID {
				cache.value = append(cache.value[:i], cache.value[i+1:]...)
				break
			}
		}
	default: // CreatedEvent, UpdatedEvent
		found := false
		for i := range cache.value {
			if cache.value[i].ID == evt.Payload.ID {
				cache.value[i] = evt.Payload
				found = true
				break
			}
		}
		if !found {
			cache.value = append(cache.value, evt.Payload)
		}
	}
	cache.invalidate()
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

// staleListRefreshCmd is the TTL backstop: while active and the memoized
// list has outlived ops.ttl, it schedules an off-thread re-probe. It never
// does IO itself.
//
// A fetched-and-empty list stays empty until a matching event invalidates
// it (the timestamp is zeroed then) — don't re-poll forever for a workspace
// with nothing in this particular list.
func staleListRefreshCmd[Msg any](cache *ttlCache[[]proto.Thread], com *common.Common, active bool, ops listCacheOps[Msg]) tea.Cmd {
	if !active || cache.fresh(ops.ttl) || cache.backingOff(ops.backoff) {
		return nil
	}
	if len(cache.value) == 0 && !cache.timestamp.IsZero() {
		return nil
	}
	return dispatchListRefresh(cache, com, ops)
}
