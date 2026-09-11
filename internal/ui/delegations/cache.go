package delegations

// Memoized delegation list state shared by the dashboard and by the
// isolated-delegation dock and header badge. The dashboard reads every kind;
// Threads filters the same cache for the two isolated-only consumers.
//
// ListThreads is treated as IO, so nothing here calls it from Update or View:
// readers use the memoized slice while refresh commands fetch off-thread and
// apply results on the Update goroutine. The generic refresh, event, stale
// generation, and backoff machinery lives in listcache.

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/listcache"
)

// threadsCacheTTL bounds how long the memoized delegation list may go without
// a re-probe being scheduled. Package var so tests can pin it.
var threadsCacheTTL = 5 * time.Second

// ListCache holds the memoized all-delegation list plus its TTL cache and
// in-flight/generation bookkeeping.
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

// Threads returns only isolated delegations for the active-work dock and
// header badge. The dashboard reads Cache.Value directly to show all kinds.
func (c *ListCache) Threads() []proto.Thread {
	threads := make([]proto.Thread, 0, len(c.Cache.Value))
	for _, delegation := range c.Cache.Value {
		if proto.ThreadKind(delegation.Kind) == proto.ThreadKindThread || delegation.Kind == "" {
			threads = append(threads, delegation)
		}
	}
	return threads
}

// LoadedMsg delivers an off-thread all-delegation list result.
type LoadedMsg struct {
	// Gen is the generation captured when the fetch was dispatched. A
	// result whose generation no longer matches ListCache.Cache's
	// generation started before a newer state transition (invalidation,
	// event edge) and is discarded, then re-fetched.
	Gen     uint64
	Threads []proto.Thread
	Err     error
}

// ops supplies the workspace fetch, support gate, and log label to the
// shared cache machinery. Kind is intentionally empty so the dashboard
// receives ordinary tasks and isolated delegations alike.
func (c *ListCache) ops() listcache.Ops[LoadedMsg, threadListWorkspace] {
	return listcache.Ops[LoadedMsg, threadListWorkspace]{
		Label:     "threads",
		TTL:       threadsCacheTTL,
		Backoff:   listcache.RefreshBackoff,
		Kind:      "",
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

// DispatchRefresh returns a command that lists delegations off the Update
// goroutine. It returns nil while a fetch is in flight or the workspace does
// not support delegation listing.
func (c *ListCache) DispatchRefresh(com *common.Common) tea.Cmd {
	if com == nil || com.Workspace == nil {
		return nil
	}
	return listcache.DispatchRefresh(&c.Cache, threadListWorkspace(com.Workspace), com.Context(), c.ops())
}

// ApplyLoaded stores an off-thread fetch result. Runs on the Update
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

// Invalidate marks the cached list stale and bumps the generation so any
// in-flight fetch result is discarded when it lands. Called on delegation
// pubsub events (via ApplyEvent) and by handlers that change delegation state
// out of band.
func (c *ListCache) Invalidate() {
	c.Cache.Invalidate()
}

// ApplyEvent reacts to a delegation event: it upserts (Created, Updated)
// or removes (Deleted) the event's row in the cached list so every
// consumer reflects the change immediately, without waiting for the next
// refresh, then invalidates the TTL so a background refresh eventually
// reconciles with the authoritative list.
func (c *ListCache) ApplyEvent(evt pubsub.Event[proto.Thread]) {
	listcache.ApplyEvent(&c.Cache, "", evt)
}

// StaleRefreshCmd is the TTL backstop: while active and the memoized list
// has outlived its TTL, it schedules an off-thread re-probe. It never does
// IO itself.
func (c *ListCache) StaleRefreshCmd(com *common.Common, active bool) tea.Cmd {
	if com == nil || com.Workspace == nil {
		return nil
	}
	return listcache.StaleRefreshCmd(&c.Cache, threadListWorkspace(com.Workspace), com.Context(), active, c.ops())
}

// ActiveCount reports how many isolated delegations are still active for
// the header badge (see UI.activeThreadBadgeCount).
func ActiveCount(threads []proto.Thread) int {
	n := 0
	for _, t := range threads {
		if proto.ThreadStatus(t.Status).Active() {
			n++
		}
	}
	return n
}
