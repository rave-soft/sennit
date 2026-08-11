package model

// Memoized state for the threads dock: a compact panel (wired in a later
// step) that sits above the chat input and shows a handful of active
// background threads, each with a live one-line status.
//
// Unlike the threads dashboard (threads_cache.go), the dock needs two
// different things at two different costs:
//   - The thread list itself, which is cheap (one ListThreads round trip)
//     and shared with every consumer that wants "all threads" — so this
//     file re-lists rather than trying to share threadsCacheState's
//     memoized slice, mirroring how thread_indicator.go keeps its own copy
//     independent of the dashboard's.
//   - Per-thread live activity (in-progress todo, message count), which
//     requires AttachThread into the thread's own isolated workspace before
//     GetSession can see its session — free in local/in-process mode but an
//     HTTP round trip in client/server mode. Because of that cost this is
//     fetched on its own, longer TTL and only for the small, bounded set of
//     threads the dock actually renders (threadsDockVisibleCap).
//
// Both halves follow the same TTL-cache idiom as threads_cache.go and
// thread_indicator.go: a memoized value, checkedAt/inFlight/gen
// bookkeeping, a dispatchXRefresh that fetches off-thread, an applyXLoaded
// that writes through on the Update goroutine (discarding stale
// generations), and a staleXRefreshCmd TTL backstop. The activity half
// additionally keys inFlight and generation per thread ID, since multiple
// per-thread fetches can be in flight at once and a thread can drop out of
// the visible set independently of the others.

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/thread"
	"github.com/rave-soft/braid/internal/ui/common"
)

// threadsDockTTL bounds how long the memoized thread list may go without a
// re-probe being scheduled. Package var so tests can pin it.
var threadsDockTTL = 5 * time.Second

// threadsDockActivityTTL bounds how long a per-thread activity snapshot may
// go without a re-probe. Longer than threadsDockTTL because refreshing it
// costs an AttachThread round trip, not just a list. Package var so tests
// can pin it.
var threadsDockActivityTTL = 8 * time.Second

// threadsDockVisibleCap is the maximum number of active threads the dock
// renders (and therefore the maximum it ever fetches live activity for).
const threadsDockVisibleCap = 3

// threadDockActivity is a per-thread live snapshot fetched from the
// thread's own session via AttachThread + GetSession.
type threadDockActivity struct {
	// InProgressTodo is the ActiveForm (falling back to Content) of the
	// session's in-progress todo, empty if there is none.
	InProgressTodo string
	MessageCount   int64
	FetchedAt      time.Time
}

// threadsDockState holds the memoized thread list plus per-thread live
// activity (see the package doc comment above) and their independent
// TTL-cache/in-flight/generation bookkeeping.
type threadsDockState struct {
	// threads mirrors the workspace's full thread list, refreshed on
	// threadsDockTTL. The dock filters this down to the active, visible
	// subset itself at render time (activeDockThreads/visibleDockThreads)
	// rather than storing a pre-filtered list, matching the "just relist"
	// idiom used elsewhere in this package.
	threads   []proto.Thread
	checkedAt time.Time
	inFlight  bool
	// gen is bumped by every state transition to the thread list; an
	// in-flight list fetch captures it at dispatch and its result is
	// discarded if the generation has moved on.
	gen uint64

	// activity holds the last known live snapshot per thread ID.
	activity map[string]threadDockActivity
	// activityInFlight guards concurrent per-thread fetches, keyed by
	// thread ID.
	activityInFlight map[string]bool
	// activityGen is bumped whenever threads changes, so a per-thread
	// fetch that started before the thread list moved on (e.g. the thread
	// was removed) is discarded when it lands, mirroring gen but scoped to
	// the activity half.
	activityGen uint64
}

// fresh reports whether the cached thread list is within its TTL.
func (c *threadsDockState) fresh(ttl time.Duration) bool {
	return !c.checkedAt.IsZero() && time.Since(c.checkedAt) < ttl
}

// threadsDockLoadedMsg delivers the result of an off-thread thread list
// fetch for the dock.
type threadsDockLoadedMsg struct {
	// gen is the generation captured when the fetch was dispatched; see
	// threadsDockState.gen.
	gen     uint64
	threads []proto.Thread
}

