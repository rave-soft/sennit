package model

// The session panel is the merged strip between chat and the editor that
// replaces the old, separately-wired "pills" (todo progress + queued-prompt
// pills) and "threads dock" (active background threads). It paints, top to
// bottom: one block per live delegation of this session (the agents
// section), one block per active thread, a collapsible todos section, and
// an always-visible queued-prompts list. Agents lead because they are the
// work of the conversation right above them — a delegation reports back
// into this very transcript, while a thread runs off in its own worktree.
//
// sessionPanelPlan is the single source of truth for how many rows each
// section gets and what it contains. sessionPanelHeight and drawSessionPanel
// both call it with the same row budget, so the space generateLayout
// reserves and what drawSessionPanel paints can never disagree — unlike the
// old pills.go/threads_dock_view.go split, where height and content were
// computed independently.

import (
	"fmt"
	"image"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/chat"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/presentation"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/ui/util"
)

// threadDockStatusText builds one thread's live status text for its
// block's second row: threadDockStatusLine over the thread's cached
// activity, with the elapsed time measured from CreatedAt — not UpdatedAt,
// which is bumped on every status transition (see thread/store.go
// SetStatus), so it tracks "last activity", not "how long has this thread
// been running"; the panel wants the latter.
func threadDockStatusText(t proto.Thread, activity threadDockActivity) string {
	elapsed := time.Since(time.Unix(t.CreatedAt, 0))
	return threadDockStatusLine(proto.ThreadStatus(t.Status), activity, elapsed)
}

// panelSpinnerWanted reports whether the panel currently shows any live
// work worth animating: an in-progress todo while the local agent is busy
// (the original todo-spinner condition), an active background thread, or a
// running delegation of this session. The spinner used to belong to the
// todos list alone; the thread and agent blocks now share it, so it must
// keep ticking even when the local agent is idle and only delegated work is
// moving.
func (m *UI) panelSpinnerWanted() bool {
	if !m.hasSession() {
		return false
	}
	if m.isAgentBusy() && hasInProgressTodo(m.sess.current.Todos) {
		return true
	}
	for _, t := range m.threadList.cache.value {
		switch proto.ThreadStatus(t.Status) {
		case proto.ThreadStatusRunning, proto.ThreadStatusMerging:
			return true
		}
	}
	for _, a := range m.agentList.cache.value {
		if a.ParentSessionID == m.sess.current.ID && proto.ThreadStatus(a.Status) == proto.ThreadStatusRunning {
			return true
		}
	}
	return false
}

// panelShowsLiveDelegations reports whether the panel's agents section is
// currently carrying this session's live delegations — the same two
// conditions sessionPanelPlan uses to fill plan.agents, asked in one place
// so the panel and the transcript cannot disagree about who is showing
// what.
//
// Collapsing the section is deliberately not one of them: it hides the
// blocks without ending the panel's claim on them, exactly as a collapsed
// todos panel still owns the todo list.
func (m *UI) panelShowsLiveDelegations() bool {
	if !m.hasSession() || !m.panelSurfacesThreads() {
		return false
	}
	return len(sessionDelegations(m.agentList.cache.value, m.sess.current.ID)) > 0
}

// syncPanelSpinner reconciles the shared panel spinner with
// panelSpinnerWanted: on a stopped→spinning transition it returns the
// initial Tick command that starts the tick loop (nil otherwise), and it
// stops the spinner the moment nothing live is left. Every event that can
// change the answer (busy refresh landing, session/thread events, dock
// refreshes) funnels through this instead of duplicating the condition.
func (m *UI) syncPanelSpinner() tea.Cmd {
	want := m.panelSpinnerWanted()
	if want && !m.panel.isSpinning {
		m.panel.isSpinning = true
		return m.panel.spinner.Tick
	}
	if !want {
		m.panel.isSpinning = false
	}
	return nil
}

// panelActivityIcon is the leading marker for a live block's status line:
// the shared panel spinner's current frame while spinning (the block
// represents work in flight), a static spinner glyph otherwise — the same
// static-fallback the todos list uses, so a briefly-stalled tick loop
// degrades to a frozen glyph rather than a misleading arrow.
func (m *UI) panelActivityIcon() string {
	if m.panel.isSpinning {
		return m.panel.spinner.View()
	}
	return m.com.Styles.Tool.TodoInProgressIcon.Render(styles.SpinnerIcon)
}

// maxPanelTodosRows caps how tall the expanded todos section may grow, in
// rows. A long todo list is a list to scroll, not a wall to read: past a
// dozen or so rows the section stops being a status strip and starts being
// the screen, pushing the chat it is supposed to annotate out of view. The
// rows beyond the cap are never dropped — the section scrolls (see
// todosScrollable / todosScrollOffset), so nothing becomes unreachable.
const maxPanelTodosRows = 10

// maxQueueDisplayLength is the maximum length of a queue item in the list.
const maxQueueDisplayLength = 60

// sessionPanelBudgetFraction caps the panel at 40% of the terminal height,
// so a long backlog of threads/todos/queue can never crowd out chat
// entirely.
const sessionPanelBudgetFraction = 0.4

