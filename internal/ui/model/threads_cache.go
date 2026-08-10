package model

// Memoized thread list state.
//
// Threads are parallel agent work streams the workspace runs in isolated
// git worktrees (see internal/workspace/threads.go). Like the workspace
// probes in workspace_cache.go, ListThreads is a synchronous round-trip in
// client/server mode, so the dashboard (added in a later step) must never
// call it from Update or View: it reads the memoized slice and this file
// refreshes it off-thread, applying results back on the Update goroutine.
//
// Follows the same idiom as workspace_cache.go:
//   - threadsCacheState holds the memoized value plus ttlCache bookkeeping,
//     an in-flight guard, and a generation counter.
//   - dispatchThreadsRefresh fetches off-thread and returns a
//     threadsLoadedMsg; it no-ops while a fetch is already in flight.
//   - applyThreadsLoaded writes the result through on the Update goroutine,
//     discarding and re-dispatching stale (gen-mismatched) results.
//   - invalidateThreads and applyThreadEvent react to pubsub.Event[proto.Thread]
//     edges: the latter also upserts/removes the event's row optimistically
//     so the list updates before the next refresh lands.
//   - staleThreadsRefreshCmd is the TTL backstop, only armed while the
//     dashboard is active (passed in by the caller added in a later step).

import (
	"context"
	"log/slog"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/ui/common"
)

// threadsCacheTTL bounds how long the memoized thread list may go without a
// re-probe being scheduled while the dashboard is active. Package var so
// tests can pin it.
var threadsCacheTTL = 5 * time.Second

// threadsCacheState holds the memoized thread list (see the package doc
// comment above) plus its TTL-cache and in-flight/generation bookkeeping.
type threadsCacheState struct {
	// threads mirrors the workspace's thread list. It is event-driven
	// (applyThreadEvent upserts/removes rows optimistically) with a TTL
	// backstop, fetched off-thread by dispatchThreadsRefresh.
	threads []proto.Thread
	// checkedAt is when threads was last confirmed fresh, either by a
	// successful refresh or by write-through from a pubsub event.
	checkedAt time.Time
	inFlight  bool
	// gen is bumped by every state transition (invalidation, event edge);
	// an in-flight fetch captures it at dispatch and its result is
	// discarded if the generation has moved on.
	gen uint64
}

// fresh reports whether the cached thread list is within its TTL.
func (c *threadsCacheState) fresh(ttl time.Duration) bool {
	return !c.checkedAt.IsZero() && time.Since(c.checkedAt) < ttl
}

// threadsLoadedMsg delivers the result of an off-thread thread list fetch.
type threadsLoadedMsg struct {
	// gen is the generation captured when the fetch was dispatched. A
	// result whose generation no longer matches threadsCacheState.gen
	// started before a newer state transition (invalidation, event edge)
	// and is discarded, then re-fetched.
	gen     uint64
	threads []proto.Thread
}

// dispatchThreadsRefresh returns a command that lists threads off the
// Update goroutine, delivering a threadsLoadedMsg. It returns nil while a
// fetch is already in flight, or if the workspace doesn't support threads.
// The closure captures only locals (never the cache or *common.Common) so
// it is safe off-thread; state is applied by applyThreadsLoaded on the
// Update goroutine.
func (c *threadsCacheState) dispatchThreadsRefresh(com *common.Common) tea.Cmd {
	if c.inFlight || com == nil || com.Workspace == nil || !com.Workspace.SupportsThreads() {
		return nil
	}
	c.inFlight = true
	ws := com.Workspace
	gen := c.gen
	return func() tea.Msg {
		threads, err := ws.ListThreads(context.Background())
		if err != nil {
			slog.Error("list threads", "error", err)
		}
		return threadsLoadedMsg{gen: gen, threads: threads}
	}
}

// applyThreadsLoaded stores an off-thread fetch result. Runs on the Update
// goroutine.
func (c *threadsCacheState) applyThreadsLoaded(com *common.Common, msg threadsLoadedMsg) []tea.Cmd {
	c.inFlight = false
	if msg.gen != c.gen {
		// This fetch started before a newer state transition (invalidation,
		// event edge). Discard its result and re-dispatch so the
		// authoritative refresh is not lost merely because this older
		// request was in flight.
		if cmd := c.dispatchThreadsRefresh(com); cmd != nil {
			return []tea.Cmd{cmd}
		}
		return nil
	}
	c.threads = msg.threads
	c.checkedAt = time.Now()
	return nil
}

// invalidateThreads marks the cached list stale and bumps the generation so
// any in-flight fetch result is discarded when it lands. Called on thread
// pubsub events (via applyThreadEvent) and by any other handler that
// changes thread state out of band.
func (c *threadsCacheState) invalidateThreads() {
	c.checkedAt = time.Time{}
	c.gen++
}

// applyThreadEvent reacts to a thread pubsub event: it upserts (Created,
// Updated) or removes (Deleted) the event's row in the cached list so the
// dashboard reflects the change immediately, without waiting for the next
// refresh, then invalidates the TTL so a background refresh eventually
// reconciles with the authoritative list.
func (c *threadsCacheState) applyThreadEvent(evt pubsub.Event[proto.Thread]) {
	switch evt.Type {
	case pubsub.DeletedEvent:
		for i := range c.threads {
			if c.threads[i].ID == evt.Payload.ID {
				c.threads = append(c.threads[:i], c.threads[i+1:]...)
				break
			}
		}
	default: // CreatedEvent, UpdatedEvent
		found := false
		for i := range c.threads {
			if c.threads[i].ID == evt.Payload.ID {
				c.threads[i] = evt.Payload
				found = true
				break
			}
		}
		if !found {
			c.threads = append(c.threads, evt.Payload)
		}
	}
	c.invalidateThreads()
}

// staleThreadsRefreshCmd is the TTL backstop: while active (the threads
// dashboard is showing) and the memoized list has outlived its TTL, it
// schedules an off-thread re-probe. It never does IO itself. A later step
// calls this from the Update tail, mirroring
// UI.staleWorkspaceRefreshCmds.
func (c *threadsCacheState) staleThreadsRefreshCmd(com *common.Common, active bool) tea.Cmd {
	if !active || c.fresh(threadsCacheTTL) {
		return nil
	}
	return c.dispatchThreadsRefresh(com)
}
