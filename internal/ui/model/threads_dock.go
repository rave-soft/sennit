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

	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/thread"
	"github.com/rave-soft/braid/internal/ui/chat"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/presentation"
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
	// LastTool is a compact summary of the session's most recent tool call
	// ("Read internal/foo.go", "bash go test ./..."), empty when the
	// session has no tool calls yet. It answers "what is this thread doing
	// right now" at a finer grain than the in-progress todo.
	LastTool     string
	MessageCount int64
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
	cache ttlCache[[]proto.Thread]

	// activity holds the last known live snapshot per thread ID.
	activity map[string]ttlCache[threadDockActivity]
	// activityGen is bumped whenever threads changes, so a per-thread
	// fetch that started before the thread list moved on (e.g. the thread
	// was removed) is discarded when it lands, mirroring gen but scoped to
	// the activity half.
	activityGen uint64
}

// fresh reports whether the cached thread list is within its TTL.
// threadsDockLoadedMsg delivers the result of an off-thread thread list
// fetch for the dock.
type threadsDockLoadedMsg struct {
	// gen is the generation captured when the fetch was dispatched; see
	// threadsDockState.gen.
	gen     uint64
	threads []proto.Thread
	err     error
}

// dispatchThreadsDockRefresh returns a command that lists threads off the
// Update goroutine, delivering a threadsDockLoadedMsg. It returns nil while
// a fetch is already in flight, or if the workspace doesn't support
// threads.
func (c *threadsDockState) dispatchThreadsDockRefresh(com *common.Common) tea.Cmd {
	if c.cache.inFlight || com == nil || com.Workspace == nil || !com.Workspace.SupportsThreads() {
		return nil
	}
	gen, started := c.cache.begin()
	if !started {
		return nil
	}
	ws := com.Workspace
	return func() tea.Msg {
		threads, err := ws.ListThreads(context.Background())
		if err != nil {
			slog.Error("list threads for dock", "error", err)
		}
		// Merge in task rows the same way threads_cache.go does: a
		// ListTasks failure is logged and just leaves task rows out of
		// this refresh, it never discards an otherwise-successful thread
		// list (applyThreadsDockLoaded only caches on err == nil).
		if ws.SupportsTasks() {
			tasks, taskErr := ws.ListTasks(context.Background())
			if taskErr != nil {
				slog.Error("list tasks for dock", "error", taskErr)
			} else {
				threads = append(threads, tasks...)
			}
		}
		return threadsDockLoadedMsg{gen: gen, threads: threads, err: err}
	}
}

// applyThreadsDockLoaded stores an off-thread fetch result. Runs on the
// Update goroutine.
func (c *threadsDockState) applyThreadsDockLoaded(com *common.Common, msg threadsDockLoadedMsg) tea.Cmd {
	if !c.cache.complete(msg.gen) {
		// Started before a newer state transition; discard and re-dispatch
		// so the authoritative refresh isn't lost.
		return c.dispatchThreadsDockRefresh(com)
	}
	if msg.err == nil {
		c.cache.set(msg.threads)
		c.activityGen++
	}
	return nil
}

// invalidateThreadsDock marks the cached list stale and bumps the
// generation so any in-flight fetch result is discarded when it lands.
func (c *threadsDockState) invalidateThreadsDock() {
	c.cache.invalidate()
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
	if !active || c.cache.fresh(threadsDockTTL) {
		return nil
	}
	// A fetched-and-empty list stays empty until a thread event
	// invalidates it (checkedAt is zeroed then) — don't re-poll
	// ListThreads forever for projects that have no threads at all.
	if len(c.cache.value) == 0 && !c.cache.timestamp.IsZero() {
		return nil
	}
	return c.dispatchThreadsDockRefresh(com)
}

