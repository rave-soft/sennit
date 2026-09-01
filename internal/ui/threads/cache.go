package threads

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
//     did neither, which is exactly the spin listcache.RefreshBackoff (see
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
// filter, and the LoadedMsg shape.

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/listcache"
)

// threadsCacheTTL bounds how long the memoized thread list may go without a
// re-probe being scheduled. Package var so tests can pin it.
var threadsCacheTTL = 5 * time.Second

// ListCache holds the memoized thread list (see the package doc
// comment above) plus its TTL-cache and in-flight/generation bookkeeping.
type threadListWorkspace interface {
	SupportsThreads() bool
	ListThreads(context.Context) ([]proto.Thread, error)
}

type ListCache struct {
	// Cache is the memoized list and its bookkeeping. It is exported
	// because the screen that owns a ListCache arranges list state
	// through it in tests — seeding a list, starting a generation,
	// asserting nothing is in flight — and there is nothing else in this
	// type to hide behind.
	Cache listcache.TTLCache[[]proto.Thread]
}

// Threads is the memoized list, whatever the last accepted fetch returned.
// It is a read: callers render it, count it, and filter it, and every write
// goes through this package's own apply paths.
func (c *ListCache) Threads() []proto.Thread { return c.Cache.Value }

// LoadedMsg delivers the result of an off-thread thread list fetch.
type LoadedMsg struct {
	// Gen is the generation captured when the fetch was dispatched. A
	// result whose generation no longer matches ListCache.Cache's
	// generation started before a newer state transition (invalidation,
	// event edge) and is discarded, then re-fetched.
	Gen     uint64
	Threads []proto.Thread
	Err     error
}

// ops builds the listCacheOps that plug threads' specifics (fetch call,
// support gate, Kind filter, log label) into the shared machinery in
// list_cache.go. Threads only: tasks share the Kind-discriminated table and
// this list used to merge them in as "live work", but a task is the `agent`
// tool's own delegation — it already renders inline in the chat that
// started it, is never merged, and nothing ever removed a finished one, so
// they only accumulated here, burying the threads this cache is about.
func (c *ListCache) ops() listcache.Ops[LoadedMsg, threadListWorkspace] {
	return listcache.Ops[LoadedMsg, threadListWorkspace]{
		Label:     "threads",
		TTL:       threadsCacheTTL,
		Backoff:   listcache.RefreshBackoff,
		Kind:      proto.ThreadKindThread,
		Available: func(ws threadListWorkspace) bool { return ws != nil },
		Supports:  func(ws threadListWorkspace) bool { return ws.SupportsThreads() },
		Fetch:     func(ctx context.Context, ws threadListWorkspace) ([]proto.Thread, error) { return ws.ListThreads(ctx) },
		Wrap: func(gen uint64, items []proto.Thread, err error) LoadedMsg {
			return LoadedMsg{Gen: gen, Threads: items, Err: err}
		},
		Unwrap: func(msg LoadedMsg) (uint64, []proto.Thread, error) {
			return msg.Gen, msg.Threads, msg.Err
		},
	}
}

// dispatchRefresh returns a command that lists threads off the Update
// goroutine, delivering a LoadedMsg. It returns nil while a fetch is
// already in flight, or if the workspace doesn't support threads.
func (c *ListCache) DispatchRefresh(com *common.Common) tea.Cmd {
	if com == nil || com.Workspace == nil {
		return nil
	}
	return listcache.DispatchRefresh(&c.Cache, threadListWorkspace(com.Workspace), com.Context(), c.ops())
}

// applyLoaded stores an off-thread fetch result. Runs on the Update
// goroutine. applied reports whether msg was actually written through
// (true) as opposed to discarded for a stale generation or a failure
// (false) — callers that need to react only to a genuine change (bumping
// the dock's activityGen, say) check it instead of re-deriving the same
// generation logic themselves.
func (c *ListCache) ApplyLoaded(com *common.Common, msg LoadedMsg) (cmds []tea.Cmd, applied bool) {
	if com == nil || com.Workspace == nil {
		return listcache.ApplyLoaded(&c.Cache, threadListWorkspace(nil), nil, c.ops(), msg)
	}
	return listcache.ApplyLoaded(&c.Cache, threadListWorkspace(com.Workspace), com.Context(), c.ops(), msg)
}

// invalidate marks the cached list stale and bumps the generation so any
// in-flight fetch result is discarded when it lands. Called on thread
// pubsub events (via applyEvent) and by any other handler that changes
// thread state out of band.
func (c *ListCache) Invalidate() {
	c.Cache.Invalidate()
}

// applyEvent reacts to a thread pubsub event: it upserts (Created, Updated)
// or removes (Deleted) the event's row in the cached list so every
// consumer reflects the change immediately, without waiting for the next
// refresh, then invalidates the TTL so a background refresh eventually
// reconciles with the authoritative list.
func (c *ListCache) ApplyEvent(evt pubsub.Event[proto.Thread]) {
	listcache.ApplyEvent(&c.Cache, proto.ThreadKindThread, evt)
}

// staleRefreshCmd is the TTL backstop: while active and the memoized list
// has outlived its TTL, it schedules an off-thread re-probe. It never does
// IO itself.
func (c *ListCache) StaleRefreshCmd(com *common.Common, active bool) tea.Cmd {
	if com == nil || com.Workspace == nil {
		return nil
	}
	return listcache.StaleRefreshCmd(&c.Cache, threadListWorkspace(com.Workspace), com.Context(), active, c.ops())
}

// ActiveCount reports how many of threads are pending, running, or
// merging — the states worth surfacing as "still working" in the header
// badge (see UI.activeThreadBadgeCount).
func ActiveCount(threads []proto.Thread) int {
	n := 0
	for _, t := range threads {
		if proto.ThreadStatus(t.Status).Active() {
			n++
		}
	}
	return n
}
