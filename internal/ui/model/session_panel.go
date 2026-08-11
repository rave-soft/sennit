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
	"github.com/rave-soft/braid/internal/ui/styles"
)

// threadDockBlockLines builds one active thread's two-line plain-text
// block: "<n> <name> — <goal>" and "  → <status>", each independently
// truncated to width (no wrapping — the panel is a fixed-height area).
// index is 1-based, matching the visible order visibleDockThreads returns.
// Unchanged from the pre-merge threads_dock_view.go.
func threadDockBlockLines(index int, t proto.Thread, activity threadDockActivity, width int) (line1, line2 string) {
	name := t.Name
	if name == "" {
		name = t.ID
	}
	goal := threadDockGoalFirstLine(t.Goal)
	line1 = fmt.Sprintf("%d %s — %s", index, name, goal)

	// CreatedAt, not UpdatedAt: UpdatedAt is bumped on every status
	// transition (see thread/store.go SetStatus), so it tracks "last
	// activity", not "how long has this thread been running" — the panel
	// wants the latter.
	elapsed := time.Since(time.Unix(t.CreatedAt, 0))
	status := threadDockStatusLine(thread.Status(t.Status), activity, elapsed)
	line2 = "  → " + status

	return ansi.Truncate(line1, width, "…"), ansi.Truncate(line2, width, "…")
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
// proto.Thread only at draw time (see threadDockBlockLines) — row counts
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
	for _, t := range todos {
		switch t.Status {
		case session.TodoStatusCompleted:
			completed = append(completed, t)
		case session.TodoStatusInProgress:
			inProgress = append(inProgress, t)
		default:
			pending = append(pending, t)
		}
	}
	return inProgress, pending, completed
}

// renderSessionTodoLine renders one todo row for the panel's expanded todos
// list. Unlike chat.FormatTodosList — built for a single tool call's
// transcript, where at most one todo is ever in_progress at a time — this
// is called once per todo independently, so every concurrently in-progress
// todo gets its own icon. With parallel subagents/threads writing todos at
// once, more than one can be in_progress simultaneously; the old pills.go
// todoPill tracked only the first in-progress todo via a single
// `currentTodo` variable and silently dropped the rest.
func renderSessionTodoLine(t session.Todo, inProgressIcon string, sty *styles.Styles, width int) string {
	var prefix string
	textStyle := sty.Tool.TodoItem

	switch t.Status {
	case session.TodoStatusCompleted:
		prefix = sty.Tool.TodoCompletedIcon.Render(styles.TodoCompletedIcon) + " "
		// Muted + strikethrough: composed from the existing muted token
		// (TodoCurrentTask) rather than a new style field, per
		// internal/ui/AGENTS.md's "reuse tokens" guidance.
		textStyle = sty.Pills.TodoCurrentTask.Strikethrough(true)
	case session.TodoStatusInProgress:
		prefix = sty.Tool.TodoInProgressIcon.Render(inProgressIcon + " ")
	default:
		prefix = sty.Tool.TodoPendingIcon.Render(styles.TodoPendingIcon) + " "
	}

	text := t.Content
	if t.Status == session.TodoStatusInProgress && t.ActiveForm != "" {
		text = t.ActiveForm
	}
	line := prefix + textStyle.Render(text)
	return ansi.Truncate(line, width, "…")
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

	// delegations are currently-running (no result yet) top-level
	// sub-agent tool calls in this session's own chat, rendered with the
	// same block shape as threads — see panelBlock/drawPanelBlocks.
	delegations     []panelDelegation
	delegationsMore int
	delegationsRows int

	todosVisible   bool // at least one incomplete todo exists
	todosExpanded  bool // for *this* render; may be forced false to fit budget
	todosCompleted int  // for the header ratio — independent of what's dropped below
	todosTotal     int
	// todosInProgress is always shown, expanded or not — collapsing the
	// panel is never total: whatever is actively running right now stays
	// visible so a user never has to expand the panel just to check.
	todosInProgress []session.Todo
	todosPending    []session.Todo // shown only when todosExpanded
	todosDone       []session.Todo // shown when todosExpanded and budget allows

	queue []string

	totalRows int
}