// sessionPanelState holds the session panel's expand/render state. Todos and
// the queue are always both shown when they have content (no more
// mutually-exclusive "focused section" the way the old pills panel had), so
// the only state left to track is whether the todos list is expanded.
type sessionPanelState struct {
	expanded     bool
	autoExpanded bool
	// threadsCollapsed hides the threads section's blocks, leaving its
	// header. Default false, so threads keep showing as they always have
	// until someone collapses them; the todos section defaults the other
	// way, since its header's completed/total ratio still reports the whole
	// list's state while a collapsed threads header only reports a count.
	threadsCollapsed bool

	// hoveredThread is the index (into threads/threadRects)
	// of the thread block currently under the pointer, -1 for none. Set
	// from tea.MouseMotionMsg, read by drawSessionPanel for hover styling.
	hoveredThread int
	// threadRects/threads are parallel slices — the on-screen
	// rect and source thread for each currently visible thread block —
	// rebuilt on every drawSessionPanel call. Used by the MouseClickMsg
	// hit-test (drill into the clicked thread) and MouseMotionMsg hover
	// tracking.
	threadRects []uv.Rectangle
	threads     []proto.Thread
	// todosHover / todosHeaderRect mirror childPanelHover /
	// childPanelButtonRect for the todos header row's click-to-toggle
	// affordance.
	// agentsCollapsed mirrors threadsCollapsed for the agents section: it
	// hides the delegation blocks and leaves the header, which still
	// reports how many are live.
	agentsCollapsed bool
	// hoveredAgent, agentRects and agents mirror the hoveredThread /
	// threadRects / threads trio for the agents section.
	hoveredAgent int
	agentRects   []uv.Rectangle
	agents       []proto.Thread

	todosHover      bool
	todosHeaderRect uv.Rectangle
	// threadsHover / threadsHeaderRect mirror the todos pair for
	// the threads section's own collapse header.
	threadsHover      bool
	threadsHeaderRect uv.Rectangle
	// agentsHover / agentsHeaderRect are the same pair for the agents
	// section's collapse header.
	agentsHover      bool
	agentsHeaderRect uv.Rectangle
	// todosListRect is the on-screen area of the (possibly scrollable)
	// todo rows below the header, rebuilt on every drawSessionPanel call —
	// the mouse-wheel handler in Update hit-tests against this to decide
	// whether a wheel event scrolls the todos section instead of the chat.
	todosListRect uv.Rectangle
	// todosScrollOffset is the expanded todos section's own scroll
	// position — an index into the concatenated in-progress+pending+done
	// row list (see sessionPanelTodosContent), independent of chat/sidebar
	// scrolling. sessionPanelPlan never drops todosDone/todosPending to fit
	// the panel's row budget anymore (see session_panel.go); when the
	// section's natural size exceeds what's granted, this offset is what
	// reveals the rest instead. drawSessionPanel clamps it to
	// [0, max(0, contentRows-viewportRows)] every frame, so a stale offset
	// left over from a shorter list (todos completed/removed) never
	// dangles out of range.
	todosScrollOffset int

	// Todo spinner
	spinner    spinner.Model
	isSpinning bool
}

// hasIncompleteTodos returns true if there are any non-completed todos.
func hasIncompleteTodos(todos []session.Todo) bool {
	return session.HasIncompleteTodos(todos)
}

// hasInProgressTodo returns true if there is at least one in-progress todo.
func hasInProgressTodo(todos []session.Todo) bool {
	for _, todo := range todos {
		if todo.Status == session.TodoStatusInProgress {
			return true
		}
	}
	return false
}

// splitTodosByStatus splits todos into in-progress, pending, and completed,
// each preserving relative order. This is the opposite of
// chat.FormatTodosList's completed-first ordering: that function renders a
// single tool call's transcript, where reading top-down chronologically
// makes sense; the panel is a persistent status view, where what's
// happening now belongs above what's already done — see sessionPanelPlan's
// todosInProgress/todosPending/todosDone.
func splitTodosByStatus(todos []session.Todo) (inProgress, pending, completed []session.Todo) {
	buckets := presentation.BucketTodos(todos)
	return buckets.InProgress, buckets.Pending, buckets.Completed
}

// renderSessionTodoLine renders one todo row for the panel's expanded todos
// list. Unlike chat.FormatTodosList — built for a single tool call's
// transcript, where at most one todo is ever in_progress at a time — this
// is called once per todo independently, so every concurrently in-progress
// todo gets its own icon. With parallel subagents/threads writing todos at
// once, more than one can be in_progress simultaneously; the old pills.go
// todoPill tracked only the first in-progress todo via a single
// `currentTodo` variable and silently dropped the rest.
func renderSessionTodoLine(todo session.Todo, inProgressIcon string, sty *styles.Styles, width int) string {
	return presentation.RenderTodoRow(todo, sty, width, presentation.TodoRowOptions{
		InProgressIcon:         inProgressIcon,
		CompletedMuted:         true,
		CompletedStrikethrough: true,
	})
}

// sessionPanelTodosHeaderText renders the todos header row's plain text:
// "todos <completed>/<total>" plus a disclosure triangle — ▸ collapsed, ▾
// expanded. No inline current-task preview, unlike the old todoPill.
func sessionPanelTodosHeaderText(completed, total int, expanded bool) string {
	chevron := "▸"
	if expanded {
		chevron = "▾"
	}
	return fmt.Sprintf("todos %d/%d %s", completed, total, chevron)
}

// shedPanelBlocks drops whole two-row blocks until the plan fits
// the budget, returning the surviving blocks, the updated hidden count,
// and the rows they occupy. Blocks are dropped from the end, so the
// oldest (first-created) threads are the ones that stay visible, and
// every dropped block is added to the "…and N more" count rather than
// disappearing unannounced.
func shedPanelBlocks(threads []proto.Thread, more, over int) ([]proto.Thread, int, int) {
	drop := (over + 1) / 2 // round up: one row over still costs a block
	if drop > len(threads) {
		drop = len(threads)
	}
	kept := threads[:len(threads)-drop]
	more += drop
	if len(kept) == 0 {
		// Nothing survived: the section disappears entirely, header and
		// footer included. The budget has to be able to reclaim every
		// row it lent — a section that always keeps one line to say how
		// much is hidden could never be shed to zero, which is exactly
		// what a pathological terminal height needs.
		return nil, more, 0
	}
	rows := len(kept) * 2
	if more > 0 {
		rows++ // the footer row that reports what is hidden
	}
	return kept, more, rows
}

// sessionPanelThreadsHeaderText renders the threads header row's plain
// text: "threads <active>" plus the same disclosure triangle the todos
// header uses — ▸ collapsed, ▾ expanded.
func sessionPanelThreadsHeaderText(active int, expanded bool) string {
	chevron := "▸"
	if expanded {
		chevron = "▾"
	}
	return fmt.Sprintf("threads %d %s", active, chevron)
}

// delegationBlockStatusText builds one delegation's status text for its
// block's second row: its status word plus how long it has been going,
// measured from CreatedAt — the same shape (and the same helper) as a
// thread's line, minus the live activity. A task has no per-entity activity
// probe behind it the way a thread does (threadsDockState.activity, paid
// for with an AttachThread round trip into the thread's own workspace); a
// delegation's own transcript is one drill-in away in this very workspace,
// which is what the block's click is for.
func delegationBlockStatusText(t proto.Thread) string {
	elapsed := time.Since(time.Unix(t.CreatedAt, 0))
	return threadDockStatusLine(proto.ThreadStatus(t.Status), threadDockActivity{}, elapsed)
}

