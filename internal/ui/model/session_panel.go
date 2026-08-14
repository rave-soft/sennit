package model

// The session panel is the merged strip between chat and the editor that
// replaces the old, separately-wired "pills" (todo progress + queued-prompt
// pills) and "threads dock" (active background threads). It paints, top to
// bottom: up to threadsDockVisibleCap active-thread blocks, up to
// delegationsVisibleCap running-delegation blocks (agent/agentic_fetch —
// the same two-line block shape as threads), a collapsible todos section,
// and an always-visible queued-prompts list.
//
// sessionPanelPlan is the single source of truth for how many rows each
// section gets and what it contains. sessionPanelHeight and drawSessionPanel
// both call it with the same row budget, so the space generateLayout
// reserves and what drawSessionPanel paints can never disagree — unlike the
// old pills.go/threads_dock_view.go split, where height and content were
// computed independently.

import (
	"encoding/json"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/thread"
	"github.com/rave-soft/braid/internal/ui/chat"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/presentation"
	"github.com/rave-soft/braid/internal/ui/styles"
)

// threadDockStatusText builds one thread's live status text for its
// block's second row: threadDockStatusLine over the thread's cached
// activity, with the elapsed time measured from CreatedAt — not UpdatedAt,
// which is bumped on every status transition (see thread/store.go
// SetStatus), so it tracks "last activity", not "how long has this thread
// been running"; the panel wants the latter.
func threadDockStatusText(t proto.Thread, activity threadDockActivity) string {
	elapsed := time.Since(time.Unix(t.CreatedAt, 0))
	return threadDockStatusLine(thread.Status(t.Status), activity, elapsed)
}

// panelSpinnerWanted reports whether the panel currently shows any live
// work worth animating: an in-progress todo while the local agent is busy
// (the original todo-spinner condition), a running delegation block, or
// an active background thread. The spinner used to belong to the todos
// list alone; the thread/delegation blocks now share it, so it must keep
// ticking even when the local agent is idle and only background threads
// are working.
func (m *UI) panelSpinnerWanted() bool {
	if !m.hasSession() {
		return false
	}
	if m.isAgentBusy() && hasInProgressTodo(m.session.Todos) {
		return true
	}
	if m.chat != nil && len(m.chat.RunningDelegations()) > 0 {
		return true
	}
	for _, t := range m.threadsDock.cache.value {
		switch thread.Status(t.Status) {
		case thread.StatusRunning, thread.StatusMerging:
			return true
		}
	}
	return false
}

// syncPanelSpinner reconciles the shared panel spinner with
// panelSpinnerWanted: on a stopped→spinning transition it returns the
// initial Tick command that starts the tick loop (nil otherwise), and it
// stops the spinner the moment nothing live is left. Every event that can
// change the answer (busy refresh landing, session/thread events, dock
// refreshes) funnels through this instead of duplicating the condition.
func (m *UI) syncPanelSpinner() tea.Cmd {
	want := m.panelSpinnerWanted()
	if want && !m.panelIsSpinning {
		m.panelIsSpinning = true
		return m.panelSpinner.Tick
	}
	if !want {
		m.panelIsSpinning = false
	}
	return nil
}

// panelActivityIcon is the leading marker for a live block's status line:
// the shared panel spinner's current frame while spinning (the block
// represents work in flight), a static spinner glyph otherwise — the same
// static-fallback the todos list uses, so a briefly-stalled tick loop
// degrades to a frozen glyph rather than a misleading arrow.
func (m *UI) panelActivityIcon() string {
	if m.panelIsSpinning {
		return m.panelSpinner.View()
	}
	return m.com.Styles.Tool.TodoInProgressIcon.Render(styles.SpinnerIcon)
}

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
	// way because its collapsed form still shows what is in progress,
	// whereas a collapsed threads section shows nothing but its count.
	threadsCollapsed bool
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

// delegationsVisibleCap caps the panel's delegations section the same way
// threadsDockVisibleCap caps threads: a handful of live blocks is useful,
// a long backlog just crowds out chat.
const delegationsVisibleCap = 3

