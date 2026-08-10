package model

// Memoized strand list state.
//
// Strands are parallel agent work streams the workspace runs in isolated
// git worktrees (see internal/workspace/strands.go). Like the workspace
// probes in workspace_cache.go, ListStrands is a synchronous round-trip in
// client/server mode, so the dashboard (added in a later step) must never
// call it from Update or View: it reads the memoized slice and this file
// refreshes it off-thread, applying results back on the Update goroutine.
//
// Follows the same idiom as workspace_cache.go:
//   - strandsCacheState holds the memoized value plus ttlCache bookkeeping,
//     an in-flight guard, and a generation counter.
//   - dispatchStrandsRefresh fetches off-thread and returns a
//     strandsLoadedMsg; it no-ops while a fetch is already in flight.
//   - applyStrandsLoaded writes the result through on the Update goroutine,
//     discarding and re-dispatching stale (gen-mismatched) results.
//   - invalidateStrands and applyStrandEvent react to pubsub.Event[proto.Strand]
//     edges: the latter also upserts/removes the event's row optimistically
//     so the list updates before the next refresh lands.
//   - staleStrandsRefreshCmd is the TTL backstop, only armed while the
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

// strandsCacheTTL bounds how long the memoized strand list may go without a
// re-probe being scheduled while the dashboard is active. Package var so
// tests can pin it.
var strandsCacheTTL = 5 * time.Second

// strandsCacheState holds the memoized strand list (see the package doc
// comment above) plus its TTL-cache and in-flight/generation bookkeeping.
type strandsCacheState struct {
	// strands mirrors the workspace's strand list. It is event-driven
	// (applyStrandEvent upserts/removes rows optimistically) with a TTL
	// backstop, fetched off-thread by dispatchStrandsRefresh.
	strands []proto.Strand
	// checkedAt is when strands was last confirmed fresh, either by a
	// successful refresh or by write-through from a pubsub event.
	checkedAt time.Time
	inFlight  bool
	// gen is bumped by every state transition (invalidation, event edge);
	// an in-flight fetch captures it at dispatch and its result is
	// discarded if the generation has moved on.
	gen uint64
}

// fresh reports whether the cached strand list is within its TTL.
func (c *strandsCacheState) fresh(ttl time.Duration) bool {
	return !c.checkedAt.IsZero() && time.Since(c.checkedAt) < ttl
}

// strandsLoadedMsg delivers the result of an off-thread strand list fetch.
type strandsLoadedMsg struct {
	// gen is the generation captured when the fetch was dispatched. A
	// result whose generation no longer matches strandsCacheState.gen
	// started before a newer state transition (invalidation, event edge)
	// and is discarded, then re-fetched.
	gen     uint64
	strands []proto.Strand
}

// dispatchStrandsRefresh returns a command that lists strands off the
// Update goroutine, delivering a strandsLoadedMsg. It returns nil while a
// fetch is already in flight, or if the workspace doesn't support strands.
// The closure captures only locals (never the cache or *common.Common) so
// it is safe off-thread; state is applied by applyStrandsLoaded on the
// Update goroutine.
func (c *strandsCacheState) dispatchStrandsRefresh(com *common.Common) tea.Cmd {
	if c.inFlight || com == nil || com.Workspace == nil || !com.Workspace.SupportsStrands() {
		return nil
	}
	c.inFlight = true
	ws := com.Workspace
	gen := c.gen
	return func() tea.Msg {
		strands, err := ws.ListStrands(context.Background())
		if err != nil {
			slog.Error("list strands", "error", err)
		}
		return strandsLoadedMsg{gen: gen, strands: strands}
	}
}

// applyStrandsLoaded stores an off-thread fetch result. Runs on the Update
// goroutine.
func (c *strandsCacheState) applyStrandsLoaded(com *common.Common, msg strandsLoadedMsg) []tea.Cmd {
	c.inFlight = false
	if msg.gen != c.gen {
		// This fetch started before a newer state transition (invalidation,
		// event edge). Discard its result and re-dispatch so the
		// authoritative refresh is not lost merely because this older
		// request was in flight.
		if cmd := c.dispatchStrandsRefresh(com); cmd != nil {
			return []tea.Cmd{cmd}
		}
		return nil
	}
	c.strands = msg.strands
	c.checkedAt = time.Now()
	return nil
}

// invalidateStrands marks the cached list stale and bumps the generation so
// any in-flight fetch result is discarded when it lands. Called on strand
// pubsub events (via applyStrandEvent) and by any other handler that
// changes strand state out of band.
func (c *strandsCacheState) invalidateStrands() {
	c.checkedAt = time.Time{}
	c.gen++
}

// applyStrandEvent reacts to a strand pubsub event: it upserts (Created,
// Updated) or removes (Deleted) the event's row in the cached list so the
// dashboard reflects the change immediately, without waiting for the next
// refresh, then invalidates the TTL so a background refresh eventually
// reconciles with the authoritative list.
func (c *strandsCacheState) applyStrandEvent(evt pubsub.Event[proto.Strand]) {
	switch evt.Type {
	case pubsub.DeletedEvent:
		for i := range c.strands {
			if c.strands[i].ID == evt.Payload.ID {
				c.strands = append(c.strands[:i], c.strands[i+1:]...)
				break
			}
		}
	default: // CreatedEvent, UpdatedEvent
		found := false
		for i := range c.strands {
			if c.strands[i].ID == evt.Payload.ID {
				c.strands[i] = evt.Payload
				found = true
				break
			}
		}
		if !found {
			c.strands = append(c.strands, evt.Payload)
		}
	}
	c.invalidateStrands()
}

// staleStrandsRefreshCmd is the TTL backstop: while active (the strands
// dashboard is showing) and the memoized list has outlived its TTL, it
// schedules an off-thread re-probe. It never does IO itself. A later step
// calls this from the Update tail, mirroring
// UI.staleWorkspaceRefreshCmds.
func (c *strandsCacheState) staleStrandsRefreshCmd(com *common.Common, active bool) tea.Cmd {
	if !active || c.fresh(strandsCacheTTL) {
		return nil
	}
	return c.dispatchStrandsRefresh(com)
}