// sessionPanelPlan computes what the panel shows for budget rows, shedding
// content in priority order when the natural size doesn't fit: active
// threads first (never shrink), then active (non-completed) todos, then the
// queue, then completed todos are the first to go. Concretely: (1) drop
// completed-todo rows from the expanded list — the header ratio already
// conveys the count; (2) if still over, collapse the todos list entirely
// for this render only, without touching m.panel.expanded; (3) if still
// over, truncate the queue tail; (4) if still over (pathological tiny
// terminal), shrink the threads section's row budget, mirroring the
// pre-existing `dockHeight = min(dockHeight, uiLayout.main.Dy())` clamp;
// (5) as a last-resort floor, drop the todos header too, so totalRows never
// exceeds budget even when budget itself is smaller than one row.
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

	active := activeDockThreads(m.threadsDock.threads)
	visible, more := visibleDockThreads(active)
	plan.threads = visible
	plan.threadsMore = more
	plan.threadsRows = len(visible) * 2
	if more > 0 {
		plan.threadsRows++
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
		// running. Only pending and completed rows are gated on expanded.
		plan.todosInProgress = inProgress
		if plan.todosExpanded {
			plan.todosPending = pending
			plan.todosDone = completed
		}
	}

	delegations, delegationsMore := m.runningDelegationBlocks()
	plan.delegations = delegations
	plan.delegationsMore = delegationsMore
	plan.delegationsRows = len(delegations) * 2
	if delegationsMore > 0 {
		plan.delegationsRows++
	}

	plan.queue = m.wsCache.promptQueueItems

	todosRows := func() int {
		if !plan.todosVisible {
			return 0
		}
		return 1 + len(plan.todosInProgress) + len(plan.todosPending) + len(plan.todosDone)
	}
	over := func() int {
		return plan.threadsRows + plan.delegationsRows + todosRows() + len(plan.queue) - budget
	}

	// Shedding priority, highest to lowest: threads > delegations >
	// todos-in-progress (never dropped while visible at all) > queue >
	// todos-done. todos-pending sheds together with the todos section
	// collapsing (step 2 below), same as before the in-progress/pending
	// split.
	if over() > 0 {
		plan.todosDone = nil
	}
	if over() > 0 {
		plan.todosExpanded = false
		plan.todosPending = nil
	}
	if o := over(); o > 0 {
		keep := max(0, len(plan.queue)-o)
		plan.queue = plan.queue[:keep]
	}
	if o := over(); o > 0 {
		plan.delegationsRows = max(0, plan.delegationsRows-o)
	}
	if o := over(); o > 0 {
		plan.threadsRows = max(0, plan.threadsRows-o)
	}
	if over() > 0 {
		// budget < 1: even the one-line todos header doesn't fit.
		plan.todosVisible = false
	}

	plan.totalRows = plan.threadsRows + plan.delegationsRows + todosRows() + len(plan.queue)
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
// which use the identical two-line-block shape — see threadBlockGeometry
// and drawDelegationBlocks.
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
// and the tea.MouseClickMsg/tea.MouseMotionMsg handlers in ui.go (which
// need it computed fresh, synchronously, at click/hover time — not lagged
// behind whatever the last Draw call happened to cache) call this instead
// of duplicating the row-advancing math.
func sessionPanelRowLayout(area uv.Rectangle, plan sessionPanelPlan) (threadBlockRects, delegationBlockRects []uv.Rectangle, todosHeaderRect uv.Rectangle) {
	if area.Dy() <= 0 || area.Dx() <= 0 {
		return nil, nil, uv.Rectangle{}
	}

	row := area
	row.Max.Y = row.Min.Y

	if plan.threadsRows > 0 {
		threadsArea := area
		threadsArea.Max.Y = min(area.Min.Y+plan.threadsRows, area.Max.Y)
		threadBlockRects = threadBlockGeometry(threadsArea, plan.threads)
		row.Min.Y = threadsArea.Max.Y
		row.Max.Y = row.Min.Y
	}

	if plan.delegationsRows > 0 && row.Min.Y < area.Max.Y {
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
	}

	return threadBlockRects, delegationBlockRects, todosHeaderRect
}