// activeDockThreads filters threads down to the ones worth showing in the
// dock as live work: pending, running, or merging (mirroring
// activeThreadCount's status set), plus idle. Idle is deliberately included
// here even though Status.Active() excludes it (see thread/types.go's
// StatusIdle doc comment): an idle delegation's workspace is still live and
// worth surfacing, it just has no run in flight right now — "idle must not
// read as finished". Rendering (threadDockStatusWord, the panel's line2
// icon) is responsible for keeping idle visually distinct from
// running/merging and from a terminal status. Results are sorted stably by
// CreatedAt ascending so the oldest (first started) thread leads, giving
// deterministic dock ordering.
func activeDockThreads(threads []proto.Thread) []proto.Thread {
	var active []proto.Thread
	for _, t := range threads {
		if thread.Status(t.Status).Active() || thread.Status(t.Status) == thread.StatusIdle {
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
	entryGen uint64
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
	if c.activity == nil {
		c.activity = make(map[string]ttlCache[threadDockActivity])
	}
	entry := c.activity[threadID]
	entryGen, started := entry.begin()
	if !started {
		return nil
	}
	c.activity[threadID] = entry
	prev, hasPrev := entry.value, !entry.timestamp.IsZero()
	return func() tea.Msg {
		ctx := context.Background()
		attached, detach, err := ws.AttachThread(ctx, threadID)
		if err != nil {
			slog.Error("attach thread for dock activity", "thread", threadID, "error", err)
			return threadDockActivityLoadedMsg{threadID: threadID, gen: gen, entryGen: entryGen, err: err}
		}
		defer detach()

		sess, err := attached.GetSession(ctx, sessionID)
		if err != nil {
			slog.Error("get session for dock activity", "thread", threadID, "error", err)
			return threadDockActivityLoadedMsg{threadID: threadID, gen: gen, entryGen: entryGen, err: err}
		}

		activity := threadDockActivity{MessageCount: sess.MessageCount}
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

		// The last tool call is not on the session row — it takes listing
		// the session's messages, whose cost grows with the session's
		// whole history (and, in local mode, forces a FlushAll first). So
		// only re-list when the message count moved since the previous
		// probe: an unchanged count means no new tool call, and the cached
		// summary is still right. A listing failure only costs this one
		// optional field, so it's a best-effort add-on rather than a
		// reason to fail the whole probe.
		if hasPrev && prev.MessageCount == sess.MessageCount {
			activity.LastTool = prev.LastTool
		} else if msgs, err := attached.ListMessages(ctx, sessionID); err != nil {
			slog.Error("list messages for dock activity", "thread", threadID, "error", err)
		} else {
			activity.LastTool = lastToolSummary(msgs)
		}
		return threadDockActivityLoadedMsg{threadID: threadID, gen: gen, entryGen: entryGen, activity: activity}
	}
}

// applyThreadActivityLoaded writes an off-thread activity fetch result
// through, discarding it if it started before a newer thread-list
// generation (the activityGen check), and always clearing the per-thread
// inFlight flag. Runs on the Update goroutine.
func (c *threadsDockState) applyThreadActivityLoaded(msg threadDockActivityLoadedMsg) {
	entry, ok := c.activity[msg.threadID]
	if !ok {
		return
	}
	matchingEntry := entry.complete(msg.entryGen)
	c.activity[msg.threadID] = entry
	if !matchingEntry || msg.gen != c.activityGen || msg.err != nil {
		return
	}
	entry.set(msg.activity)
	c.activity[msg.threadID] = entry
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
		if t.SessionID == "" {
			continue
		}
		if c.activity == nil {
			c.activity = make(map[string]ttlCache[threadDockActivity])
		}
		activity := c.activity[t.ID]
		if activity.inFlight || activity.fresh(threadsDockActivityTTL) {
			continue
		}
		if cmd := c.dispatchThreadActivityRefresh(com, t.ID, t.SessionID); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return cmds
}

// lastToolSummary reduces a session's message history to a compact
// summary of its most recent tool call (chat.LastToolSummary's "name +
// key argument" shape), scanning from the end since only the newest call
// matters. "" when no message has any tool calls yet.
func lastToolSummary(msgs []message.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		calls := msgs[i].ToolCalls()
		if len(calls) == 0 {
			continue
		}
		return chat.LastToolSummary(calls[len(calls)-1])
	}
	return ""
}

// threadDockGoalFirstLine returns the first line of a thread's goal/prompt
// text, trimmed of surrounding whitespace — the dock shows one line per
// thread and leaves wrapping/truncation for the drawing step.
func threadDockGoalFirstLine(goal string) string {
	line, _, _ := strings.Cut(goal, "\n")
	return strings.TrimSpace(line)
}

// threadDockStatusLine builds the dock's per-thread status text: the step
// count plus what the thread is doing right now — its in-progress todo
// when there is one (a todo says more about intent than a raw tool name,
// matching renderPanelStatusLine's priority), else its last tool call —
// falling back to the thread's own status word when there's no activity
// at all, always suffixed with the elapsed time. Doesn't add the leading
// spinner/arrow — that's a rendering concern for the drawing step to
// prepend.
func threadDockStatusLine(status thread.Status, activity threadDockActivity, elapsed time.Duration) string {
	var parts []string
	if activity.MessageCount > 0 {
		parts = append(parts, fmt.Sprintf("step %d", activity.MessageCount))
	}
	switch {
	case activity.InProgressTodo != "":
		parts = append(parts, "→ "+activity.InProgressTodo)
	case activity.LastTool != "":
		parts = append(parts, "→ "+activity.LastTool)
	}
	if len(parts) == 0 {
		parts = append(parts, threadDockStatusWord(status))
	}
	parts = append(parts, presentation.FormatElapsed(elapsed))
	return presentation.JoinStatusParts(parts, -1)
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
	case thread.StatusIdle:
		// Explicit, not the raw-status default: idle must read as its own
		// waiting state, distinct from both "running" and a terminal word.
		return "idle"
	default:
		return string(status)
	}
}
