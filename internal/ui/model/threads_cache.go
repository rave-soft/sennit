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
//     did neither, which is exactly the spin listRefreshBackoff (see
//     threads_dock.go) exists to prevent — on the dashboard, only sheer
//     luck (Tick was never wired up; see threads.go) kept it from
//     happening. Kept: fail-and-back-off.
//   - Only the dock's and indicator's predecessors stopped polling once a
//     refresh landed empty, until an event invalidated it. Kept: that
//     optimization, folded into staleRefreshCmd below.
//
// The dispatchRefresh/applyLoaded/applyEvent/staleRefreshCmd machinery
// itself now lives in list_cache.go, shared with agentListCache
// (agents_cache.go); this file only supplies what's specific to threads —
// the ListThreads call, the SupportsThreads gate, the ThreadKindThread
// filter, and the threadsLoadedMsg shape.

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/workspace"
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

// ops builds the listCacheOps that plug threads' specifics (fetch call,
// support gate, Kind filter, log label) into the shared machinery in
// list_cache.go. Threads only: tasks share the Kind-discriminated table and
// this list used to merge them in as "live work", but a task is the `agent`
// tool's own delegation — it already renders inline in the chat that
// started it, is never merged, and nothing ever removed a finished one, so
// they only accumulated here, burying the threads this cache is about.
func (c *threadListCache) ops() listCacheOps[threadsLoadedMsg] {
	return listCacheOps[threadsLoadedMsg]{
		label:    "threads",
		ttl:      threadsCacheTTL,
		backoff:  listRefreshBackoff,
		kind:     proto.ThreadKindThread,
		supports: func(ws workspace.Workspace) bool { return ws.SupportsThreads() },
		fetch:    func(ctx context.Context, ws workspace.Workspace) ([]proto.Thread, error) { return ws.ListThreads(ctx) },
		wrap: func(gen uint64, items []proto.Thread, err error) threadsLoadedMsg {
			return threadsLoadedMsg{gen: gen, threads: items, err: err}
		},
		unwrap: func(msg threadsLoadedMsg) (uint64, []proto.Thread, error) {
			return msg.gen, msg.threads, msg.err
		},
	}
}

// dispatchRefresh returns a command that lists threads off the Update
// goroutine, delivering a threadsLoadedMsg. It returns nil while a fetch is
// already in flight, or if the workspace doesn't support threads.
func (c *threadListCache) dispatchRefresh(com *common.Common) tea.Cmd {
	return dispatchListRefresh(&c.cache, com, c.ops())
}

// applyLoaded stores an off-thread fetch result. Runs on the Update
// goroutine. applied reports whether msg was actually written through
// (true) as opposed to discarded for a stale generation or a failure
// (false) — callers that need to react only to a genuine change (bumping
// the dock's activityGen, say) check it instead of re-deriving the same
// generation logic themselves.
func (c *threadListCache) applyLoaded(com *common.Common, msg threadsLoadedMsg) (cmds []tea.Cmd, applied bool) {
	return applyListLoaded(&c.cache, com, c.ops(), msg)
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
	applyListEvent(&c.cache, proto.ThreadKindThread, evt)
}

// staleRefreshCmd is the TTL backstop: while active and the memoized list
// has outlived its TTL, it schedules an off-thread re-probe. It never does
// IO itself.
func (c *threadListCache) staleRefreshCmd(com *common.Common, active bool) tea.Cmd {
	return staleListRefreshCmd(&c.cache, com, active, c.ops())
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
