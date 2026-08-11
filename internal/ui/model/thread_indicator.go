package model

// Active-thread count for the header badge (see header.go's
// renderHeaderDetails). This is deliberately independent of the threads
// dashboard's own cache (threads_cache.go): the badge must stay current
// whether or not the dashboard has ever been opened (it's lazily created,
// see root.go), and it lives on *UI* rather than *Root* so the main screen
// can render it without reaching into the dashboard.
//
// Follows the same TTL-cache idiom as workspace_cache.go and
// threads_cache.go: dispatchThreadIndicatorRefresh fetches off-thread,
// applyThreadIndicatorLoaded writes the result through on the Update
// goroutine (discarding stale generations), and a pubsub thread event
// invalidates the cache so the TTL backstop reconciles promptly.

import (
	"context"
	"log/slog"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/thread"
	"github.com/rave-soft/braid/internal/ui/common"
)

// threadIndicatorTTL bounds how long the memoized active-thread count may
// go without a re-probe being scheduled. Package var so tests can pin it.
var threadIndicatorTTL = 5 * time.Second

// threadIndicatorState holds the memoized active-thread count plus its
// TTL-cache and in-flight/generation bookkeeping.
type threadIndicatorState struct {
	count     int
	checkedAt time.Time
	inFlight  bool
	gen       uint64
}

// threadIndicatorLoadedMsg delivers the result of an off-thread thread list
// fetch made for the indicator badge.
type threadIndicatorLoadedMsg struct {
	gen   uint64
	count int
}

// fresh reports whether the cached count is within its TTL.
func (c *threadIndicatorState) fresh(ttl time.Duration) bool {
	return !c.checkedAt.IsZero() && time.Since(c.checkedAt) < ttl
}

// activeThreadCount reports how many of threads are pending, running, or
// merging — the states worth surfacing as "still working" in a badge.
func activeThreadCount(threads []proto.Thread) int {
	n := 0
	for _, t := range threads {
		if thread.Status(t.Status).Active() {
			n++
		}
	}
	return n
}

// dispatchRefresh returns a command that lists threads off the Update
// goroutine and reduces them to a count, delivering a
// threadIndicatorLoadedMsg. Returns nil while a fetch is already in
// flight, or if the workspace doesn't support threads.
func (c *threadIndicatorState) dispatchRefresh(com *common.Common) tea.Cmd {
	if c.inFlight || com == nil || com.Workspace == nil || !com.Workspace.SupportsThreads() {
		return nil
	}
	c.inFlight = true
	ws := com.Workspace
	gen := c.gen
	return func() tea.Msg {
		threads, err := ws.ListThreads(context.Background())
		if err != nil {
			slog.Error("list threads for indicator", "error", err)
		}
		return threadIndicatorLoadedMsg{gen: gen, count: activeThreadCount(threads)}
	}
}

// applyLoaded stores an off-thread fetch result. Runs on the Update
// goroutine.
func (c *threadIndicatorState) applyLoaded(com *common.Common, msg threadIndicatorLoadedMsg) tea.Cmd {
	c.inFlight = false
	if msg.gen != c.gen {
		return c.dispatchRefresh(com)
	}
	c.count = msg.count
	c.checkedAt = time.Now()
	return nil
}

// invalidate marks the cached count stale and bumps the generation, so an
// in-flight fetch's result is discarded when it lands.
func (c *threadIndicatorState) invalidate() {
	c.checkedAt = time.Time{}
	c.gen++
}

// applyEvent reacts to a thread pubsub event: it invalidates the cache so
// the next stale-refresh reflects the change. It doesn't try to reason
// about the event's effect on the count optimistically — a full re-list is
// cheap and avoids drift between this and the dashboard's own count.
func (c *threadIndicatorState) applyEvent(_ pubsub.Event[proto.Thread]) {
	c.invalidate()
}

// staleRefreshCmd is the TTL backstop: schedules an off-thread re-probe
// when the memoized count has outlived its TTL.
func (c *threadIndicatorState) staleRefreshCmd(com *common.Common) tea.Cmd {
	if c.fresh(threadIndicatorTTL) {
		return nil
	}
	// A fetched zero count stays zero until a thread event invalidates it
	// (checkedAt is zeroed then) — don't re-poll ListThreads forever for
	// projects that have no active threads.
	if c.count == 0 && !c.checkedAt.IsZero() {
		return nil
	}
	return c.dispatchRefresh(com)
}