// delegationBlockName is the name a delegation's block carries. A task's
// own Name is generated ("task-<uuid>", see thread.TaskManager.Create) and
// says nothing, so this resolves the agent's real display name through the
// transcript instead: the delegation's child session is named for the tool
// call that started it (see delegationSessionID in internal/agent), so the
// id parses straight back to the chat item that has the name on it.
//
// Falls back to "agent" for a delegation with no stub to resolve — one
// started before this session was reloaded far enough back to hold it, or
// by a delegation of its own rather than by this transcript.
func (m *UI) delegationBlockName(t proto.Thread) string {
	if m.com == nil || m.com.Workspace == nil {
		return defaultDelegationBlockName
	}
	_, toolCallID, ok := m.com.Workspace.ParseAgentToolSessionID(t.SessionID)
	if !ok {
		return defaultDelegationBlockName
	}
	item, ok := m.chat.MessageItem(toolCallID).(chat.ToolMessageItem)
	if !ok {
		return defaultDelegationBlockName
	}
	if name, _, _, _, _ := delegationInfo(item); name != "" {
		return name
	}
	return defaultDelegationBlockName
}

// defaultDelegationBlockName is what a delegation block is called when its
// agent's own name cannot be resolved — see delegationBlockName.
const defaultDelegationBlockName = "agent"

// sessionPanelAgentsHeaderText renders the agents header row's plain text:
// "agents <live>" plus the same disclosure triangle the other section
// headers use — ▸ collapsed, ▾ expanded.
func sessionPanelAgentsHeaderText(live int, expanded bool) string {
	chevron := "▸"
	if expanded {
		chevron = "▾"
	}
	return fmt.Sprintf("agents %d %s", live, chevron)
}

// renderSessionQueueLines renders one truncated line per queued prompt,
// mirroring pills.go's old queueList — same prefix/text styling and
// per-item truncation — but with no border or focus gating: the queue is
// now always shown whenever there are queued prompts.
func renderSessionQueueLines(items []string, sty *styles.Styles, width int) []string {
	lines := make([]string, 0, len(items))
	for _, item := range items {
		text := item
		if ansi.StringWidth(text) > maxQueueDisplayLength {
			text = ansi.Truncate(text, maxQueueDisplayLength-1, "…")
		}
		prefix := sty.Pills.QueueItemPrefix.Render() + " "
		line := prefix + sty.Pills.QueueItemText.Render(text)
		lines = append(lines, ansi.Truncate(line, width, "…"))
	}
	return lines
}

// sessionPanelPlan is what sessionPanelPlan computes for a given row budget:
// exactly which threads/todos/queue content fits, in priority order. Both
// sessionPanelHeight (row budgeting in generateLayout) and drawSessionPanel
// consume the same plan for the same budget, so they can never disagree
// about how many rows the panel occupies.
type sessionPanelPlan struct {
	threads     []proto.Thread // visible thread blocks, in draw order
	threadsMore int
	threadsRows int
	// threadsHeaderRows is 1 iff threadsRows > 0 at the final,
	// post-shedding state (0 otherwise) — the "threads" section-separator
	// line drawSessionPanel paints directly above the thread blocks. Kept
	// as its own field (rather than re-deriving threadsRows>0 at draw
	// time) so sessionPanelRowLayout and drawSessionPanel agree on
	// exactly how many rows the header consumes without recomputing it.
	threadsHeaderRows int
	// threadsExpanded is m.panel.threadsCollapsed inverted; when false the
	// section keeps its header (and its count) but paints no blocks.
	threadsExpanded bool
	// threadsActive is how many active delegations exist regardless of
	// the visible cap or the collapse state — what the header reports.
	threadsActive int

	// agents are this session's live delegations (internal/thread's
	// KindTask: the `agent` tool, the custom agent tools, agentic_fetch),
	// drawn with the same two-line block shape as threads. Delegation is
	// asynchronous, so the transcript's stub for one is finished the
	// instant it is created — these blocks are the only live view of a
	// delegation there is.
	agents     []proto.Thread
	agentsMore int
	agentsRows int
	// agentsHeaderRows mirrors threadsHeaderRows for the "agents" header.
	agentsHeaderRows int
	// agentsExpanded is m.panel.agentsCollapsed inverted; when false the
	// section keeps its header (and its count) but paints no blocks.
	agentsExpanded bool
	// agentsLive is how many delegations this session has in flight
	// regardless of the collapse state — what the header reports.
	agentsLive int

	todosVisible    bool // at least one incomplete todo exists
	todosExpanded   bool // m.panel.expanded, verbatim — budget shedding never forces this false anymore
	todosCompleted  int  // for the header ratio — independent of what's dropped below
	todosTotal      int
	todosInProgress []session.Todo // populated only when todosExpanded; never shed by budget
	todosPending    []session.Todo // populated only when todosExpanded; never shed by budget
	todosDone       []session.Todo // populated only when todosExpanded; never shed by budget

	// todosContentRows is the expanded todos section's full row count
	// (todosInProgress+todosPending+todosDone concatenated in that order;
	// zero while collapsed — collapsed is header-only and never scrolls).
	// todosViewportRows is how many of those rows are actually painted:
	// the lesser of the content, maxPanelTodosRows, and what the row
	// budget grants. When it's less than todosContentRows the section is
	// scrollable (todosScrollable) and drawSessionPanel windows the list by
	// m.panel.todosScrollOffset instead of truncating it — see the
	// sessionPanelPlan doc comment.
	todosContentRows  int
	todosViewportRows int
	todosScrollable   bool

	queue []string
	// queueHeaderRows is 1 iff len(queue) > 0 at the final, post-shedding
	// state (0 otherwise) — the "queue" section-separator line
	// drawSessionPanel paints directly above the queue lines.
	queueHeaderRows int

	totalRows int
}

