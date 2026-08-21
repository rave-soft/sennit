package model

// Memoized thread list state, shared by every consumer that needs "all
// threads": the threads dashboard (threads.go), the session panel's threads
// dock (threads_dock.go), and the header's active-thread badge
// (activeThreadBadgeCount in ui.go).
//
// Threads are parallel agent work streams the workspace runs in isolated
// git worktrees (see internal/workspace/threads.go). Like the workspace
// probes in workspace_cache.go, ListThreads is treated as IO, so nothing
// here may call it from Update or View: readers use the memoized slice and
// this file refreshes it off-thread, applying results back on the Update
// goroutine.
//
// This used to be three near-identical caches, one per consumer, each
// dispatching its own ListThreads round trip on the same pubsub thread
// event — three RPCs for one edge. They also disagreed in ways that were
// bugs, not intentional divergence:
//   - Only this cache's predecessor filtered a pubsub event by Kind before
//     applying it; the dock's and indicator's applied (and invalidated on)
//     every event, including task events that could never appear in a
//     threads-only list. Kept: the Kind filter, since it is strictly more
//     correct and avoids pointless re-fetches on task churn.
//   - Only the dock's and indicator's predecessors called ttlCache.fail on
//     an error, so a failing refresh backed off. This cache's predecessor
//     did neither, which is exactly the spin threadsRefreshBackoff (see
//     threads_dock.go) exists to prevent — on the dashboard, only sheer
//     luck (Tick was never wired up; see threads.go) kept it from
//     happening. Kept: fail-and-back-off.
//   - Only the dock's and indicator's predecessors stopped polling once a
//     refresh landed empty, until an event invalidated it. Kept: that
//     optimization, folded into staleRefreshCmd below.
//
// Follows the ttlCache idiom used throughout this package (see
// ttl_cache.go and workspace_cache.go):
//   - threadListCache holds the memoized value plus ttlCache bookkeeping.
//   - dispatchRefresh fetches off-thread and returns a threadsLoadedMsg; it
//     no-ops while a fetch is already in flight.
//   - applyLoaded writes the result through on the Update goroutine,
//     discarding and re-dispatching stale (gen-mismatched) results, and
//     backing off after a failure.
//   - invalidate and applyEvent react to pubsub.Event[proto.Thread] edges:
//     the latter also upserts/removes the event's row optimistically so
//     every consumer's view updates before the next refresh lands.
//   - staleRefreshCmd is the TTL backstop, called unconditionally whenever
//     any consumer needs the list current (the header badge always does;
//     see UI.threadViewsRefreshCmds).

import (
	"log/slog"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/ui/common"
)

// threadsCacheTTL bounds how long the memoized thread list may go without a
// re-probe being scheduled. Package var so tests can pin it.
var threadsCacheTTL = 5 * time.Second

// threadListCache holds the memoized thread list (see the package doc
// comment above) plus its TTL-cache and in-flight/generation bookkeeping.
type threadListCache struct {
	cache ttlCache[[]proto.Thread]
}

// threadsLoadedMsg delivers the result of an off-thread thread list fetch.
type threadsLoadedMsg struct {
	// gen is the generation captured when the fetch was dispatched. A
	// result whose generation no longer matches threadListCache.cache's
	// generation started before a newer state transition (invalidation,
	// event edge) and is discarded, then re-fetched.
	gen     uint64
	threads []proto.Thread
	err     error
}

// dispatchRefresh returns a command that lists threads off the Update
// goroutine, delivering a threadsLoadedMsg. It returns nil while a fetch is
// already in flight, or if the workspace doesn't support threads. The
// closure captures only locals (never the cache or *common.Common) so it is
// safe off-thread; state is applied by applyLoaded on the Update goroutine.
func (c *threadListCache) dispatchRefresh(com *common.Common) tea.Cmd {
	if c.cache.inFlight || com == nil || com.Workspace == nil || !com.Workspace.SupportsThreads() {
		return nil
	}
	gen, started := c.cache.begin()
	if !started {
		return nil
	}
	ws := com.Workspace
	ctx := com.Context()
	return func() tea.Msg {
		// Threads only. Tasks share the Kind-discriminated table and this
		// list used to merge them in as "live work", but a task is the
		// `agent` tool's own delegation: it already renders inline in the
		// chat that started it, it is never merged, and nothing ever
		// removed a finished one — so they only accumulated here, burying
		// the threads this cache is about.
		threads, err := ws.ListThreads(ctx)
		if err != nil {
			slog.Error("Failed to list threads", "error", err)
		}
		return threadsLoadedMsg{gen: gen, threads: threads, err: err}
	}
}