// dispatchThreadsDockRefresh returns a command that lists threads off the
// Update goroutine, delivering a threadsDockLoadedMsg. It returns nil while
// a fetch is already in flight, or if the workspace doesn't support
// threads.
func (c *threadsDockState) dispatchThreadsDockRefresh(com *common.Common) tea.Cmd {
	if c.inFlight || com == nil || com.Workspace == nil || !com.Workspace.SupportsThreads() {
		return nil
	}
	c.inFlight = true
	ws := com.Workspace
	gen := c.gen
	return func() tea.Msg {
		threads, err := ws.ListThreads(context.Background())
		if err != nil {
			slog.Error("list threads for dock", "error", err)
		}
		return threadsDockLoadedMsg{gen: gen, threads: threads}
	}
}

// applyThreadsDockLoaded stores an off-thread fetch result. Runs on the
// Update goroutine.
func (c *threadsDockState) applyThreadsDockLoaded(com *common.Common, msg threadsDockLoadedMsg) tea.Cmd {
	c.inFlight = false
	if msg.gen != c.gen {
		// Started before a newer state transition; discard and re-dispatch
		// so the authoritative refresh isn't lost.
		return c.dispatchThreadsDockRefresh(com)
	}
	c.threads = msg.threads
	c.checkedAt = time.Now()
	c.activityGen++
	return nil
}

// invalidateThreadsDock marks the cached list stale and bumps the
// generation so any in-flight fetch result is discarded when it lands.
func (c *threadsDockState) invalidateThreadsDock() {
	c.checkedAt = time.Time{}
	c.gen++
}

// applyThreadEvent reacts to a thread pubsub event by invalidating the
// cached list, so the next stale-refresh reconciles with the authoritative
// list. Unlike threads_cache.go's analogous method, this doesn't try to
// upsert the row optimistically — the dock's list is a coarse input to
// filtering/capping logic, not something rendered field-by-field, so a
// short-lived staleness until the next refresh is unremarkable.
func (c *threadsDockState) applyThreadEvent(_ pubsub.Event[proto.Thread]) {
	c.invalidateThreadsDock()
}

// staleThreadsDockRefreshCmd is the TTL backstop for the thread list: while
// active (the caller reports whether the chat screen the dock lives on is
// currently visible) and the memoized list has outlived its TTL, it
// schedules an off-thread re-probe. It never does IO itself.
func (c *threadsDockState) staleThreadsDockRefreshCmd(com *common.Common, active bool) tea.Cmd {
	if !active || c.fresh(threadsDockTTL) {
		return nil
	}
	return c.dispatchThreadsDockRefresh(com)
}

// activeDockThreads filters threads down to the ones worth showing in the
// dock — pending, running, or merging, mirroring activeThreadCount's status
// set — and sorts them stably by CreatedAt ascending so the oldest (first
// started) thread leads, giving deterministic dock ordering.
func activeDockThreads(threads []proto.Thread) []proto.Thread {
	var active []proto.Thread
	for _, t := range threads {
		switch thread.Status(t.Status) {
		case thread.StatusPending, thread.StatusRunning, thread.StatusMerging:
			active = append(active, t)
		}
	}
	sort.SliceStable(active, func(i, j int) bool {
		return active[i].CreatedAt < active[j].CreatedAt
	})
	return active
}

// visibleDockThreads splits active into the threads the dock actually
// renders (capped at threadsDockVisibleCap) and a count of how many more
// active threads exist beyond that cap.
func visibleDockThreads(active []proto.Thread) (visible []proto.Thread, moreCount int) {
	if len(active) <= threadsDockVisibleCap {
		return active, 0
	}
	return active[:threadsDockVisibleCap], len(active) - threadsDockVisibleCap
}

// threadDockActivityLoadedMsg delivers the result of an off-thread
// AttachThread + GetSession fetch for a single thread's live activity.
type threadDockActivityLoadedMsg struct {
	threadID string
	// gen is the activityGen captured when the fetch was dispatched; see
	// threadsDockState.activityGen.
	gen      uint64
	activity threadDockActivity
	err      error
}