// sessionPanelPlan computes what the panel shows for budget rows, shedding
// content in priority order when the natural size doesn't fit: active
// threads and delegations are never shrunk except as an absolute last
// resort, and — critically — completed/pending todo *data* is never
// dropped once the section is expanded (m.panel.expanded). What used to be
// "drop completed rows, then collapse the list" is now "hand the expanded
// todos section a smaller viewport and let it scroll": todosInProgress,
// todosPending, and todosDone always hold the full lists; todosViewportRows
// is the (possibly smaller) number of those rows the budget actually grants
// for painting this frame, and todosScrollable signals drawSessionPanel to
// render a scrollbar and window the list by m.panel.todosScrollOffset
// instead of truncating it. Concretely: (1) if the expanded todos section's
// natural size doesn't fit, shrink its viewport (not its data); (2) if
// still over, truncate the queue tail; (3) if still over, shrink the
// delegations row budget; (4) if still over (pathological tiny terminal),
// shrink the threads section's row budget, mirroring the pre-existing
// `dockHeight = min(dockHeight, uiLayout.main.Dy())` clamp; (5) as a
// last-resort floor, drop the todos header too, so totalRows never exceeds
// budget even when budget itself is smaller than one row — this still
// leaves the todo slices themselves populated, just undrawn this frame.
//
// Collapsed mode (todosExpanded == false) is header-only: no todo rows at
// all count toward its content, in-progress included, and it never scrolls.
//
// Row counts never depend on rendering width — only per-row text
// truncation does, decided at draw time — matching threadsDockHeight's
// existing convention.
func (m *UI) sessionPanelPlan(budget int) sessionPanelPlan {
	budget = max(0, budget)

	var plan sessionPanelPlan
	if !m.hasSession() || m.activeInline != nil {
		return plan
	}

	if m.panelSurfacesThreads() {
		active := activeDockThreads(m.threadList.cache.value)
		plan.threadsActive = len(active)
		plan.threadsExpanded = !m.panel.threadsCollapsed
		if plan.threadsExpanded {
			// The dock shows every active thread here — no fixed cap. The
			// panel is the live view of what is running right now, and a
			// running thread the panel refuses to name is exactly the one a
			// user goes looking for. Fitting the list on screen is the row
			// budget's job: shedPanelBlocks below sheds thread blocks (and
			// sets plan.threadsMore) only after todos and the queue, i.e.
			// only in a genuinely short terminal.
			plan.threads = active
			plan.threadsRows = len(active) * 2
		}
	}

	if m.panelSurfacesThreads() {
		live := sessionDelegations(m.agentList.cache.value, m.sess.current.ID)
		plan.agentsLive = len(live)
		plan.agentsExpanded = !m.panel.agentsCollapsed
		if plan.agentsExpanded {
			// Every live delegation, uncapped, for the same reason threads
			// are uncapped: the one the panel refuses to name is the one
			// somebody goes looking for. The row budget below sheds blocks
			// if the terminal is genuinely too short.
			plan.agents = live
			plan.agentsRows = len(live) * 2
		}
	}

	todos := m.sess.current.Todos
	plan.todosTotal = len(todos)
	inProgress, pending, completed := splitTodosByStatus(todos)
	plan.todosCompleted = len(completed)
	// The panel is the live view of active work: it disappears once every
	// todo is completed, at which point the chat transcript (always
	// rendering the full list — see chat.TodosToolRenderContext) becomes
	// the permanent record instead, so nothing is actually lost.
	plan.todosVisible = hasIncompleteTodos(todos)
	if plan.todosVisible {
		plan.todosExpanded = m.panel.expanded
		// Collapsing is total: the header alone is left, in-progress rows
		// included. Keeping those pinned open made "collapsed" mean
		// different heights at different moments — the one thing a
		// disclosure toggle should never do — and the header already
		// carries the completed/total ratio, so nothing is silently lost.
		// Budget shedding below never clears these slices; it only ever
		// narrows the viewport that paints them.
		if plan.todosExpanded {
			plan.todosInProgress = inProgress
			plan.todosPending = pending
			plan.todosDone = completed
		}
		plan.todosContentRows = len(plan.todosInProgress) + len(plan.todosPending) + len(plan.todosDone)
		// The natural viewport is the whole list, up to the cap; a longer
		// list scrolls inside those rows rather than growing the panel.
		plan.todosViewportRows = min(plan.todosContentRows, maxPanelTodosRows)
	}

	plan.queue = m.wsCache.promptQueueCache.value

	headerRows := func() int {
		if !plan.todosVisible {
			return 0
		}
		return 1
	}
	// threadsHeaderRows/queueHeaderRows are closures,
	// not snapshotted values, so over() always reflects the CURRENT
	// row/item counts — a header disappears for free the instant its
	// section is shed to zero, which shrinks over() by exactly the row it
	// would otherwise still be charging for.
	threadsHeaderRows := func() int {
		// A collapsed section keeps its header: it is the only thing left
		// to click to get the threads back, and it still reports how many
		// are active. Shedding to zero rows still drops it, since that is
		// the budget reclaiming space rather than a user hiding content.
		if plan.threadsRows > 0 || (!plan.threadsExpanded && plan.threadsActive > 0) {
			return 1
		}
		return 0
	}
	agentsHeaderRows := func() int {
		// Same rule as the threads header: a section collapsed by hand
		// keeps the row that says so and lets it back, a section shed to
		// zero by the budget does not.
		if plan.agentsRows > 0 || (!plan.agentsExpanded && plan.agentsLive > 0) {
			return 1
		}
		return 0
	}
	queueHeaderRows := func() int {
		if len(plan.queue) > 0 {
			return 1
		}
		return 0
	}
	over := func() int {
		return threadsHeaderRows() + plan.threadsRows +
			agentsHeaderRows() + plan.agentsRows +
			headerRows() + plan.todosViewportRows +
			queueHeaderRows() + len(plan.queue) - budget
	}

	// Shedding priority, highest to lowest: threads and agents >
	// todos-in-progress (never touched — see the floor below) > todos
	// pending/done (display-windowed via todosViewportRows, but never
	// dropped from the plan) > queue tail > agents row budget > threads row
	// budget. Agents give way before threads: a delegation is minutes of
	// work whose result comes back into this very transcript, a thread is a
	// whole parallel worktree whose only status is the one shown here. The viewport
	// never shrinks below len(todosInProgress): once the section is open,
	// the default (unscrolled) viewport always shows every in-progress row
	// — only pending/done ever have to be reached by scrolling.
	if plan.todosExpanded {
		if o := over(); o > 0 {
			// The floor is capped too: with more in-progress todos than the
			// section is ever allowed to be tall, "always show every
			// in-progress row" is not a promise the panel can keep, and the
			// height cap is the one that wins — the rest are one scroll away.
			floor := min(len(plan.todosInProgress), maxPanelTodosRows)
			reducible := max(0, plan.todosViewportRows-floor)
			plan.todosViewportRows -= min(o, reducible)
		}
	}
	if o := over(); o > 0 {
		keep := max(0, len(plan.queue)-o)
		plan.queue = plan.queue[:keep]
	}
	if o := over(); o > 0 {
		plan.agents, plan.agentsMore, plan.agentsRows = shedPanelBlocks(plan.agents, plan.agentsMore, o)
	}
	if o := over(); o > 0 {
		// Shed whole blocks, not rows. A thread block is two rows, so
		// subtracting an odd count used to leave half a block painted —
		// a name with no status line under it — and the hidden threads
		// vanished silently, with the "…and N more" footer still
		// reporting only what the visible cap had dropped.
		plan.threads, plan.threadsMore, plan.threadsRows = shedPanelBlocks(plan.threads, plan.threadsMore, o)
	}
	if over() > 0 {
		// budget < 1: even the one-line todos header doesn't fit. The todo
		// slices stay populated (see doc comment) — only this frame's
		// paint is skipped.
		plan.todosVisible = false
		plan.todosViewportRows = 0
	}

	plan.todosScrollable = plan.todosExpanded && plan.todosVisible && plan.todosViewportRows < plan.todosContentRows
	plan.threadsHeaderRows = threadsHeaderRows()
	plan.agentsHeaderRows = agentsHeaderRows()
	plan.queueHeaderRows = queueHeaderRows()
	plan.totalRows = plan.threadsHeaderRows + plan.threadsRows +
		plan.agentsHeaderRows + plan.agentsRows +
		headerRows() + plan.todosViewportRows +
		plan.queueHeaderRows + len(plan.queue)
	return plan
}