// drawThreadBlocks paints plan.threads as the panel's top section — one
// two-line block per active thread (text from threadDockBlockLines, same as
// the old drawThreadsDock) plus the "…and N more threads" footer — and
// returns each block's on-screen rect (from threadBlockGeometry), in the
// same order as plan.threads, for hover-highlight bookkeeping in Draw.
// hoveredIdx (-1 for none) highlights the block under the pointer.
func (m *UI) drawThreadBlocks(scr uv.Screen, area uv.Rectangle, plan sessionPanelPlan, hoveredIdx int) []uv.Rectangle {
	rects := threadBlockGeometry(area, plan.threads)
	if len(rects) == 0 {
		return nil
	}

	// Reuse ChildBanner's name/base styles rather than adding a new style
	// group, matching the old drawThreadsDock.
	sty := &m.com.Styles.ChildBanner
	width := area.Dx()

	for i, block := range rects {
		t := plan.threads[i]
		line1Row := uv.Rectangle{
			Min: uv.Position{X: area.Min.X, Y: block.Min.Y},
			Max: uv.Position{X: area.Max.X, Y: block.Min.Y + 1},
		}

		_, line2 := threadDockBlockLines(i+1, t, m.threadsDock.activity[t.ID], width)
		name := t.Name
		if name == "" {
			name = t.ID
		}
		goal := threadDockGoalFirstLine(t.Goal)

		nameStyle := sty.Current
		if i == hoveredIdx {
			nameStyle = nameStyle.Underline(true)
		}
		styled := sty.Base.Render(fmt.Sprintf("%d ", i+1)) +
			nameStyle.Render(name) +
			sty.Base.Render(" — "+goal)
		uv.NewStyledString(ansi.Truncate(styled, width, "…")).Draw(scr, line1Row)

		if block.Max.Y-block.Min.Y < 2 {
			continue // truncated by area's bottom edge: no room for line 2.
		}
		line2Row := uv.Rectangle{
			Min: uv.Position{X: area.Min.X, Y: block.Min.Y + 1},
			Max: uv.Position{X: area.Max.X, Y: block.Min.Y + 2},
		}
		uv.NewStyledString(sty.Base.Render(ansi.Truncate(line2, width, "…"))).Draw(scr, line2Row)
	}

	if plan.threadsMore > 0 {
		footerY := rects[len(rects)-1].Max.Y
		if footerY < area.Max.Y {
			footer := fmt.Sprintf("…and %d more threads", plan.threadsMore)
			footerRow := uv.Rectangle{
				Min: uv.Position{X: area.Min.X, Y: footerY},
				Max: uv.Position{X: area.Max.X, Y: footerY + 1},
			}
			uv.NewStyledString(sty.Base.Render(ansi.Truncate(footer, width, "…"))).Draw(scr, footerRow)
		}
	}

	return rects
}

// drawDelegationBlocks paints plan.delegations as the panel's second
// section (between threads and todos) — the EXACT SAME two-line block
// shape drawThreadBlocks uses (number/name bold — dash — task on line 1,
// live status on line 2), via the shared panelBlockGeometry, rather than a
// compressed one-liner: a running delegation is exactly as significant as
// a running thread. Returns each block's on-screen rect for hover-
// highlight bookkeeping in Draw, same contract as drawThreadBlocks.
// hoveredIdx (-1 for none) highlights the block under the pointer.
func (m *UI) drawDelegationBlocks(scr uv.Screen, area uv.Rectangle, plan sessionPanelPlan, hoveredIdx int) []uv.Rectangle {
	rects := panelBlockGeometry(area, len(plan.delegations))
	if len(rects) == 0 {
		return nil
	}

	// Reuse ChildBanner's name/base styles, matching drawThreadBlocks —
	// the whole point of the shared geometry is that these read as one
	// visual family, not two different widgets bolted together.
	sty := &m.com.Styles.ChildBanner
	width := area.Dx()

	for i, block := range rects {
		d := plan.delegations[i]
		line1Row := uv.Rectangle{
			Min: uv.Position{X: area.Min.X, Y: block.Min.Y},
			Max: uv.Position{X: area.Max.X, Y: block.Min.Y + 1},
		}

		nameStyle := sty.Current
		if i == hoveredIdx {
			nameStyle = nameStyle.Underline(true)
		}
		styled := sty.Base.Render(fmt.Sprintf("%d ", i+1)) +
			nameStyle.Render(delegationName(d.item)) +
			sty.Base.Render(" — "+delegationTask(d.item))
		uv.NewStyledString(ansi.Truncate(styled, width, "…")).Draw(scr, line1Row)

		if block.Max.Y-block.Min.Y < 2 {
			continue // truncated by area's bottom edge: no room for line 2.
		}
		line2Row := uv.Rectangle{
			Min: uv.Position{X: area.Min.X, Y: block.Min.Y + 1},
			Max: uv.Position{X: area.Max.X, Y: block.Min.Y + 2},
		}
		line2 := "  " + delegationStatusLine(d.item, m.com.Styles, width-2)
		uv.NewStyledString(ansi.Truncate(line2, width, "…")).Draw(scr, line2Row)
	}

	if plan.delegationsMore > 0 {
		footerY := rects[len(rects)-1].Max.Y
		if footerY < area.Max.Y {
			footer := fmt.Sprintf("…and %d more delegations", plan.delegationsMore)
			footerRow := uv.Rectangle{
				Min: uv.Position{X: area.Min.X, Y: footerY},
				Max: uv.Position{X: area.Max.X, Y: footerY + 1},
			}
			uv.NewStyledString(sty.Base.Render(ansi.Truncate(footer, width, "…"))).Draw(scr, footerRow)
		}
	}

	return rects
}

