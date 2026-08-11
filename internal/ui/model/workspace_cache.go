package model

// Memoized workspace state.
//
// In client/server mode every workspace probe (busy checks, permission mode,
// queued prompts, agent readiness/model, LSP state) is a synchronous HTTP
// round-trip, and the Update goroutine is the render loop — blocking it
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
	"slices"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/rave-soft/braid/internal/workspace"
)

// busyCacheTTL bounds how long the memoized busy/permission state may go
// without a re-probe being scheduled. Package var so tests can pin it.
var busyCacheTTL = 500 * time.Millisecond

// promptQueueTTL is the backstop refresh interval for the queued-prompt
// state; the queue is otherwise refreshed on event edges.
var promptQueueTTL = 2 * time.Second

// workspaceCacheState holds the memoized workspace busy/permission/queue
// state (see the package doc comment above) plus its TTL-cache and
// in-flight/generation bookkeeping.
type workspaceCacheState struct {
	// promptQueue / promptQueueItems mirror the session's queued prompts.
	// They are event-driven with a TTL backstop, fetched off-thread by
	// dispatchPromptQueueRefresh; promptQueue is always
	// len(promptQueueItems).
	promptQueue          int
	promptQueueItems     []string
	promptQueueCheckedAt time.Time
	promptQueueInFlight  bool
	// promptQueueGen is bumped by every queue state transition; an
	// in-flight fetch captures it at dispatch and its result is discarded
	// if the generation has moved on.
	promptQueueGen uint64
	// agentBusyCache / yoloCache memoize the workspace busy and permission
	// probes (synchronous HTTP round-trips in client/server mode). Reads
	// never probe; refreshes happen off-thread.
	agentBusyCache    ttlCache
	yoloCache         ttlCache
	busyFetchInFlight bool
	// agentReady / agentModel memoize the coordinator readiness and
	// selected model (AgentIsReady/AgentModel are synchronous HTTP GETs in
	// client/server mode, and modelInfo renders them every frame). Seeded
	// once at construction and refreshed by the same off-thread probe as
	// agentBusyCache.
	agentReady bool
	agentModel workspace.AgentModel
	// busyFetchGen is bumped by every busy/permission state transition;
	// like promptQueueGen it lets a stale in-flight probe result be
	// discarded and re-fetched instead of clobbering newer state.
	busyFetchGen uint64
}

// ttlCache memoizes one boolean workspace probe result.
type ttlCache struct {
	val bool
	at  time.Time
}

// fresh reports whether the cached value is within its TTL.
func (c *ttlCache) fresh(ttl time.Duration) bool {
	return !c.at.IsZero() && time.Since(c.at) < ttl
}

// set writes a known-good value through the cache.
func (c *ttlCache) set(val bool) {
	c.val = val
	c.at = time.Now()
}

// invalidate marks the value stale so the next Update-tail backstop
// re-probes; the last value keeps being served in the meantime.
func (c *ttlCache) invalidate() {
	c.at = time.Time{}
}