// sessionPanelHeight reports how many rows the merged session panel needs,
// capped at sessionPanelBudgetFraction of available — the space actually
// contested between chat and the panel (mainRect.Dy() at the call site in
// generateLayout), not the whole-terminal m.lay.height. Budgeting off the full
// terminal height would make the 40% cap far tighter than intended on a
// typical screen (e.g. 40% of an 80x24 terminal is 9 rows total, including
// the header/editor/help chrome that never competes with the panel), which
// triggered shedding for even a small, everyday todo list.
func (m *UI) sessionPanelHeight(available int) int {
	budget := max(0, min(available, int(float64(available)*sessionPanelBudgetFraction)))
	return m.sessionPanelPlan(budget).totalRows
}

// panelBlockGeometry computes n blocks' two-row (or one-row, if truncated
// by area's bottom edge) on-screen rects, in order — no drawing, no *UI
// dependency, so it can be called both from a section's draw function
// (which paints from it) and, via sessionPanelRowLayout, from the
// click/hover hit-test, which must not wait for a Draw call to have
// populated anything. Used by the threads section,
// which use the identical two-line-block shape.
func panelBlockGeometry(area uv.Rectangle, n int) []uv.Rectangle {
	if area.Dy() <= 0 || area.Dx() <= 0 || n == 0 {
		return nil
	}

	row := area
	row.Max.Y = row.Min.Y + 1

	rects := make([]uv.Rectangle, 0, n)
	for range n {
		if row.Min.Y >= area.Max.Y {
			break
		}
		block := row
		block.Max.Y = min(block.Min.Y+2, area.Max.Y)
		rects = append(rects, block)

		row.Min.Y += 2
		row.Max.Y = row.Min.Y
	}

	return rects
}

// panelRowLayout is where every hit-testable row of the session panel
// lands for one (area, plan) pair. Returned as a struct rather than the
// five loose rectangles this used to be: the sections that can carry blocks
// come in pairs (header, blocks) and grow, and a positional return list is
// exactly the shape that quietly transposes two rects the day a section is
// added between two others.
type panelRowLayout struct {
	threadsHeader uv.Rectangle
	threadBlocks  []uv.Rectangle
	agentsHeader  uv.Rectangle
	agentBlocks   []uv.Rectangle
	todosHeader   uv.Rectangle
	todosList     uv.Rectangle
}

// sessionPanelRowLayout is the single source of truth for where each
// hit-testable row of the session panel lands, given an area and a plan —
// pure geometry, no drawing. Both drawSessionPanel (which paints from it)
// and the tea.MouseClickMsg/tea.MouseMotionMsg/CoalescedWheelMsg handlers
// in ui.go (which need it computed fresh, synchronously, at click/hover
// time — not lagged behind whatever the last Draw call happened to cache)
// call this instead of duplicating the row-advancing math. todosListRect is
// the area of the (possibly scrollable) todo rows below the header, for the
// mouse-wheel hit-test that adjusts m.panel.todosScrollOffset.
func sessionPanelRowLayout(area uv.Rectangle, plan sessionPanelPlan) (layout panelRowLayout) {
	if area.Dy() <= 0 || area.Dx() <= 0 {
		return panelRowLayout{}
	}

	row := area
	row.Max.Y = row.Min.Y

	// The header is laid out independently of the blocks: a collapsed
	// section has no blocks but still owns its header row, and that row
	// is the only hit target left for expanding it again.
	if plan.agentsHeaderRows > 0 && row.Min.Y < area.Max.Y {
		layout.agentsHeader = row
		layout.agentsHeader.Min.Y = row.Min.Y
		layout.agentsHeader.Max.Y = min(row.Min.Y+1, area.Max.Y)
		row.Min.Y = min(row.Min.Y+plan.agentsHeaderRows, area.Max.Y)
		row.Max.Y = row.Min.Y
	}
	if plan.agentsRows > 0 && row.Min.Y < area.Max.Y {
		agentsArea := area
		agentsArea.Min.Y = row.Min.Y
		agentsArea.Max.Y = min(row.Min.Y+plan.agentsRows, area.Max.Y)
		layout.agentBlocks = panelBlockGeometry(agentsArea, len(plan.agents))
		row.Min.Y = agentsArea.Max.Y
		row.Max.Y = row.Min.Y
	}

	if plan.threadsHeaderRows > 0 && row.Min.Y < area.Max.Y {
		layout.threadsHeader = row
		layout.threadsHeader.Min.Y = row.Min.Y
		layout.threadsHeader.Max.Y = min(row.Min.Y+1, area.Max.Y)
		row.Min.Y = min(row.Min.Y+plan.threadsHeaderRows, area.Max.Y)
		row.Max.Y = row.Min.Y
	}
	if plan.threadsRows > 0 && row.Min.Y < area.Max.Y {
		threadsArea := area
		threadsArea.Min.Y = row.Min.Y
		threadsArea.Max.Y = min(row.Min.Y+plan.threadsRows, area.Max.Y)
		layout.threadBlocks = panelBlockGeometry(threadsArea, len(plan.threads))
		row.Min.Y = threadsArea.Max.Y
		row.Max.Y = row.Min.Y
	}

	if plan.todosVisible && row.Min.Y < area.Max.Y {
		layout.todosHeader = row
		layout.todosHeader.Max.Y = layout.todosHeader.Min.Y + 1

		layout.todosList = area
		layout.todosList.Min.Y = layout.todosHeader.Max.Y
		layout.todosList.Max.Y = min(layout.todosList.Min.Y+plan.todosViewportRows, area.Max.Y)
	}

	return layout
}

