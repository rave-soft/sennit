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
// Deliberately separate from threads.ListCache rather than folded into it:
// that cache is Kind-scoped to threads on both its fetch and its event path,
// for good reasons documented there (a task has no worktree, is never
// merged, and used to accumulate in the list and bury the threads). This is
// the mirror image of it, scoped the other way. Both share the
// dispatchRefresh/applyLoaded/applyEvent/staleRefreshCmd machinery in
// list_cache.go; this file only supplies what's specific to delegations —
// the ListTasks call, the SupportsTasks gate, the ThreadKindTask filter, and
// the agentsLoadedMsg shape.

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/listcache"
	"github.com/rave-soft/sennit/internal/ui/threads"
)

// agentsCacheTTL bounds how long the memoized task list may go without a
// re-probe being scheduled. Shorter than threadsCacheTTL: a delegation is
// usually a matter of seconds to a couple of minutes, and unlike a thread it
// has no per-entity activity probe behind it, so this list is the only thing
// that ever moves its block. Package var so tests can pin it.
var agentsCacheTTL = 3 * time.Second

// agentListCache holds the memoized delegation list plus its TTL-cache and
// in-flight/generation bookkeeping.
type taskListWorkspace interface {
	SupportsTasks() bool
	ListTasks(context.Context) ([]proto.Thread, error)
}

type agentListCache struct {
	cache listcache.TTLCache[[]proto.Thread]
}

// agentsLoadedMsg delivers the result of an off-thread task list fetch.
//
// uiOwned, not uimsg.MainScreenOwned: agentViewsRefreshCmds is a *UI method
// (gated on panelSurfacesThreads() && state == uiChat && hasSession()), and
// an attached thread's embedded UI is itself a *UI that can dispatch this
// same fetch for its own session panel. Tagging it MainScreenOwned would
// misroute a thread UI's own fetch to the main screen instead of back to
// the thread — the fetch that started it never gets its result, so its
// InFlight flag never clears and the thread's own agents section freezes
// exactly the way the main screen's did before this marker existed.
type agentsLoadedMsg struct {
	uiOwned

	// gen is the generation captured when the fetch was dispatched; a
	// result that no longer matches began before a newer state transition
	// and is discarded, then re-fetched.
	gen    uint64
	agents []proto.Thread
	err    error
}

// ops builds the listCacheOps that plug delegations' specifics (fetch call,
// support gate, Kind filter, log label) into the shared machinery in
// list_cache.go. owner is stamped onto the result so Root can hand it back
// to the *UI that dispatched the fetch instead of routing it by whichever
// screen is active when it lands — see agentsLoadedMsg's doc comment.
func (c *agentListCache) ops(owner *UI) listcache.Ops[agentsLoadedMsg, taskListWorkspace] {
	return listcache.Ops[agentsLoadedMsg, taskListWorkspace]{
		Label:     "delegations",
		TTL:       agentsCacheTTL,
		Backoff:   listcache.RefreshBackoff,
		Kind:      proto.ThreadKindTask,
		Available: func(ws taskListWorkspace) bool { return ws != nil },
		Supports:  func(ws taskListWorkspace) bool { return ws.SupportsTasks() },
		Fetch:     func(ctx context.Context, ws taskListWorkspace) ([]proto.Thread, error) { return ws.ListTasks(ctx) },
		Wrap: func(gen uint64, items []proto.Thread, err error) agentsLoadedMsg {
			return agentsLoadedMsg{uiOwned: uiOwned{owner: owner}, gen: gen, agents: items, err: err}
		},
		Unwrap: func(msg agentsLoadedMsg) (uint64, []proto.Thread, error) {
			return msg.gen, msg.agents, msg.err
		},
	}
}

// applyLoaded stores an off-thread fetch result. Runs on the Update
// goroutine. applied reports whether msg was written through, as opposed to
// discarded for a stale generation or a failure.
func (c *agentListCache) applyLoaded(com *common.Common, owner *UI, msg agentsLoadedMsg) (cmds []tea.Cmd, applied bool) {
	if com == nil || com.Workspace == nil {
		return listcache.ApplyLoaded(&c.cache, taskListWorkspace(nil), nil, c.ops(owner), msg)
	}
	return listcache.ApplyLoaded(&c.cache, taskListWorkspace(com.Workspace), com.Context(), c.ops(owner), msg)
}

// applyEvent reacts to a delegation pubsub event: it upserts (Created,
// Updated) or removes (Deleted) the event's row so the panel reflects the
// change on the edge itself, then invalidates the TTL so a refresh
// eventually reconciles with the authoritative list.
//
// Tasks only, the mirror of threads.ListCache.applyEvent's thread-only filter:
// the two kinds share one table and one lifecycle publisher.
func (c *agentListCache) applyEvent(evt pubsub.Event[proto.Thread]) {
	listcache.ApplyEvent(&c.cache, proto.ThreadKindTask, evt)
}

// staleRefreshCmd is the TTL backstop: while active and the memoized list
// has outlived its TTL, it schedules an off-thread re-probe. It never does
// IO itself.
//
// A fetched-and-empty list stops the polling until an event invalidates it
// (the timestamp is zeroed then), the same way the thread list's backstop
// does: a session that never delegates anything must not re-list forever,
// and a delegation's own create event is what starts the section moving.
func (c *agentListCache) staleRefreshCmd(com *common.Common, owner *UI, active bool) tea.Cmd {
	if com == nil || com.Workspace == nil {
		return nil
	}
	return listcache.StaleRefreshCmd(&c.cache, taskListWorkspace(com.Workspace), com.Context(), active, c.ops(owner))
}

// sessionDelegations filters agents down to the live delegations of
// parentSessionID — the ones worth a block in that session's panel: pending,
// running or merging (proto.ThreadStatus.Active, mirroring
// threads.ActiveDockThreads) plus idle, which is a delegation whose run is not in
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
	threads.SortByCreation(live)
	return live
}