// applyLoaded stores an off-thread fetch result. Runs on the Update
// goroutine. applied reports whether msg was actually written through
// (true) as opposed to discarded for a stale generation or a failure
// (false) — callers that need to react only to a genuine change (bumping
// the dock's activityGen, say) check it instead of re-deriving the same
// generation logic themselves.
func (c *threadListCache) applyLoaded(com *common.Common, msg threadsLoadedMsg) (cmds []tea.Cmd, applied bool) {
	if msg.err != nil {
		if !c.cache.fail(msg.gen) {
			// Started before a newer state transition; discard and
			// re-dispatch so the authoritative refresh isn't lost.
			if cmd := c.dispatchRefresh(com); cmd != nil {
				return []tea.Cmd{cmd}, false
			}
		}
		return nil, false
	}
	if !c.cache.complete(msg.gen) {
		// This fetch started before a newer state transition (invalidation,
		// event edge). Discard its result and re-dispatch so the
		// authoritative refresh is not lost merely because this older
		// request was in flight.
		if cmd := c.dispatchRefresh(com); cmd != nil {
			return []tea.Cmd{cmd}, false
		}
		return nil, false
	}
	c.cache.set(msg.threads)
	return nil, true
}

// invalidate marks the cached list stale and bumps the generation so any
// in-flight fetch result is discarded when it lands. Called on thread
// pubsub events (via applyEvent) and by any other handler that changes
// thread state out of band.
func (c *threadListCache) invalidate() {
	c.cache.invalidate()
}

// applyEvent reacts to a thread pubsub event: it upserts (Created, Updated)
// or removes (Deleted) the event's row in the cached list so every
// consumer reflects the change immediately, without waiting for the next
// refresh, then invalidates the TTL so a background refresh eventually
// reconciles with the authoritative list.
func (c *threadListCache) applyEvent(evt pubsub.Event[proto.Thread]) {
	// Threads only, matching the list this cache is refreshed from. Tasks
	// share the delegations table and the lifecycle that publishes these
	// events, so without this a task's own create/status event wrote a row
	// straight into the cache — around the kind-scoped query, and staying
	// until a full refresh replaced the slice. Filtering the fetch alone
	// was not enough.
	//
	// An empty kind reads as a thread: it is an additive field (see
	// proto.Thread.Kind), and a payload that predates it describes a
	// thread. A removal is not filtered — dropping a row this cache does
	// not hold is already a no-op, and refusing to drop one it somehow
	// does hold would strand it.
	if evt.Type != pubsub.DeletedEvent &&
		evt.Payload.Kind != "" &&
		proto.ThreadKind(evt.Payload.Kind) != proto.ThreadKindThread {
		return
	}
	switch evt.Type {
	case pubsub.DeletedEvent:
		for i := range c.cache.value {
			if c.cache.value[i].ID == evt.Payload.ID {
				c.cache.value = append(c.cache.value[:i], c.cache.value[i+1:]...)
				break
			}
		}
	default: // CreatedEvent, UpdatedEvent
		found := false
		for i := range c.cache.value {
			if c.cache.value[i].ID == evt.Payload.ID {
				c.cache.value[i] = evt.Payload
				found = true
				break
			}
		}
		if !found {
			c.cache.value = append(c.cache.value, evt.Payload)
		}
	}
	c.invalidate()
}

// staleRefreshCmd is the TTL backstop: while active and the memoized list
// has outlived its TTL, it schedules an off-thread re-probe. It never does
// IO itself.
func (c *threadListCache) staleRefreshCmd(com *common.Common, active bool) tea.Cmd {
	if !active || c.cache.fresh(threadsCacheTTL) || c.cache.backingOff(threadsRefreshBackoff) {
		return nil
	}
	// A fetched-and-empty list stays empty until a thread event invalidates
	// it (the timestamp is zeroed then) — don't re-poll ListThreads forever
	// for projects that have no threads at all.
	if len(c.cache.value) == 0 && !c.cache.timestamp.IsZero() {
		return nil
	}
	return c.dispatchRefresh(com)
}

// activeThreadCount reports how many of threads are pending, running, or
// merging — the states worth surfacing as "still working" in the header
// badge (see UI.activeThreadBadgeCount).
func activeThreadCount(threads []proto.Thread) int {
	n := 0
	for _, t := range threads {
		if proto.ThreadStatus(t.Status).Active() {
			n++
		}
	}
	return n
}
