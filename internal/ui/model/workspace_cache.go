package model

// Memoized workspace state.
//
// Every workspace probe (busy checks, permission mode, queued prompts, agent
// readiness/model, LSP state) goes through the workspace.Workspace boundary
// and is treated as IO (see internal/ui/AGENTS.md: never do IO or expensive
// work in Update). The Update goroutine is the render loop — blocking it
// freezes typing. The UI therefore never probes the workspace synchronously
// from Update or View. (The constructor is the one carve-out: New seeds the
// yolo and ready/model caches synchronously so the first frame has values to
// render; Init then refreshes them off-thread.)
//
//   - Reads (isAgentBusy, yoloModeCached, promptQueue, selectedModel,
//     lspInfo) always return the memoized value, stale or not.
//   - State edges (message created, agent finished/errored, prompt
//     submitted, cancel, session switch, yolo toggle, model change, LSP
//     events) invalidate or write through the caches and dispatch an
//     off-thread refresh cmd.
//   - A TTL backstop at the end of Update re-dispatches a refresh whenever
//     the memoized state has gone stale, so unrelated churn (typing,
//     resize storms, spinner ticks) only ever schedules async work.
//
// Fresh values arrive as busyStateMsg / promptQueueMsg / lspStatesMsg and
// are applied on the Update goroutine, per the UI guidelines (no IO in
// Update, no model mutation inside commands).

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/rave-soft/sennit/internal/ui/listcache"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/rave-soft/sennit/internal/workspace"
)

// busyCacheTTL bounds how long the memoized busy/permission state may go
// without a re-probe being scheduled. Package var so tests can pin it.
var busyCacheTTL = 500 * time.Millisecond

// promptQueueTTL is the backstop refresh interval for the queued-prompt
// state; the queue is otherwise refreshed on event edges.
var promptQueueTTL = 2 * time.Second

// agentReadyModel bundles AgentIsReady/AgentModel, probed together by one
// off-thread call and so cached as a unit.
type agentReadyModel struct {
	ready bool
	model workspace.AgentModel
}

// workspaceCacheState holds the memoized workspace busy/permission/queue
// state (see the package doc comment above) plus its TTL-cache and
// in-flight/generation bookkeeping.
type workspaceCacheState struct {
	// promptQueueCache mirrors the session's queued prompts. It is
	// event-driven with a TTL backstop, fetched off-thread by
	// dispatchPromptQueueRefresh; the queue count is always
	// len(promptQueueCache.value).
	promptQueueCache listcache.TTLCache[[]string]
	// agentBusyCache / yoloCache memoize the workspace busy and permission
	// probes (treated as IO — see the package doc comment above). Reads
	// never probe; refreshes happen off-thread.
	agentBusyCache    listcache.TTLCache[bool]
	yoloCache         listcache.TTLCache[bool]
	busyFetchInFlight bool
	// agentCache memoizes the coordinator readiness/model (treated as IO;
	// modelInfo renders it every frame). Seeded at construction, refreshed
	// by the same off-thread probe as agentBusyCache/yoloCache above.
	agentCache listcache.TTLCache[agentReadyModel]
	// busyFetchGen is bumped by every busy/permission state transition; a
	// stale in-flight probe result is discarded and re-fetched instead of
	// clobbering newer state. It gates agentBusyCache, yoloCache, and
	// agentCache together, since one probe fetches all three at once.
	busyFetchGen uint64
}

// busyStateMsg delivers the result of an off-thread busy/permission probe.
type busyStateMsg struct {
	uiOwned

	// gen is the busy generation captured when the probe was dispatched.
	// A result whose generation no longer matches m.wsCache.busyFetchGen
	// started before a newer state transition (optimistic send,
	// invalidation, session switch, ...) and is discarded, then
	// re-fetched, so the authoritative refresh is never lost to an older
	// in-flight request.
	gen       uint64
	ready     bool
	agentBusy bool
	yolo      bool
	// model is the coordinator's selected model, fetched by the same probe
	// so the sidebar/landing model info renders from memoized state. Zero
	// (and ignored) when ready is false.
	model workspace.AgentModel
}