// panelHit is what panelHitTest reports for a screen point against the
// current session panel layout: which row (if any) the point lands on,
// plus the plan it was computed from so callers can read thread identity
// (plan.threads[ThreadIndex]) or todosScrollable without a second call.
type panelHit struct {
	plan            sessionPanelPlan
	inTodosHeader   bool
	inThreadsHeader bool
	inAgentsHeader  bool
	inTodosList     bool
	// threadIndex is -1 when pt isn't over a thread block.
	threadIndex int
	// agentIndex is -1 when pt isn't over a delegation block.
	agentIndex int
}

// panelHitTest computes the session panel's plan and row layout once and
// hit-tests pt against it. tea.MouseClickMsg, tea.MouseMotionMsg, and
// CoalescedWheelMsg each need this same plan+rects pair — for a click
// action, a hover highlight, and a wheel-scroll target respectively — so
// this is the single place that pairs sessionPanelPlan with
// sessionPanelRowLayout for mouse handling, instead of each handler
// re-deriving both. It must be called fresh at event time, not cached
// across events: like sessionPanelRowLayout's callers, a mouse event can
// arrive before drawSessionPanel has painted the current layout.
func (m *UI) panelHitTest(pt image.Point) panelHit {
	plan := m.sessionPanelPlan(m.lay.layout.panel.Dy())
	layout := sessionPanelRowLayout(m.lay.layout.panel, plan)

	hit := panelHit{
		plan:            plan,
		inTodosHeader:   pt.In(layout.todosHeader),
		inThreadsHeader: pt.In(layout.threadsHeader),
		inAgentsHeader:  pt.In(layout.agentsHeader),
		inTodosList:     pt.In(layout.todosList),
		threadIndex:     -1,
		agentIndex:      -1,
	}
	for i, rect := range layout.threadBlocks {
		if pt.In(rect) {
			hit.threadIndex = i
			break
		}
	}
	for i, rect := range layout.agentBlocks {
		if pt.In(rect) {
			hit.agentIndex = i
			break
		}
	}
	return hit
}

// panelBlockDrawSpec supplies the context-specific content for the shared
// two-line thread block drawing path. Geometry, hover treatment,
// truncation, and footer placement stay identical for both sections.
type panelBlockDrawSpec struct {
	count  int
	more   int
	footer string
	name   func(index int) string
	task   func(index int) string
	line2  func(index int) string
}

// drawPanelBlocks paints two-line panel blocks and returns their hit-test
// rectangles. Callers provide only their distinct name/task/status content.
func (m *UI) drawPanelBlocks(scr uv.Screen, area uv.Rectangle, hoveredIdx int, spec panelBlockDrawSpec) []uv.Rectangle {
	rects := panelBlockGeometry(area, spec.count)
	if len(rects) == 0 {
		return nil
	}

	sty := &m.com.Styles.ChildBanner
	width := area.Dx()
	for i, block := range rects {
		line1Row := uv.Rectangle{Min: uv.Position{X: area.Min.X, Y: block.Min.Y}, Max: uv.Position{X: area.Max.X, Y: block.Min.Y + 1}}
		nameStyle := sty.Current
		if i == hoveredIdx {
			nameStyle = nameStyle.Underline(true)
		}
		styled := sty.Base.Render(fmt.Sprintf("%d ", i+1)) +
			nameStyle.Render(spec.name(i)) +
			sty.Base.Render(" — "+spec.task(i))
		styled = ansi.Truncate(styled, width, "…")
		if i == hoveredIdx {
			styled = common.BlockBackground(styled, width, m.com.Styles.Tool.ClickableHoverBg)
		}
		uv.NewStyledString(styled).Draw(scr, line1Row)

		if block.Max.Y-block.Min.Y < 2 {
			continue
		}
		line2Row := uv.Rectangle{Min: uv.Position{X: area.Min.X, Y: block.Min.Y + 1}, Max: uv.Position{X: area.Max.X, Y: block.Min.Y + 2}}
		line2 := ansi.Truncate(spec.line2(i), width, "…")
		if i == hoveredIdx {
			line2 = common.BlockBackground(line2, width, m.com.Styles.Tool.ClickableHoverBg)
		}
		uv.NewStyledString(line2).Draw(scr, line2Row)
	}

	if spec.more > 0 {
		footerY := rects[len(rects)-1].Max.Y
		if footerY < area.Max.Y {
			footerRow := uv.Rectangle{Min: uv.Position{X: area.Min.X, Y: footerY}, Max: uv.Position{X: area.Max.X, Y: footerY + 1}}
			uv.NewStyledString(sty.Base.Render(ansi.Truncate(fmt.Sprintf(spec.footer, spec.more), width, "…"))).Draw(scr, footerRow)
		}
	}
	return rects
}

// sessionPanelTodosContent concatenates plan.todosInProgress, todosPending,
// and todosDone in that fixed order — the same ordering the panel has
// always drawn them in (see splitTodosByStatus), just materialized as one
// slice so drawSessionPanel and the scroll-offset math can window it
// without re-deriving the concatenation in two places.
func sessionPanelTodosContent(plan sessionPanelPlan) []session.Todo {
	all := make([]session.Todo, 0, plan.todosContentRows)
	all = append(all, plan.todosInProgress...)
	all = append(all, plan.todosPending...)
	all = append(all, plan.todosDone...)
	return all
}

// clampPanelTodosScrollOffset clamps offset to [0, max(0,
// contentRows-viewportRows)] — the range sessionPanelPlan's todosContentRows
// and todosViewportRows report for the current render. Shared by
// drawSessionPanel (which self-corrects m.panel.todosScrollOffset every
// frame — the same "cosmetic state that tolerates staleness" pattern the
// panel already uses for hover rects, so a stale offset left over from a
// shorter list never dangles past the new bounds) and the mouse-wheel
// handler in ui.go.
func clampPanelTodosScrollOffset(offset int, plan sessionPanelPlan) int {
	maxOffset := max(0, plan.todosContentRows-plan.todosViewportRows)
	return max(0, min(offset, maxOffset))
}