// panelDelegation is a running delegation's identity, captured at plan
// time — enough for row math and the click/drill-in hit-test
// (messageID/toolCallID) without touching width-dependent text. Its
// display text (name/task/status line) is resolved from item at draw
// time, mirroring how thread blocks resolve their text from the raw
// proto.Thread only at draw time (see threadDockStatusText) — row counts
// must never depend on rendering width, only per-row truncation does.
type panelDelegation struct {
	item       chat.ToolMessageItem
	messageID  string
	toolCallID string
}

// runningDelegationBlocks enumerates the current session's running
// delegations for the panel, capped at delegationsVisibleCap, plus a count
// of how many more are running beyond the cap — mirroring
// visibleDockThreads. No IO: Chat.RunningDelegations reads the already-
// loaded chat items directly, the same live-pushed data
// (todos/tokens/nested-tool count) the transcript's own collapsed-
// delegation summary already uses.
func (m *UI) runningDelegationBlocks() ([]panelDelegation, int) {
	if !m.hasSession() {
		return nil, 0
	}
	items := m.chat.RunningDelegations()
	if len(items) == 0 {
		return nil, 0
	}
	more := 0
	if len(items) > delegationsVisibleCap {
		more = len(items) - delegationsVisibleCap
		items = items[:delegationsVisibleCap]
	}
	blocks := make([]panelDelegation, 0, len(items))
	for _, item := range items {
		blocks = append(blocks, panelDelegation{
			item:       item,
			messageID:  item.MessageID(),
			toolCallID: item.ToolCall().ID,
		})
	}
	return blocks, more
}

// delegationTaskParams is the minimal shape shared by agent.AgentParams and
// tools.AgenticFetchParams — both carry a "prompt" field describing the
// delegation's task, which is all the panel block's line 1 needs. Decoding
// it locally (rather than importing agent/tools types just for this)
// keeps the panel block agnostic to which delegation tool produced it.
type delegationTaskParams struct {
	Prompt string `json:"prompt"`
}

// delegationName resolves a delegation block's display name via
// chat.DelegationInfoProvider, falling back to a generic label on the
// (should-never-happen) chance an item doesn't implement it.
func delegationName(item chat.ToolMessageItem) string {
	if di, ok := item.(chat.DelegationInfoProvider); ok {
		name, _, _, _, _ := di.DelegationInfo()
		if name != "" {
			return name
		}
	}
	return "delegation"
}

// delegationTask extracts a delegation's task/prompt first line from its
// raw tool-call input.
func delegationTask(item chat.ToolMessageItem) string {
	var params delegationTaskParams
	_ = json.Unmarshal([]byte(item.ToolCall().Input), &params)
	return threadDockGoalFirstLine(params.Prompt)
}

// delegationStatusLine resolves a delegation's live status line (elapsed,
// step count, last tool, tokens) via chat.PanelLiveActivityProvider — the
// exact same text the transcript's own pending render used to show inline
// before the panel took over that job. "" if the item doesn't implement
// the interface or there's nothing to show yet.
func delegationStatusLine(item chat.ToolMessageItem, sty *styles.Styles, width int) string {
	if lp, ok := item.(chat.PanelLiveActivityProvider); ok {
		return lp.PanelStatusLine(sty, width)
	}
	return ""
}

// splitTodosByStatus splits todos into in-progress, pending, and completed,
// each preserving relative order. This is the opposite of
// chat.FormatTodosList's completed-first ordering: that function renders a
// single tool call's transcript, where reading top-down chronologically
// makes sense; the panel is a persistent status view, where what's
// happening now belongs above what's already done. The three-way split
// (rather than a merged "active" set) lets the panel keep in-progress rows
// visible even while collapsed, without leaking pending rows too — see
// sessionPanelPlan's todosInProgress/todosPending/todosDone.
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

