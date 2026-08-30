package model

// Memoized state for the threads dock: a compact panel that sits above the
// chat input and shows a handful of active background threads, each with a
// live one-line status.
//
// The thread list itself lives in threads.ListCache (threads_cache.go),
// shared with the dashboard and the header badge — one ListThreads round
// trip serves every consumer. What's specific to the dock, and stays here,
// is per-thread live activity (in-progress todo, message count): it
// requires AttachThread into the thread's own isolated workspace before
// GetSession can see its session — cheap for a live thread, but a completed
// one must first be reactivated (respawning its worktree and process; see
// AttachThread). Because of that cost this is fetched on its own, longer
// TTL and only for the threads the dock actually renders.
//
// Follows the same TTL-cache idiom as threads_cache.go: a memoized value,
// checkedAt/inFlight/gen bookkeeping, a dispatchXRefresh that fetches
// off-thread, an applyXLoaded that writes through on the Update goroutine
// (discarding stale generations), and a staleXRefreshCmd TTL backstop. It
// additionally keys inFlight and generation per thread ID, since multiple
// per-thread fetches can be in flight at once and a thread can drop out of
// the visible set independently of the others.

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/chat"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/listcache"
	"github.com/rave-soft/sennit/internal/ui/presentation"
	"github.com/rave-soft/sennit/internal/workspace"
)

// threadsDockActivityTTL bounds how long a per-thread activity snapshot may
// go without a re-probe. Longer than threadsCacheTTL because refreshing it
// costs an AttachThread round trip, not just a list. Package var so tests
// can pin it.
var threadsDockActivityTTL = 8 * time.Second

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

// threadsDockState holds the dock's per-thread live activity (see the
// package doc comment above) and its independent TTL-cache/in-flight/
// generation bookkeeping.
type threadsDockState struct {
	// activity holds the last known live snapshot per thread ID.
	activity map[string]listcache.TTLCache[threadDockActivity]
	// activityGen is bumped whenever the shared thread list changes, so a
	// per-thread fetch that started before the thread list moved on (e.g.
	// the thread was removed) is discarded when it lands, mirroring gen but
	// scoped to the activity half. Bumped by UI.updateThreads on every
	// applied threads.LoadedMsg (see threads.ListCache.applyLoaded's applied
	// return).
	activityGen uint64
}

// dropActivity discards a thread's cached live activity, leaving the shared
// thread list alone: the caller (UI.updateThreads, on a Deleted event) is
// responsible for that. A stale activity snapshot for a thread that no
// longer exists is otherwise silently harmless — nothing reads it once the
// thread is gone from the shared list — but there is no reason to keep it
// or let its next TTL tick attach to a thread that isn't there.
func (c *threadsDockState) dropActivity(id string) {
	delete(c.activity, id)
}

// activeDockThreads filters threads down to the ones worth showing in the
// dock as live work: pending, running, or merging (mirroring
// threads.ActiveCount's status set), plus idle. Idle is deliberately included
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
		if proto.ThreadStatus(t.Status).Active() || proto.ThreadStatus(t.Status) == proto.ThreadStatusIdle {
			active = append(active, t)
		}
	}
	sortThreadsByCreation(active)
	return active
}

// sortThreadsByCreation orders delegations oldest-first, in place and
// stably, so the first one started leads and the order does not shuffle
// under a refresh. Shared with the panel's agents section (see
// sessionDelegations), which wants the same guarantee for the same reason.
func sortThreadsByCreation(items []proto.Thread) {
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].CreatedAt < items[j].CreatedAt
	})
}

// threadDockActivityLoadedMsg delivers the result of an off-thread
// AttachThread + GetSession fetch for a single thread's live activity.
type threadDockActivityLoadedMsg struct {
	mainScreenOwned
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
		c.activity = make(map[string]listcache.TTLCache[threadDockActivity])
	}
	entry := c.activity[threadID]
	entryGen, started := entry.Begin()
	if !started {
		return nil
	}
	c.activity[threadID] = entry
	prev, hasPrev := entry.Value, !entry.Timestamp.IsZero()
	ctx := com.Context()
	return func() tea.Msg {
		attached, detach, err := ws.AttachThread(ctx, threadID)
		if err != nil {
			slog.Error("Failed to attach thread for dock activity", "thread", threadID, "error", err)
			return threadDockActivityLoadedMsg{threadID: threadID, gen: gen, entryGen: entryGen, err: err}
		}
		defer detach()

		sess, err := attached.GetSession(ctx, sessionID)
		if err != nil {
			slog.Error("Failed to get session for dock activity", "thread", threadID, "error", err)
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
			slog.Error("Failed to list messages for dock activity", "thread", threadID, "error", err)
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
	if msg.err != nil {
		// Record the failure so the next Update backs off instead of
		// re-dispatching immediately; see listcache.RefreshBackoff.
		entry.Fail(msg.entryGen)
		c.activity[msg.threadID] = entry
		return
	}
	matchingEntry := entry.Complete(msg.entryGen)
	c.activity[msg.threadID] = entry
	if !matchingEntry || msg.gen != c.activityGen {
		return
	}
	entry.Set(msg.activity)
	c.activity[msg.threadID] = entry
}

// staleThreadActivityRefreshCmds schedules off-thread activity refreshes
// for every thread in visible that isn't already in flight and whose
// cached activity is missing or has outlived threadsDockActivityTTL.
// Threads without a session yet (SessionID == "") are skipped — there's
// nothing to attach to.
func (c *threadsDockState) staleThreadActivityRefreshCmds(com *common.Common, visible []proto.Thread) []tea.Cmd {
	// Activity needs AttachThread, which a read-only workspace refuses
	// unconditionally — that happens whenever the user is inside a thread
	// that could not be reactivated. Backing off would already stop the
	// spin this used to cause, but there is no reason to keep asking a
	// workspace that can only ever say no.
	if com == nil || com.Workspace == nil || !workspace.SupportsThreadAttach(com.Workspace) {
		return nil
	}

	var cmds []tea.Cmd
	for _, t := range visible {
		if t.SessionID == "" {
			continue
		}
		if c.activity == nil {
			c.activity = make(map[string]listcache.TTLCache[threadDockActivity])
		}
		activity := c.activity[t.ID]
		if activity.InFlight || activity.Fresh(threadsDockActivityTTL) || activity.BackingOff(listcache.RefreshBackoff) {
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
func threadDockStatusLine(status proto.ThreadStatus, activity threadDockActivity, elapsed time.Duration) string {
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
func threadDockStatusWord(status proto.ThreadStatus) string {
	switch status {
	case proto.ThreadStatusPending:
		return "pending"
	case proto.ThreadStatusRunning:
		return "running…"
	case proto.ThreadStatusMerging:
		return "merging…"
	case proto.ThreadStatusIdle:
		// Explicit, not the raw-status default: idle must read as its own
		// waiting state, distinct from both "running" and a terminal word.
		return "idle"
	default:
		return string(status)
	}
}