// promptQueueMsg delivers the queued prompts fetched off-thread.
type promptQueueMsg struct {
	uiOwned

	// forSession is the session the fetch was scoped to; a result that
	// raced a session switch is discarded and re-fetched.
	forSession string
	// gen is the queue generation captured at dispatch; like
	// busyStateMsg.gen it guards against a stale in-flight result
	// overwriting newer optimistic or invalidated queue state.
	gen     uint64
	prompts []string
}

// agentRunSubmittedMsg reports that AgentRun accepted a prompt (it either
// started a run or was enqueued behind one), so busy and queue state should
// be re-fetched.
type agentRunSubmittedMsg struct {
	sessionID      string
	loadGeneration uint64
}

// agentModelChangedMsg reports that the coordinator's model was updated
// (model selection, thinking toggle, reasoning effort), so the memoized
// ready/model state should be re-fetched without waiting for the TTL.
type agentModelChangedMsg struct{}

// agentModelChangedCmd is sequenced after cmds that call UpdateAgentModel so
// the refresh probes the coordinator only once the update has completed.
// Callers should reach for updateAgentModelCmd rather than sequencing this
// by hand.
func agentModelChangedCmd() tea.Msg { return agentModelChangedMsg{} }

// currentSessionID returns the active session's ID, or "" when none.
func (s *sessionState) currentSessionID() string {
	if s.current == nil {
		return ""
	}
	return s.current.ID
}

// invalidateBusyCaches marks all memoized workspace probe state stale and
// bumps the busy generation so any in-flight probe result is discarded when
// it lands. Called by handlers for events that change agent or permission
// state.
func (c *workspaceCacheState) invalidateBusyCaches() {
	c.agentBusyCache.Invalidate()
	c.yoloCache.Invalidate()
	c.busyFetchGen++
}

// invalidatePromptQueue marks the cached queue stale and bumps its
// generation, so any in-flight queue fetch result is discarded when it
// lands (and re-fetched) instead of overwriting newer optimistic or cleared
// queue state. Callers that already know the authoritative new value
// re-freshen it with a follow-up set, per ttlCache's invalidate doc.
func (c *workspaceCacheState) invalidatePromptQueue() {
	c.promptQueueCache.Invalidate()
}

// dispatchBusyRefresh returns a command that probes the workspace busy and
// permission state off the Update goroutine, delivering a busyStateMsg. It
// returns nil while a probe is already in flight. The closure captures only
// locals (never m) so it is safe off-thread; state is applied by
// applyBusyState on the Update goroutine.
func (m *UI) dispatchBusyRefresh() tea.Cmd {
	if m.wsCache.busyFetchInFlight || m.com == nil || m.com.Workspace == nil {
		return nil
	}
	m.wsCache.busyFetchInFlight = true
	ws := m.com.Workspace
	gen := m.wsCache.busyFetchGen
	// owner is a local, like ws and gen: the closure carries the pointer
	// back so Root can address the reply, and never dereferences it.
	owner := m
	return func() tea.Msg {
		st := busyStateMsg{uiOwned: uiOwned{owner: owner}, gen: gen}
		if ws.AgentIsReady() {
			st.ready = true
			st.agentBusy = ws.AgentIsBusy()
			st.model = ws.AgentModel()
		}
		st.yolo = ws.PermissionSkipRequests()
		return st
	}
}

// updateAgentModelCmd sequences a coordinator model rebuild
// (UpdateAgentModel) with the invalidation of the memoized ready/model
// state. Callers wrap their pre-work in pre; the memoized model must only
// be re-probed after the rebuild lands (treated as IO — see the package doc
// comment above), so the message drives the refresh instead of each call
// site remembering to.
func (m *UI) updateAgentModelCmd(pre tea.Cmd) tea.Cmd {
	return tea.Sequence(pre, agentModelChangedCmd)
}