// shedThreadBlocks drops whole two-row thread blocks until the plan fits
// the budget, returning the surviving blocks, the updated hidden count,
// and the rows they occupy. Blocks are dropped from the end, so the
// oldest (first-created) threads are the ones that stay visible, and
// every dropped block is added to the "…and N more" count rather than
// disappearing unannounced.
func shedThreadBlocks(threads []proto.Thread, more, over int) ([]proto.Thread, int, int) {
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

	// delegations are currently-running (no result yet) top-level
	// sub-agent tool calls in this session's own chat, rendered with the
	// same block shape as threads — see panelBlock/drawPanelBlocks.
	delegations     []panelDelegation
	delegationsMore int
	delegationsRows int
	// delegationsHeaderRows mirrors threadsHeaderRows for the "agents"
	// section-separator line.
	delegationsHeaderRows int

	todosVisible   bool // at least one incomplete todo exists
	todosExpanded  bool // m.panel.expanded, verbatim — budget shedding never forces this false anymore
	todosCompleted int  // for the header ratio — independent of what's dropped below
	todosTotal     int
	// todosInProgress is always shown, expanded or not — collapsing the
	// panel is never total: whatever is actively running right now stays
	// visible so a user never has to expand the panel just to check.
	todosInProgress []session.Todo
	todosPending    []session.Todo // populated only when todosExpanded; never shed by budget
	todosDone       []session.Todo // populated only when todosExpanded; never shed by budget

	// todosContentRows is the expanded todos section's full row count
	// (todosInProgress+todosPending+todosDone concatenated in that order,
	// or just todosInProgress while collapsed — collapsed never scrolls).
	// todosViewportRows is how many of those rows the budget actually
	// grants for painting; when it's less than todosContentRows the
	// section is scrollable (todosScrollable) and drawSessionPanel windows
	// the list by m.panelTodosScrollOffset instead of truncating it — see
	// the sessionPanelPlan doc comment.
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
// render a scrollbar and window the list by m.panelTodosScrollOffset
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
// Collapsed mode (todosExpanded == false) is unchanged: only the header and
// the always-visible in-progress rows count toward its content, and it
// never scrolls — see todosInProgress's doc comment.
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

	active := activeDockThreads(m.threadsDock.cache.value)
	plan.threadsActive = len(active)
	plan.threadsExpanded = !m.panel.threadsCollapsed
	if plan.threadsExpanded {
		visible, more := visibleDockThreads(active)
		plan.threads = visible
		plan.threadsMore = more
		plan.threadsRows = len(visible) * 2
		if more > 0 {
			plan.threadsRows++
		}
	}

	todos := m.session.Todos
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
		// Collapsing the panel is never total: whatever is actively in
		// progress right now stays visible whether expanded or not, so a
		// user never has to expand the panel just to see what's currently
		// running. Only pending and completed rows are gated on expanded,
		// and — unlike threads/queue — budget shedding below never clears
		// them; it only ever narrows the viewport that paints them.
		plan.todosInProgress = inProgress
		if plan.todosExpanded {
			plan.todosPending = pending
			plan.todosDone = completed
		}
		plan.todosContentRows = len(plan.todosInProgress) + len(plan.todosPending) + len(plan.todosDone)
		plan.todosViewportRows = plan.todosContentRows
	}

	delegations, delegationsMore := m.runningDelegationBlocks()
	plan.delegations = delegations
	plan.delegationsMore = delegationsMore
	plan.delegationsRows = len(delegations) * 2
	if delegationsMore > 0 {
		plan.delegationsRows++
	}

	plan.queue = m.wsCache.promptQueueItems

	headerRows := func() int {
		if !plan.todosVisible {
			return 0
		}
		return 1
	}
	// threadsHeaderRows/delegationsHeaderRows/queueHeaderRows are closures,
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
	delegationsHeaderRows := func() int {
		if plan.delegationsRows > 0 {
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
			delegationsHeaderRows() + plan.delegationsRows +
			headerRows() + plan.todosViewportRows +
			queueHeaderRows() + len(plan.queue) - budget
	}

	// Shedding priority, highest to lowest: threads > delegations >
	// todos-in-progress (never touched — see the floor below) > todos
	// pending/done (display-windowed via todosViewportRows, but never
	// dropped from the plan) > queue tail > delegations row budget >
	// threads row budget. The viewport never shrinks below
	// len(todosInProgress): the in-progress rows are what "collapsing is
	// never total" always guaranteed, and that guarantee carries over here
	// as "the default (unscrolled) viewport always shows every in-progress
	// row" — only pending/done ever have to be reached by scrolling.
	if plan.todosExpanded {
		if o := over(); o > 0 {
			floor := len(plan.todosInProgress)
			reducible := max(0, plan.todosViewportRows-floor)
			plan.todosViewportRows -= min(o, reducible)
		}
	}
	if o := over(); o > 0 {
		keep := max(0, len(plan.queue)-o)
		plan.queue = plan.queue[:keep]
	}
	if o := over(); o > 0 {
		plan.delegationsRows = max(0, plan.delegationsRows-o)
	}
	if o := over(); o > 0 {
		// Shed whole blocks, not rows. A thread block is two rows, so
		// subtracting an odd count used to leave half a block painted —
		// a name with no status line under it — and the hidden threads
		// vanished silently, with the "…and N more" footer still
		// reporting only what the visible cap had dropped.
		plan.threads, plan.threadsMore, plan.threadsRows = shedThreadBlocks(plan.threads, plan.threadsMore, o)
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
	plan.delegationsHeaderRows = delegationsHeaderRows()
	plan.queueHeaderRows = queueHeaderRows()
	plan.totalRows = plan.threadsHeaderRows + plan.threadsRows +
		plan.delegationsHeaderRows + plan.delegationsRows +
		headerRows() + plan.todosViewportRows +
		plan.queueHeaderRows + len(plan.queue)
	return plan
}

// sessionPanelHeight reports how many rows the merged session panel needs,
// capped at sessionPanelBudgetFraction of available — the space actually
// contested between chat and the panel (mainRect.Dy() at the call site in
// generateLayout), not the whole-terminal m.height. Budgeting off the full
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
// populated anything. Shared by the threads and delegations sections,
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

// threadBlockGeometry is panelBlockGeometry specialized to a thread slice
// — kept as a thin named wrapper since existing callers/tests spell it out
// by thread count implicitly via the slice.
func threadBlockGeometry(area uv.Rectangle, threads []proto.Thread) []uv.Rectangle {
	return panelBlockGeometry(area, len(threads))
}

// sessionPanelRowLayout is the single source of truth for where each
// hit-testable row of the session panel lands, given an area and a plan —
// pure geometry, no drawing. Both drawSessionPanel (which paints from it)
// and the tea.MouseClickMsg/tea.MouseMotionMsg/CoalescedWheelMsg handlers
// in ui.go (which need it computed fresh, synchronously, at click/hover
// time — not lagged behind whatever the last Draw call happened to cache)
// call this instead of duplicating the row-advancing math. todosListRect is
// the area of the (possibly scrollable) todo rows below the header, for the
// mouse-wheel hit-test that adjusts m.panelTodosScrollOffset.
func sessionPanelRowLayout(area uv.Rectangle, plan sessionPanelPlan) (threadBlockRects, delegationBlockRects []uv.Rectangle, todosHeaderRect, todosListRect, threadsHeaderRect uv.Rectangle) {
	if area.Dy() <= 0 || area.Dx() <= 0 {
		return nil, nil, uv.Rectangle{}, uv.Rectangle{}, uv.Rectangle{}
	}

	row := area
	row.Max.Y = row.Min.Y

	// The header is laid out independently of the blocks: a collapsed
	// section has no blocks but still owns its header row, and that row
	// is the only hit target left for expanding it again.
	if plan.threadsHeaderRows > 0 {
		threadsHeaderRect = row
		threadsHeaderRect.Min.Y = row.Min.Y
		threadsHeaderRect.Max.Y = min(row.Min.Y+1, area.Max.Y)
		row.Min.Y = min(row.Min.Y+plan.threadsHeaderRows, area.Max.Y)
		row.Max.Y = row.Min.Y
	}
	if plan.threadsRows > 0 {
		threadsArea := area
		threadsArea.Min.Y = row.Min.Y
		threadsArea.Max.Y = min(row.Min.Y+plan.threadsRows, area.Max.Y)
		threadBlockRects = threadBlockGeometry(threadsArea, plan.threads)
		row.Min.Y = threadsArea.Max.Y
		row.Max.Y = row.Min.Y
	}

	if plan.delegationsRows > 0 && row.Min.Y < area.Max.Y {
		row.Min.Y = min(row.Min.Y+plan.delegationsHeaderRows, area.Max.Y) // "agents" section-separator line, not hit-tested.
		delegationsArea := area
		delegationsArea.Min.Y = row.Min.Y
		delegationsArea.Max.Y = min(row.Min.Y+plan.delegationsRows, area.Max.Y)
		delegationBlockRects = panelBlockGeometry(delegationsArea, len(plan.delegations))
		row.Min.Y = delegationsArea.Max.Y
		row.Max.Y = row.Min.Y
	}

	if plan.todosVisible && row.Min.Y < area.Max.Y {
		todosHeaderRect = row
		todosHeaderRect.Max.Y = todosHeaderRect.Min.Y + 1

		todosListRect = area
		todosListRect.Min.Y = todosHeaderRect.Max.Y
		todosListRect.Max.Y = min(todosListRect.Min.Y+plan.todosViewportRows, area.Max.Y)
	}

	return threadBlockRects, delegationBlockRects, todosHeaderRect, todosListRect, threadsHeaderRect
}

// panelBlockDrawSpec supplies the context-specific content for the shared
// two-line thread/delegation drawing path. Geometry, hover treatment,
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
// drawSessionPanel (which self-corrects m.panelTodosScrollOffset every
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
// caches m.panelThreadRects/m.panelThreads/m.panelTodosHeaderRect for
// hover-highlight rendering (block-name underline, panelTodosHover's
// style swap) — cosmetic state that can tolerate one frame of staleness.
// The click hit-test itself does NOT read these fields: it recomputes the
// same rects on demand from sessionPanelRowLayout, since a click can arrive
// before this function has ever run for the current layout (see ui.go's
// tea.MouseClickMsg handler).
func (m *UI) drawSessionPanel(scr uv.Screen, area uv.Rectangle) {
	m.panelThreadRects = nil
	m.panelThreads = nil
	m.panelDelegationRects = nil
	m.panelDelegations = nil
	m.panelTodosHeaderRect = uv.Rectangle{}
	m.panelThreadsHeaderRect = uv.Rectangle{}
	m.panelTodosListRect = uv.Rectangle{}
	if area.Dy() <= 0 || area.Dx() <= 0 {
		return
	}

	plan := m.sessionPanelPlan(area.Dy())
	t := m.com.Styles
	width := area.Dx()
	_, _, todosHeaderRect, todosListRect, _ := sessionPanelRowLayout(area, plan)
	row := area
	row.Max.Y = row.Min.Y

	if plan.threadsHeaderRows > 0 || plan.threadsRows > 0 {
		if plan.threadsHeaderRows > 0 {
			// Styled and hover-lit like the todos header rather than a
			// plain separator: this row is clickable now, and a row that
			// responds to a click has to look like it does.
			headerView := common.SectionStyled(t, t.Section.Title,
				sessionPanelThreadsHeaderText(plan.threadsActive, plan.threadsExpanded), width)
			if m.panelThreadsHover {
				headerView = common.BlockBackground(headerView, width, t.Tool.ClickableHoverBg)
			}
			headerRow := area
			headerRow.Min.Y = row.Min.Y
			headerRow.Max.Y = row.Min.Y + 1
			uv.NewStyledString(headerView).Draw(scr, headerRow)
			m.panelThreadsHeaderRect = headerRow
			row.Min.Y++
			row.Max.Y = row.Min.Y
		}
		threadsArea := area
		threadsArea.Min.Y = row.Min.Y
		threadsArea.Max.Y = min(row.Min.Y+plan.threadsRows, area.Max.Y)
		m.panelThreadRects = m.drawPanelBlocks(scr, threadsArea, m.hoveredPanelThread, panelBlockDrawSpec{
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
				if thread.Kind(item.Kind) == thread.KindTask {
					return "[task] " + name
				}
				return name
			},
			task: func(i int) string { return threadDockGoalFirstLine(plan.threads[i].Goal) },
			line2: func(i int) string {
				item := plan.threads[i]
				icon := m.com.Styles.ChildBanner.Base.Render("→")
				if status := thread.Status(item.Status); status == thread.StatusRunning || status == thread.StatusMerging {
					icon = m.panelActivityIcon()
				}
				return "  " + icon + " " + m.com.Styles.ChildBanner.Base.Render(threadDockStatusText(item, m.threadsDock.activity[item.ID].value))
			},
		})
		m.panelThreads = plan.threads
		row.Min.Y = threadsArea.Max.Y
		row.Max.Y = row.Min.Y
	}

	if plan.delegationsRows > 0 && row.Min.Y < area.Max.Y {
		if plan.delegationsHeaderRows > 0 {
			row.Min.Y = drawPanelLine(scr, area, row.Min.Y, common.Section(t, "agents", width))
			row.Max.Y = row.Min.Y
		}
		delegationsArea := area
		delegationsArea.Min.Y = row.Min.Y
		delegationsArea.Max.Y = min(row.Min.Y+plan.delegationsRows, area.Max.Y)
		m.panelDelegationRects = m.drawPanelBlocks(scr, delegationsArea, m.hoveredPanelDelegation, panelBlockDrawSpec{
			count: len(plan.delegations), more: plan.delegationsMore, footer: "…and %d more delegations",
			name: func(i int) string { return delegationName(plan.delegations[i].item) },
			task: func(i int) string { return delegationTask(plan.delegations[i].item) },
			line2: func(i int) string {
				return "  " + m.panelActivityIcon() + " " + delegationStatusLine(plan.delegations[i].item, m.com.Styles, width-4)
			},
		})
		m.panelDelegations = plan.delegations
		row.Min.Y = delegationsArea.Max.Y
		row.Max.Y = row.Min.Y
	}

	if plan.todosVisible && row.Min.Y < area.Max.Y {
		headerRow := todosHeaderRect
		header := sessionPanelTodosHeaderText(plan.todosCompleted, plan.todosTotal, plan.todosExpanded)
		// The todos header is the same section-separator style as
		// threads/agents/queue. Its full row is clickable, so hover paints the
		// full row rather than only changing the title.
		titleStyle := t.Section.Title
		headerView := common.SectionStyled(t, titleStyle, header, width)
		if m.panelTodosHover {
			headerView = common.BlockBackground(headerView, width, t.Tool.ClickableHoverBg)
		}
		uv.NewStyledString(headerView).Draw(scr, headerRow)
		m.panelTodosHeaderRect = headerRow
		m.panelTodosListRect = todosListRect
		row.Min.Y++
		row.Max.Y = row.Min.Y

		// The section's own row list — in-progress, then pending, then
		// completed (sessionPanelTodosContent) — windowed by the scroll
		// offset rather than truncated: no todo is ever silently
		// unreachable, only scrolled out of the currently-painted rows.
		// Collapsed mode is never scrollable (todosViewportRows ==
		// todosContentRows == len(todosInProgress) there), so offset is
		// always 0 and this paints exactly what it always has.
		offset := clampPanelTodosScrollOffset(m.panelTodosScrollOffset, plan)
		m.panelTodosScrollOffset = offset
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
					Min: uv.Position{X: area.Max.X - 1, Y: todosListRect.Min.Y},
					Max: uv.Position{X: area.Max.X, Y: todosListRect.Min.Y + plan.todosViewportRows},
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
	if m.height < sessionPanelHeightReasonableTerminalHeight {
		return nil
	}
	if !hasIncompleteTodos(m.session.Todos) {
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
	if !m.hasSession() || !hasIncompleteTodos(m.session.Todos) {
		return nil
	}
	m.panel.expanded = !m.panel.expanded
	// A fresh expand always starts scrolled to the top (in-progress rows
	// first) rather than resuming wherever a previous expand left off,
	// which would be surprising after the list has likely changed shape.
	m.panelTodosScrollOffset = 0
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