// busyStateMsg delivers the result of an off-thread busy/permission probe.
type busyStateMsg struct {
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
type agentRunSubmittedMsg struct{}

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
func (m *UI) currentSessionID() string {
	if m.session == nil {
		return ""
	}
	return m.session.ID
}

// invalidateBusyCaches marks all memoized workspace probe state stale and
// bumps the busy generation so any in-flight probe result is discarded when
// it lands. Called by handlers for events that change agent or permission
// state.
func (m *UI) invalidateBusyCaches() {
	m.wsCache.agentBusyCache.invalidate()
	m.wsCache.yoloCache.invalidate()
	m.wsCache.busyFetchGen++
}

// invalidatePromptQueue bumps the prompt-queue generation so any in-flight
// queue fetch result is discarded when it lands (and re-fetched) instead of
// overwriting newer optimistic or cleared queue state.
func (m *UI) invalidatePromptQueue() {
	m.wsCache.promptQueueGen++
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
	return func() tea.Msg {
		st := busyStateMsg{gen: gen}
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
// be re-probed after the rebuild lands (a synchronous HTTP round-trip in
// client/server mode), so the message drives the refresh instead of each
// call site remembering to.
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
	prevBusy := m.isAgentBusy()
	prevYolo := m.yoloModeCached()
	m.wsCache.agentBusyCache.set(msg.agentBusy)
	m.wsCache.yoloCache.set(msg.yolo)
	m.wsCache.agentReady = msg.ready
	m.wsCache.agentModel = msg.model
	if prevYolo != msg.yolo {
		// A remote/async toggle changed yolo mode: update the editor
		// prompt function so the prompt icon/style tracks the new mode.
		// The cache is written above and the placeholder is refreshed by
		// the Update tail.
		m.setEditorPrompt(msg.yolo)
	}

	var cmds []tea.Cmd
	busy := m.isAgentBusy()
	if m.hasSession() && hasInProgressTodo(m.session.Todos) && busy && !m.todoIsSpinning {
		m.todoIsSpinning = true
		cmds = append(cmds, m.todoSpinner.Tick)
	}
	if m.todoIsSpinning && !busy {
		m.todoIsSpinning = false
	}
	if prevBusy != busy {
		m.renderPills()
	}
	return cmds
}

// dispatchPromptQueueRefresh returns a command that fetches the queued
// prompts off the Update goroutine, delivering a promptQueueMsg. It returns
// nil while a fetch is already in flight. With no active session the queue
// is simply cleared.
func (m *UI) dispatchPromptQueueRefresh() tea.Cmd {
	if m.wsCache.promptQueueInFlight || m.com == nil || m.com.Workspace == nil {
		return nil
	}
	if !m.hasSession() {
		m.wsCache.promptQueueItems = nil
		m.wsCache.promptQueueCheckedAt = time.Now()
		// Bump the generation so any in-flight fetch scoped to the
		// now-departed session is discarded rather than repopulating the
		// queue.
		m.invalidatePromptQueue()
		if m.wsCache.promptQueue != 0 {
			m.wsCache.promptQueue = 0
			m.updateLayoutAndSize()
		}
		return nil
	}
	m.wsCache.promptQueueInFlight = true
	ws := m.com.Workspace
	sessionID := m.session.ID
	gen := m.wsCache.promptQueueGen
	return func() tea.Msg {
		msg := promptQueueMsg{forSession: sessionID, gen: gen}
		if ws.AgentIsReady() {
			msg.prompts = ws.AgentQueuedPromptsList(sessionID)
		}
		return msg
	}
}

// applyPromptQueue stores an off-thread queue fetch and re-layouts when the
// count changed. Runs on the Update goroutine.
func (m *UI) applyPromptQueue(msg promptQueueMsg) []tea.Cmd {
	m.wsCache.promptQueueInFlight = false
	if msg.forSession != m.currentSessionID() || msg.gen != m.wsCache.promptQueueGen {
		// The fetch raced a session switch or a newer queue transition
		// (submit, clear, invalidation). Discard the stale result and
		// re-fetch so newer state is not clobbered and the authoritative
		// refresh is not lost to this older in-flight request.
		if cmd := m.dispatchPromptQueueRefresh(); cmd != nil {
			return []tea.Cmd{cmd}
		}
		return nil
	}
	m.wsCache.promptQueueCheckedAt = time.Now()
	itemsChanged := !slices.Equal(m.wsCache.promptQueueItems, msg.prompts)
	countChanged := len(msg.prompts) != m.wsCache.promptQueue
	m.wsCache.promptQueueItems = msg.prompts
	m.wsCache.promptQueue = len(msg.prompts)
	if countChanged {
		m.updateLayoutAndSize()
	} else if itemsChanged {
		m.renderPills()
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
	if !m.wsCache.agentBusyCache.fresh(busyCacheTTL) || !m.wsCache.yoloCache.fresh(busyCacheTTL) {
		if cmd := m.dispatchBusyRefresh(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	if m.hasSession() && time.Since(m.wsCache.promptQueueCheckedAt) >= promptQueueTTL {
		if cmd := m.dispatchPromptQueueRefresh(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	if time.Since(m.lspCheckedAt) >= lspStatesTTL {
		if cmd := m.dispatchLSPRefresh(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	if cmd := m.threadIndicator.staleRefreshCmd(m.com); cmd != nil {
		cmds = append(cmds, cmd)
	}
	return cmds
}

// toggleYoloMode flips permission auto-approval and writes the new value
// through the yolo cache (no re-probe needed) and the editor prompt. Shared
// by the direct keybinding and the commands-dialog action so both stay
// write-through. Returns the new mode.
func (m *UI) toggleYoloMode() bool {
	yolo := !m.com.Workspace.PermissionSkipRequests()
	m.com.Workspace.PermissionSetSkipRequests(yolo)
	m.wsCache.yoloCache.set(yolo)
	// Supersede any in-flight busy/yolo probe: its result carries the old
	// generation and would otherwise overwrite the value we just wrote.
	// Bump the generation (rather than invalidateBusyCaches, which would
	// clear the fresh value) so applyBusyState's guard discards and
	// re-dispatches the stale probe.
	m.wsCache.busyFetchGen++
	m.setEditorPrompt(yolo)
	return yolo
}

// yoloModeCached reports the memoized permission-skip ("yolo") mode. Toggles
// write through the cache; the Update-tail backstop keeps it bounded-stale
// otherwise.
func (m *UI) yoloModeCached() bool {
	return m.wsCache.yoloCache.val
}