// applyBusyState stores an off-thread probe result and reacts to busy
// edges (todo spinner, pills). Runs on the Update goroutine.
func (m *UI) applyBusyState(msg busyStateMsg) []tea.Cmd {
	m.wsCache.busyFetchInFlight = false
	if msg.gen != m.wsCache.busyFetchGen {
		// This probe started before a newer state transition (optimistic
		// send, invalidation, session switch, ...). Discard its result and
		// re-dispatch so the required authoritative refresh is not lost
		// merely because this older request was in flight.
		if cmd := m.dispatchBusyRefresh(); cmd != nil {
			return []tea.Cmd{cmd}
		}
		return nil
	}
	prevYolo := m.yoloModeCached()
	m.wsCache.agentBusyCache.Set(msg.agentBusy)
	m.wsCache.yoloCache.Set(msg.yolo)
	m.wsCache.agentCache.Set(agentReadyModel{ready: msg.ready, model: msg.model})
	if prevYolo != msg.yolo {
		// A remote/async toggle changed yolo mode: update the editor
		// prompt function so the prompt icon/style tracks the new mode.
		// The cache is written above and the placeholder is refreshed by
		// the Update tail.
		m.setEditorPrompt(msg.yolo)
	}

	var cmds []tea.Cmd
	if cmd := m.syncPanelSpinner(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	return cmds
}

// dispatchPromptQueueRefresh returns a command that fetches the queued
// prompts off the Update goroutine, delivering a promptQueueMsg. It returns
// nil while a fetch is already in flight. With no active session the queue
// is simply cleared.
func (m *UI) dispatchPromptQueueRefresh() tea.Cmd {
	if m.wsCache.promptQueueCache.InFlight || m.com == nil || m.com.Workspace == nil {
		return nil
	}
	if !m.hasSession() {
		hadItems := len(m.wsCache.promptQueueCache.Value) != 0
		// Bump the generation so any in-flight fetch scoped to the
		// now-departed session is discarded rather than repopulating the
		// queue, then write the now-authoritative empty queue through as
		// fresh.
		m.wsCache.invalidatePromptQueue()
		m.wsCache.promptQueueCache.Set(nil)
		if hadItems {
			m.updateLayoutAndSize()
		}
		return nil
	}
	ws := m.com.Workspace
	sessionID := m.sess.current.ID
	gen, started := m.wsCache.promptQueueCache.Begin()
	if !started {
		return nil
	}
	owner := m
	return func() tea.Msg {
		msg := promptQueueMsg{uiOwned: uiOwned{owner: owner}, forSession: sessionID, gen: gen}
		if ws.AgentIsReady() {
			msg.prompts = ws.AgentQueuedPromptsList(sessionID)
		}
		return msg
	}
}

// applyPromptQueue stores an off-thread queue fetch and re-layouts when the
// count changed. Runs on the Update goroutine.
func (m *UI) applyPromptQueue(msg promptQueueMsg) []tea.Cmd {
	// complete clears the in-flight marker unconditionally (even when the
	// session-scope check below rejects the result), matching complete's
	// contract of always being called once per dispatched fetch.
	genOK := m.wsCache.promptQueueCache.Complete(msg.gen)
	if msg.forSession != m.sess.currentSessionID() || !genOK {
		// The fetch raced a session switch or a newer queue transition
		// (submit, clear, invalidation). Discard the stale result and
		// re-fetch so newer state is not clobbered and the authoritative
		// refresh is not lost to this older in-flight request.
		if cmd := m.dispatchPromptQueueRefresh(); cmd != nil {
			return []tea.Cmd{cmd}
		}
		return nil
	}
	countChanged := len(msg.prompts) != len(m.wsCache.promptQueueCache.Value)
	m.wsCache.promptQueueCache.Set(msg.prompts)
	if countChanged {
		// A row-count change moves the panel/chat split; anything else
		// (item text edited in place) is picked up on the next draw, since
		// drawSessionPanel always paints from live state.
		m.updateLayoutAndSize()
	}
	return nil
}

// staleWorkspaceRefreshCmds is the TTL backstop, called at the tail of
// Update: when any memoized workspace state has outlived its TTL (and no
// event edge refreshed it), schedule an off-thread re-probe. It never does
// IO itself — a couple of time comparisons per message at most.
func (m *UI) staleWorkspaceRefreshCmds() []tea.Cmd {
	if m.com == nil || m.com.Workspace == nil {
		return nil
	}
	var cmds []tea.Cmd
	if !m.wsCache.agentBusyCache.Fresh(busyCacheTTL) || !m.wsCache.yoloCache.Fresh(busyCacheTTL) {
		if cmd := m.dispatchBusyRefresh(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	if m.hasSession() && !m.wsCache.promptQueueCache.Fresh(promptQueueTTL) {
		if cmd := m.dispatchPromptQueueRefresh(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	if time.Since(m.lsp.checkedAt) >= lspStatesTTL {
		if cmd := m.dispatchLSPRefresh(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	// The threads dock's list and per-thread activity (step count, current
	// tool) previously re-probed only on thread pubsub events; with the
	// panel spinner ticking whenever a thread is live, this Update tail
	// runs continuously, so their TTLs act as a real backstop and the
	// "what is this thread doing" line stays fresh between events.
	cmds = append(cmds, m.threadViewsRefreshCmds()...)
	cmds = append(cmds, m.agentViewsRefreshCmds()...)
	return cmds
}

// threadViewsRefreshCmds re-probes the shared thread list (feeding the
// header badge, the dashboard, and the dock) and the dock's per-thread
// activity wherever a TTL has expired or an invalidation demands it.
// Shared by the thread-event handler and the Update-tail backstop so the
// sequence exists exactly once. It never does IO itself, and it costs
// nothing beyond time comparisons while the project has no threads.
//
// The list refresh is unconditional (not gated on m.state) because the
// header badge needs it current on every screen this UI ever draws, and
// gating it to uiChat would just make the dock wait for the badge's own
// next tick to catch up — the same round trip either way, so there is
// nothing to save by narrowing it.
func (m *UI) threadViewsRefreshCmds() []tea.Cmd {
	if !m.surfacesThreads() {
		return nil
	}
	var cmds []tea.Cmd
	if cmd := m.threadList.StaleRefreshCmd(m.com, true); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if m.state == uiChat && len(m.threadList.Threads()) > 0 {
		visible := activeDockThreads(m.threadList.Threads())
		cmds = append(cmds, m.threadsDock.staleThreadActivityRefreshCmds(m.com, visible)...)
	}
	return cmds
}

// agentViewsRefreshCmds re-probes the delegation list behind the session
// panel's agents section wherever its TTL has expired or an invalidation
// demands it. Like threadViewsRefreshCmds it never does IO itself.
//
// Narrower than its threads counterpart in two ways, both because a
// delegation has exactly one consumer — the panel of the session that
// started it. It is gated on uiChat, since no other screen shows one; and
// once the list has come back empty it stops re-polling until an event
// invalidates it (see agentListCache.staleRefreshCmd), so a session that
// never delegates anything costs a single round trip in total.
func (m *UI) agentViewsRefreshCmds() []tea.Cmd {
	if !m.panelSurfacesThreads() || m.state != uiChat || !m.hasSession() {
		return nil
	}
	if cmd := m.agentList.staleRefreshCmd(m.com, true); cmd != nil {
		return []tea.Cmd{cmd}
	}
	return nil
}

func (m *UI) toggleYoloMode() tea.Cmd {
	if m.ops.yoloLoading {
		return util.ReportWarn("Yolo mode is already being updated")
	}
	desired := !m.yoloModeCached()
	m.ops.yoloLoading = true
	m.ops.yoloGeneration++
	generation := m.ops.yoloGeneration
	workspace := m.com.Workspace
	return func() tea.Msg {
		workspace.PermissionSetSkipRequests(desired)
		return yoloToggledMsg{Enabled: desired, generation: generation}
	}
}

// yoloModeCached reports the memoized permission-skip ("yolo") mode. Toggles
// write through the cache; the Update-tail backstop keeps it bounded-stale
// otherwise.
func (m *UI) yoloModeCached() bool {
	return m.wsCache.yoloCache.Value
}