// dispatchThreadActivityRefresh returns a command that attaches to
// threadID's own isolated workspace, reads its session, and reduces it to
// a threadDockActivity, delivering a threadDockActivityLoadedMsg. It always
// detaches, even on error, and always delivers a message (with a zero
// activity on failure) so the caller's inFlight flag is cleared rather than
// left stuck. Guards against a nil workspace like the other dispatchers.
func (c *threadsDockState) dispatchThreadActivityRefresh(com *common.Common, threadID, sessionID string) tea.Cmd {
	if com == nil || com.Workspace == nil {
		return nil
	}
	ws := com.Workspace
	gen := c.activityGen
	return func() tea.Msg {
		ctx := context.Background()
		attached, detach, err := ws.AttachThread(ctx, threadID)
		if err != nil {
			slog.Error("attach thread for dock activity", "thread", threadID, "error", err)
			return threadDockActivityLoadedMsg{threadID: threadID, gen: gen, err: err}
		}
		defer detach()

		sess, err := attached.GetSession(ctx, sessionID)
		if err != nil {
			slog.Error("get session for dock activity", "thread", threadID, "error", err)
			return threadDockActivityLoadedMsg{threadID: threadID, gen: gen, err: err}
		}

		activity := threadDockActivity{
			MessageCount: sess.MessageCount,
			FetchedAt:    time.Now(),
		}
		for _, todo := range sess.Todos {
			if todo.Status != session.TodoStatusInProgress {
				continue
			}
			if todo.ActiveForm != "" {
				activity.InProgressTodo = todo.ActiveForm
			} else {
				activity.InProgressTodo = todo.Content
			}
			break
		}
		return threadDockActivityLoadedMsg{threadID: threadID, gen: gen, activity: activity}
	}
}

// applyThreadActivityLoaded writes an off-thread activity fetch result
// through, discarding it if it started before a newer thread-list
// generation (the activityGen check), and always clearing the per-thread
// inFlight flag. Runs on the Update goroutine.
func (c *threadsDockState) applyThreadActivityLoaded(msg threadDockActivityLoadedMsg) {
	if c.activityInFlight != nil {
		delete(c.activityInFlight, msg.threadID)
	}
	if msg.gen != c.activityGen || msg.err != nil {
		return
	}
	if c.activity == nil {
		c.activity = make(map[string]threadDockActivity)
	}
	c.activity[msg.threadID] = msg.activity
}

// staleThreadActivityRefreshCmds schedules off-thread activity refreshes
// for every thread in visible (already capped to threadsDockVisibleCap by
// the caller) that isn't already in flight and whose cached activity is
// missing or has outlived threadsDockActivityTTL. Threads without a
// session yet (SessionID == "") are skipped — there's nothing to attach
// to.
func (c *threadsDockState) staleThreadActivityRefreshCmds(com *common.Common, visible []proto.Thread) []tea.Cmd {
	var cmds []tea.Cmd
	for _, t := range visible {
		if t.SessionID == "" || c.activityInFlight[t.ID] {
			continue
		}
		if activity, ok := c.activity[t.ID]; ok && time.Since(activity.FetchedAt) < threadsDockActivityTTL {
			continue
		}
		cmd := c.dispatchThreadActivityRefresh(com, t.ID, t.SessionID)
		if cmd == nil {
			continue
		}
		if c.activityInFlight == nil {
			c.activityInFlight = make(map[string]bool)
		}
		c.activityInFlight[t.ID] = true
		cmds = append(cmds, cmd)
	}
	return cmds
}

// threadDockGoalFirstLine returns the first line of a thread's goal/prompt
// text, trimmed of surrounding whitespace — the dock shows one line per
// thread and leaves wrapping/truncation for the drawing step.
func threadDockGoalFirstLine(goal string) string {
	line, _, _ := strings.Cut(goal, "\n")
	return strings.TrimSpace(line)
}

// threadDockStatusLine builds the dock's per-thread status text: the
// in-progress todo if there is one, else a step count from the message
// count, else the thread's own status word, always suffixed with the
// elapsed time. Doesn't add the leading "→ " arrow — that's a rendering
// concern for the drawing step to prepend.
func threadDockStatusLine(status thread.Status, activity threadDockActivity, elapsed time.Duration) string {
	var text string
	switch {
	case activity.InProgressTodo != "":
		text = activity.InProgressTodo
	case activity.MessageCount > 0:
		text = fmt.Sprintf("step %d", activity.MessageCount)
	default:
		text = threadDockStatusWord(status)
	}
	return text + " · " + childPanelFormatElapsed(elapsed)
}

// threadDockStatusWord renders a thread's status as the terse, lowercase
// fallback word used when there's no live activity to show instead.
func threadDockStatusWord(status thread.Status) string {
	switch status {
	case thread.StatusPending:
		return "pending"
	case thread.StatusRunning:
		return "running…"
	case thread.StatusMerging:
		return "merging…"
	default:
		return string(status)
	}
}