// drawSessionPanel paints the merged panel: threads blocks, then the todos
// header (plus list when expanded and budget allows), then the queue tail
// — all from sessionPanelPlan(area.Dy()), so it never paints a different
// row count than sessionPanelHeight reserved for this exact area. Also
// caches m.panelThreadRects/m.panelThreads/m.panelTodosHeaderRect for
// hover-highlight rendering (drawThreadBlocks' underline, panelTodosHover's
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
	if area.Dy() <= 0 || area.Dx() <= 0 {
		return
	}

	plan := m.sessionPanelPlan(area.Dy())
	t := m.com.Styles
	width := area.Dx()
	_, _, todosHeaderRect := sessionPanelRowLayout(area, plan)
	row := area
	row.Max.Y = row.Min.Y

	if plan.threadsRows > 0 {
		threadsArea := area
		threadsArea.Max.Y = min(area.Min.Y+plan.threadsRows, area.Max.Y)
		m.panelThreadRects = m.drawThreadBlocks(scr, threadsArea, plan, m.hoveredPanelThread)
		m.panelThreads = plan.threads
		row.Min.Y = threadsArea.Max.Y
		row.Max.Y = row.Min.Y
	}

	if plan.delegationsRows > 0 && row.Min.Y < area.Max.Y {
		delegationsArea := area
		delegationsArea.Min.Y = row.Min.Y
		delegationsArea.Max.Y = min(row.Min.Y+plan.delegationsRows, area.Max.Y)
		m.panelDelegationRects = m.drawDelegationBlocks(scr, delegationsArea, plan, m.hoveredPanelDelegation)
		m.panelDelegations = plan.delegations
		row.Min.Y = delegationsArea.Max.Y
		row.Max.Y = row.Min.Y
	}

	if plan.todosVisible && row.Min.Y < area.Max.Y {
		headerRow := todosHeaderRect
		header := sessionPanelTodosHeaderText(plan.todosCompleted, plan.todosTotal, plan.todosExpanded)
		headerStyle := t.Pills.TodoLabel
		if m.panelTodosHover {
			headerStyle = t.Pills.HeaderHover
		}
		uv.NewStyledString(headerStyle.Render(ansi.Truncate(header, width, "…"))).Draw(scr, headerRow)
		m.panelTodosHeaderRect = headerRow
		row.Min.Y++
		row.Max.Y = row.Min.Y

		// plan.todosInProgress/todosPending/todosDone are already the right
		// rows for the current state — the full in-progress+pending+
		// completed split when expanded, or just the always-visible
		// in-progress subset when collapsed (sessionPanelPlan's
		// "collapsing is never total" rule) — so draw them unconditionally
		// rather than re-gating on todosExpanded here.
		{
			inProgressIcon := t.Tool.TodoInProgressIcon.Render(styles.SpinnerIcon)
			if m.todoIsSpinning {
				inProgressIcon = m.todoSpinner.View()
			}
			for _, group := range [][]session.Todo{plan.todosInProgress, plan.todosPending, plan.todosDone} {
				for _, todo := range group {
					if row.Min.Y >= area.Max.Y {
						break
					}
					row.Min.Y = drawPanelLine(scr, area, row.Min.Y, renderSessionTodoLine(todo, inProgressIcon, t, width))
				}
			}
		}
	}

	for _, line := range renderSessionQueueLines(plan.queue, t, width) {
		if row.Min.Y >= area.Max.Y {
			break
		}
		row.Min.Y = drawPanelLine(scr, area, row.Min.Y, line)
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
	m.updateLayoutAndSize()

	// Follow scroll if enabled, same as the old togglePillsExpanded — this
	// is layout adjustment, not user-initiated scrolling, hence
	// ScrollToBottom (no scrollbar) rather than an animated scroll.
	if m.chat.Follow() {
		m.chat.ScrollToBottom()
	}
	return nil
}
