package model

// Memoized list of this workspace's delegations — the tasks the `agent`
// tool, the custom agent tools and `agentic_fetch` start (internal/thread's
// KindTask). It feeds the session panel's "agents" section, the live view of
// what the current session has delegated and where it has got to.
//
// A delegation used to be visible without any of this: the tool call blocked
// until the sub-agent answered, so the transcript's own stub was a live
// thing, and the panel's old delegations section read it straight out of the
// chat items. Delegation is asynchronous now — the call returns an
// acknowledgement the moment the task is created, and the stub in the
// transcript is finished from that instant on — so liveness has to come from
// the task record itself, which is what this cache holds.
//
// Deliberately separate from threadListCache rather than folded into it:
// that cache is Kind-scoped to threads on both its fetch and its event path,
// for good reasons documented there (a task has no worktree, is never
// merged, and used to accumulate in the list and bury the threads). This is
// the mirror image of it, scoped the other way, sharing the same ttlCache
// idiom — dispatchRefresh off-thread, applyLoaded on the Update goroutine,
// applyEvent for the pubsub edge, staleRefreshCmd as the TTL backstop.

import (
	"log/slog"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/ui/common"
)

// agentsCacheTTL bounds how long the memoized task list may go without a
// re-probe being scheduled. Shorter than threadsCacheTTL: a delegation is
// usually a matter of seconds to a couple of minutes, and unlike a thread it
// has no per-entity activity probe behind it, so this list is the only thing
// that ever moves its block. Package var so tests can pin it.
var agentsCacheTTL = 3 * time.Second

// agentListCache holds the memoized delegation list plus its TTL-cache and
// in-flight/generation bookkeeping.
type agentListCache struct {
	cache ttlCache[[]proto.Thread]
}

// agentsLoadedMsg delivers the result of an off-thread task list fetch.
type agentsLoadedMsg struct {
	// gen is the generation captured when the fetch was dispatched; a
	// result that no longer matches began before a newer state transition
	// and is discarded, then re-fetched.
	gen    uint64
	agents []proto.Thread
	err    error
}

// dispatchRefresh returns a command that lists delegations off the Update
// goroutine, delivering an agentsLoadedMsg. It returns nil while a fetch is
// already in flight, or if the workspace has no task manager (see
// AppWorkspace.SupportsTasks). The closure captures only locals, never the
// cache or *common.Common, so it is safe off-thread.
func (c *agentListCache) dispatchRefresh(com *common.Common) tea.Cmd {
	if c.cache.inFlight || com == nil || com.Workspace == nil || !com.Workspace.SupportsTasks() {
		return nil
	}
	gen, started := c.cache.begin()
	if !started {
		return nil
	}
	ws := com.Workspace
	ctx := com.Context()
	return func() tea.Msg {
		agents, err := ws.ListTasks(ctx)
		if err != nil {
			slog.Error("Failed to list delegations", "error", err)
		}
		return agentsLoadedMsg{gen: gen, agents: agents, err: err}
	}
}

// applyLoaded stores an off-thread fetch result. Runs on the Update
// goroutine. applied reports whether msg was written through, as opposed to
// discarded for a stale generation or a failure.
func (c *agentListCache) applyLoaded(com *common.Common, msg agentsLoadedMsg) (cmds []tea.Cmd, applied bool) {
	if msg.err != nil {
		if !c.cache.fail(msg.gen) {
			if cmd := c.dispatchRefresh(com); cmd != nil {
				return []tea.Cmd{cmd}, false
			}
		}
		return nil, false
	}
	if !c.cache.complete(msg.gen) {
		// Began before a newer state transition; discard and re-dispatch so
		// the authoritative refresh is not lost to an older request being
		// in flight.
		if cmd := c.dispatchRefresh(com); cmd != nil {
			return []tea.Cmd{cmd}, false
		}
		return nil, false
	}
	c.cache.set(msg.agents)
	return nil, true
}

// invalidate marks the cached list stale and bumps the generation so any
// in-flight result is discarded when it lands.
func (c *agentListCache) invalidate() {
	c.cache.invalidate()
}

// applyEvent reacts to a delegation pubsub event: it upserts (Created,
// Updated) or removes (Deleted) the event's row so the panel reflects the
// change on the edge itself, then invalidates the TTL so a refresh
// eventually reconciles with the authoritative list.
//
// Tasks only, the mirror of threadListCache.applyEvent's thread-only filter:
// the two kinds share one table and one lifecycle publisher. An empty kind
// reads as a thread there (an additive field, so a payload predating it
// describes a thread), and for the same reason it is not a task here. A
// removal is not filtered — dropping a row this cache does not hold is
// already a no-op, and refusing to drop one it does hold would strand it.
func (c *agentListCache) applyEvent(evt pubsub.Event[proto.Thread]) {
	if evt.Type != pubsub.DeletedEvent && proto.ThreadKind(evt.Payload.Kind) != proto.ThreadKindTask {
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
//
// A fetched-and-empty list stops the polling until an event invalidates it
// (the timestamp is zeroed then), the same way the thread list's backstop
// does: a session that never delegates anything must not re-list forever,
// and a delegation's own create event is what starts the section moving.
func (c *agentListCache) staleRefreshCmd(com *common.Common, active bool) tea.Cmd {
	if !active || c.cache.fresh(agentsCacheTTL) || c.cache.backingOff(threadsRefreshBackoff) {
		return nil
	}
	if len(c.cache.value) == 0 && !c.cache.timestamp.IsZero() {
		return nil
	}
	return c.dispatchRefresh(com)
}

// sessionDelegations filters agents down to the live delegations of
// parentSessionID — the ones worth a block in that session's panel: pending,
// running or merging (proto.ThreadStatus.Active, mirroring
// activeDockThreads) plus idle, which is a delegation whose run is not in
// flight this instant but which has not finished either and must not read as
// done. Sorted stably by CreatedAt ascending, so the first one started leads
// and the order does not shuffle under a refresh.
//
// Scoped to one parent session on purpose: a delegation started by another
// session — or by another delegation, which parents to its own child session
// — is live work, but it is not this conversation's, and the panel is the
// view of the session being driven.
func sessionDelegations(agents []proto.Thread, parentSessionID string) []proto.Thread {
	if parentSessionID == "" {
		return nil
	}
	var live []proto.Thread
	for _, a := range agents {
		if a.ParentSessionID != parentSessionID {
			continue
		}
		if proto.ThreadStatus(a.Status).Active() || proto.ThreadStatus(a.Status) == proto.ThreadStatusIdle {
			live = append(live, a)
		}
	}
	sortThreadsByCreation(live)
	return live
}