// drawSessionPanel paints the merged panel: threads blocks, then the todos
// header (plus list when expanded and budget allows), then the queue tail
// — all from sessionPanelPlan(area.Dy()), so it never paints a different
// row count than sessionPanelHeight reserved for this exact area. Also
// caches m.panel.threadRects/m.panel.threads/m.panel.todosHeaderRect for
// hover-highlight rendering (block-name underline, todosHover's
// style swap) — cosmetic state that can tolerate one frame of staleness.
// The click hit-test itself does NOT read these fields: it recomputes the
// same rects on demand from sessionPanelRowLayout, since a click can arrive
// before this function has ever run for the current layout (see ui.go's
// tea.MouseClickMsg handler).
func (m *UI) drawSessionPanel(scr uv.Screen, area uv.Rectangle) {
	m.panel.threadRects = nil
	m.panel.threads = nil
	m.panel.agentRects = nil
	m.panel.agents = nil
	m.panel.todosHeaderRect = uv.Rectangle{}
	m.panel.threadsHeaderRect = uv.Rectangle{}
	m.panel.agentsHeaderRect = uv.Rectangle{}
	m.panel.todosListRect = uv.Rectangle{}
	if area.Dy() <= 0 || area.Dx() <= 0 {
		return
	}

	plan := m.sessionPanelPlan(area.Dy())
	t := m.com.Styles
	width := area.Dx()
	layout := sessionPanelRowLayout(area, plan)
	row := area
	row.Max.Y = row.Min.Y

	if plan.agentsHeaderRows > 0 || plan.agentsRows > 0 {
		if plan.agentsHeaderRows > 0 {
			headerView := common.SectionStyled(t, t.Section.Title,
				sessionPanelAgentsHeaderText(plan.agentsLive, plan.agentsExpanded), width)
			if m.panel.agentsHover {
				headerView = common.BlockBackground(headerView, width, t.Tool.ClickableHoverBg)
			}
			headerRow := area
			headerRow.Min.Y = row.Min.Y
			headerRow.Max.Y = row.Min.Y + 1
			uv.NewStyledString(headerView).Draw(scr, headerRow)
			m.panel.agentsHeaderRect = headerRow
			row.Min.Y++
			row.Max.Y = row.Min.Y
		}
		agentsArea := area
		agentsArea.Min.Y = row.Min.Y
		agentsArea.Max.Y = min(row.Min.Y+plan.agentsRows, area.Max.Y)
		m.panel.agentRects = m.drawPanelBlocks(scr, agentsArea, m.panel.hoveredAgent, panelBlockDrawSpec{
			count: len(plan.agents), more: plan.agentsMore, footer: "…and %d more agents",
			name: func(i int) string { return m.delegationBlockName(plan.agents[i]) },
			task: func(i int) string { return threadDockGoalFirstLine(plan.agents[i].Goal) },
			line2: func(i int) string {
				item := plan.agents[i]
				icon := m.com.Styles.ChildBanner.Base.Render("→")
				if proto.ThreadStatus(item.Status) == proto.ThreadStatusRunning {
					icon = m.panelActivityIcon()
				}
				return "  " + icon + " " + m.com.Styles.ChildBanner.Base.Render(delegationBlockStatusText(item))
			},
		})
		m.panel.agents = plan.agents
		row.Min.Y = agentsArea.Max.Y
		row.Max.Y = row.Min.Y
	}

	if plan.threadsHeaderRows > 0 || plan.threadsRows > 0 {
		if plan.threadsHeaderRows > 0 {
			// Styled and hover-lit like the todos header rather than a
			// plain separator: this row is clickable now, and a row that
			// responds to a click has to look like it does.
			headerView := common.SectionStyled(t, t.Section.Title,
				sessionPanelThreadsHeaderText(plan.threadsActive, plan.threadsExpanded), width)
			if m.panel.threadsHover {
				headerView = common.BlockBackground(headerView, width, t.Tool.ClickableHoverBg)
			}
			headerRow := area
			headerRow.Min.Y = row.Min.Y
			headerRow.Max.Y = row.Min.Y + 1
			uv.NewStyledString(headerView).Draw(scr, headerRow)
			m.panel.threadsHeaderRect = headerRow
			row.Min.Y++
			row.Max.Y = row.Min.Y
		}
		threadsArea := area
		threadsArea.Min.Y = row.Min.Y
		threadsArea.Max.Y = min(row.Min.Y+plan.threadsRows, area.Max.Y)
		m.panel.threadRects = m.drawPanelBlocks(scr, threadsArea, m.panel.hoveredThread, panelBlockDrawSpec{
			count: len(plan.threads), more: plan.threadsMore, footer: "…and %d more threads",
			name: func(i int) string {
				item := plan.threads[i]
				name := item.Name
				if name == "" {
					name = item.ID
				}
				// Tasks share this block shape with threads but have no
				// worktree/branch of their own — tag them so a user can
				// tell the two apart without opening the dashboard. Threads
				// stay unlabeled since they're the common case.
				if proto.ThreadKind(item.Kind) == proto.ThreadKindTask {
					return "[task] " + name
				}
				return name
			},
			task: func(i int) string { return threadDockGoalFirstLine(plan.threads[i].Goal) },
			line2: func(i int) string {
				item := plan.threads[i]
				icon := m.com.Styles.ChildBanner.Base.Render("→")
				if status := proto.ThreadStatus(item.Status); status == proto.ThreadStatusRunning || status == proto.ThreadStatusMerging {
					icon = m.panelActivityIcon()
				}
				return "  " + icon + " " + m.com.Styles.ChildBanner.Base.Render(threadDockStatusText(item, m.threadsDock.activity[item.ID].value))
			},
		})
		m.panel.threads = plan.threads
		row.Min.Y = threadsArea.Max.Y
		row.Max.Y = row.Min.Y
	}

	if plan.todosVisible && row.Min.Y < area.Max.Y {
		headerRow := layout.todosHeader
		header := sessionPanelTodosHeaderText(plan.todosCompleted, plan.todosTotal, plan.todosExpanded)
		// The todos header is the same section-separator style as
		// threads/agents/queue. Its full row is clickable, so hover paints the
		// full row rather than only changing the title.
		titleStyle := t.Section.Title
		headerView := common.SectionStyled(t, titleStyle, header, width)
		if m.panel.todosHover {
			headerView = common.BlockBackground(headerView, width, t.Tool.ClickableHoverBg)
		}
		uv.NewStyledString(headerView).Draw(scr, headerRow)
		m.panel.todosHeaderRect = headerRow
		m.panel.todosListRect = layout.todosList
		row.Min.Y++
		row.Max.Y = row.Min.Y

		// The section's own row list — in-progress, then pending, then
		// completed (sessionPanelTodosContent) — windowed by the scroll
		// offset rather than truncated: no todo is ever silently
		// unreachable, only scrolled out of the currently-painted rows.
		// Collapsed mode is never scrollable (todosViewportRows ==
		// todosContentRows == 0 there), so offset is always 0 and this
		// loop paints nothing — the header stands alone.
		offset := clampPanelTodosScrollOffset(m.panel.todosScrollOffset, plan)
		m.panel.todosScrollOffset = offset
		all := sessionPanelTodosContent(plan)
		end := min(len(all), offset+plan.todosViewportRows)

		listWidth := width
		if plan.todosScrollable {
			listWidth = max(0, width-1) // reserve the right column for the scrollbar.
		}

		inProgressIcon := m.panelActivityIcon()
		for _, todo := range all[offset:end] {
			if row.Min.Y >= area.Max.Y {
				break
			}
			listRow := uv.Rectangle{Min: area.Min, Max: uv.Position{X: area.Min.X + listWidth, Y: area.Max.Y}}
			row.Min.Y = drawPanelLine(scr, listRow, row.Min.Y, renderSessionTodoLine(todo, inProgressIcon, t, listWidth))
		}

		if plan.todosScrollable {
			sb := common.Scrollbar(t, plan.todosViewportRows, plan.todosContentRows, plan.todosViewportRows, offset)
			if sb != "" {
				scrollbarArea := uv.Rectangle{
					Min: uv.Position{X: area.Max.X - 1, Y: layout.todosList.Min.Y},
					Max: uv.Position{X: area.Max.X, Y: layout.todosList.Min.Y + plan.todosViewportRows},
				}
				uv.NewStyledString(sb).Draw(scr, scrollbarArea)
			}
		}
	}

	if len(plan.queue) > 0 && row.Min.Y < area.Max.Y {
		if plan.queueHeaderRows > 0 {
			row.Min.Y = drawPanelLine(scr, area, row.Min.Y, common.Section(t, "queue", width))
		}
		for _, line := range renderSessionQueueLines(plan.queue, t, width) {
			if row.Min.Y >= area.Max.Y {
				break
			}
			row.Min.Y = drawPanelLine(scr, area, row.Min.Y, line)
		}
	}
}

// drawPanelLine draws one already-styled, already-truncated line at row y
// (spanning area's full width) and returns y+1 — a one-row-tall rect built
// fresh each call, since a zero-height uv.Rectangle silently draws nothing.
func drawPanelLine(scr uv.Screen, area uv.Rectangle, y int, line string) int {
	row := uv.Rectangle{
		Min: uv.Position{X: area.Min.X, Y: y},
		Max: uv.Position{X: area.Max.X, Y: y + 1},
	}
	uv.NewStyledString(line).Draw(scr, row)
	return y + 1
}

// sessionPanelHeightReasonableTerminalHeight is the minimum terminal height
// at which we auto-expand the todos section on load.
const sessionPanelHeightReasonableTerminalHeight = 40

// autoExpandTodosIfReasonable expands the todos section if the terminal has
// enough vertical space to show the expanded list comfortably and there are
// incomplete todos. Unlike the old autoExpandPillsIfReasonable, the queue is
// unconditionally always visible now, so it's no longer a reason to expand
// anything.
//
//nolint:unparam // always nil today, but keeps the tea.Cmd signature shared with the other panel handlers callers check for a non-nil cmd
func (m *UI) autoExpandTodosIfReasonable() tea.Cmd {
	if !m.hasSession() {
		return nil
	}
	if m.activeInline != nil {
		return nil
	}
	if m.lay.height < sessionPanelHeightReasonableTerminalHeight {
		return nil
	}
	if !hasIncompleteTodos(m.sess.current.Todos) {
		return nil
	}
	if m.panel.expanded || m.panel.autoExpanded {
		return nil
	}
	m.panel.expanded = true
	m.panel.autoExpanded = true
	m.updateLayoutAndSize()
	if m.chat.Follow() {
		m.chat.ScrollToBottom()
	}
	return nil
}

// toggleTodosExpanded toggles the todos section's expand state (ctrl+t /
// ctrl+space, and a click on the todos header row).
//
//nolint:unparam // always nil today, but keeps the tea.Cmd signature shared with the other panel handlers callers check for a non-nil cmd
func (m *UI) toggleTodosExpanded() tea.Cmd {
	if !m.hasSession() || !hasIncompleteTodos(m.sess.current.Todos) {
		return nil
	}
	m.panel.expanded = !m.panel.expanded
	// A fresh expand always starts scrolled to the top (in-progress rows
	// first) rather than resuming wherever a previous expand left off,
	// which would be surprising after the list has likely changed shape.
	m.panel.todosScrollOffset = 0
	m.updateLayoutAndSize()

	// Follow scroll if enabled, same as the old togglePillsExpanded — this
	// is layout adjustment, not user-initiated scrolling, hence
	// ScrollToBottom (no scrollbar) rather than an animated scroll.
	if m.chat.Follow() {
		m.chat.ScrollToBottom()
	}
	return nil
}

// toggleThreadsCollapsed flips the threads section between showing its
// blocks and showing only its header. It mirrors toggleTodosExpanded, but
// needs no relayout command of its own: the row budget is recomputed from
// the plan on the next draw, and the plan reads this flag directly.
func (m *UI) toggleThreadsCollapsed() {
	m.panel.threadsCollapsed = !m.panel.threadsCollapsed
}

// toggleAgentsCollapsed is toggleThreadsCollapsed for the agents section.
func (m *UI) toggleAgentsCollapsed() {
	m.panel.agentsCollapsed = !m.panel.agentsCollapsed
}

// enterDelegation opens a delegation's own transcript from its panel
// block. The child session is named for the tool call that started it (see
// delegationSessionID in internal/agent), so the nav machinery the
// transcript's own drill-in uses works unchanged here — enterChildSession
// wants that pair, not a session id.
//
// A delegation whose session id does not parse (one created without a tool
// call identity to carry) has nothing to push a nav frame for; say so
// rather than opening an empty child view.
func (m *UI) enterDelegation(t proto.Thread) tea.Cmd {
	if m.com == nil || m.com.Workspace == nil {
		return nil
	}
	messageID, toolCallID, ok := m.com.Workspace.ParseAgentToolSessionID(t.SessionID)
	if !ok {
		return util.ReportWarn("This delegation has no transcript to open")
	}
	return m.enterChildSession(messageID, toolCallID)
}
