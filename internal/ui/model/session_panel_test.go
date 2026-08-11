package model

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/dialog"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// mkDockThreads builds n synthetic "running" threads, old enough to have a
// non-zero elapsed time, for exercising sessionPanelPlan/drawSessionPanel
// without a real workspace.
func mkDockThreads(n int) []proto.Thread {
	threads := make([]proto.Thread, n)
	now := time.Now()
	for i := range threads {
		threads[i] = proto.Thread{
			ID:        string(rune('a' + i)),
			Name:      "fix-auth",
			Goal:      "Refactor login flow to OAuth2",
			Status:    "running",
			CreatedAt: now.Add(-time.Duration(i+1) * time.Minute).Unix(),
		}
	}
	return threads
}

// TestThreadDockBlockLines covers the two-line block's plain text: the
// number, name, em dash, truncated goal on line 1, and the arrow-prefixed
// status on line 2 — plus that a narrow width truncates rather than
// wrapping or panicking. threadDockBlockLines itself lives in
// threads_dock_view.go's predecessor's file, threads_dock.go — untouched by
// the panel merge — this just re-confirms the panel still gets the same
// text out of it.
func TestThreadDockBlockLines(t *testing.T) {
	t.Parallel()

	th := proto.Thread{
		ID: "t1", Name: "fix-auth", Goal: "Refactor login flow to OAuth2",
		Status: "running", CreatedAt: time.Now().Add(-4 * time.Minute).Unix(),
	}
	activity := threadDockActivity{MessageCount: 12}

	line1, line2 := threadDockBlockLines(1, th, activity, 200)
	require.Equal(t, "1 fix-auth — Refactor login flow to OAuth2", line1)
	require.Contains(t, line2, "  → step 12 · ")

	// A narrow width truncates each line independently instead of
	// wrapping or panicking.
	require.NotPanics(t, func() {
		narrow1, narrow2 := threadDockBlockLines(1, th, activity, 10)
		require.LessOrEqual(t, ansi.StringWidth(narrow1), 10)
		require.LessOrEqual(t, ansi.StringWidth(narrow2), 10)
	})
}

// sessionUI builds a uiChat UI with an active session, ready to exercise
// the session panel without a real workspace.
func sessionUI() *UI {
	u := newTestUI()
	u.session = &session.Session{ID: "s1"}
	return u
}

// TestSessionPanelPlan_ThreadsRows covers the threads section's natural row
// budget: zero for no active threads, two rows per visible thread up to the
// cap, and one extra "more" row once the active set overflows
// threadsDockVisibleCap.
func TestSessionPanelPlan_ThreadsRows(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	require.Zero(t, u.sessionPanelPlan(100).threadsRows, "no threads at all")

	u.threadsDock.threads = []proto.Thread{{ID: "x", Status: "merged"}}
	require.Zero(t, u.sessionPanelPlan(100).threadsRows, "no active threads")

	for n := 1; n <= threadsDockVisibleCap; n++ {
		u.threadsDock.threads = mkDockThreads(n)
		require.Equal(t, n*2, u.sessionPanelPlan(100).threadsRows, "n=%d", n)
	}

	u.threadsDock.threads = mkDockThreads(threadsDockVisibleCap + 2)
	plan := u.sessionPanelPlan(100)
	require.Equal(t, threadsDockVisibleCap*2+1, plan.threadsRows,
		"overflow must add exactly one footer row")
	require.Len(t, plan.threads, threadsDockVisibleCap)
	require.Equal(t, 2, plan.threadsMore)
}

// TestDrawSessionPanel_RendersThreadBlocksAndMoreFooter covers end-to-end
// rendering: numbered blocks for the visible threads and the "…and N more
// threads" footer when the active set overflows the cap.
func TestDrawSessionPanel_RendersThreadBlocksAndMoreFooter(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.threadsDock.threads = mkDockThreads(threadsDockVisibleCap + 1)

	height := u.sessionPanelPlan(100).totalRows
	require.Equal(t, threadsDockVisibleCap*2+1, height)

	scr := uv.NewScreenBuffer(u.width, height)
	area := uv.Rectangle{Max: uv.Position{X: u.width, Y: height}}
	u.drawSessionPanel(scr, area)
	out := ansi.Strip(scr.Render())

	require.Contains(t, out, "1 fix-auth — Refactor login flow to OAuth2")
	require.Contains(t, out, "2 fix-auth — Refactor login flow to OAuth2")
	require.Contains(t, out, "3 fix-auth — Refactor login flow to OAuth2")
	require.Contains(t, out, "…and 1 more threads")
	require.Len(t, u.panelThreadRects, threadsDockVisibleCap)
	require.Len(t, u.panelThreads, threadsDockVisibleCap)
}

// TestDrawSessionPanel_NoOpOnZeroArea guards against a panic when the panel
// is given a degenerate (zero-height or zero-width) area.
func TestDrawSessionPanel_NoOpOnZeroArea(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.threadsDock.threads = mkDockThreads(1)

	scr := uv.NewScreenBuffer(u.width, u.height)
	require.NotPanics(t, func() {
		u.drawSessionPanel(scr, uv.Rectangle{})
	})
}

// TestSessionPanelPlan_TodosOrderingActiveFirstThenCompleted covers the
// expanded todos list ordering: in-progress, then pending, then completed
// last — the opposite of chat.FormatTodosList's completed-first ordering.
func TestSessionPanelPlan_TodosOrderingActiveFirstThenCompleted(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.panel.expanded = true
	u.session.Todos = []session.Todo{
		{Content: "done first", Status: session.TodoStatusCompleted},
		{Content: "pending one", Status: session.TodoStatusPending},
		{Content: "working now", Status: session.TodoStatusInProgress},
		{Content: "done second", Status: session.TodoStatusCompleted},
	}

	plan := u.sessionPanelPlan(100)
	require.True(t, plan.todosExpanded)
	require.Len(t, plan.todosInProgress, 1)
	require.Equal(t, "working now", plan.todosInProgress[0].Content, "in-progress must lead")
	require.Len(t, plan.todosPending, 1)
	require.Equal(t, "pending one", plan.todosPending[0].Content, "pending follows in-progress")
	require.Len(t, plan.todosDone, 2)
	require.Equal(t, "done first", plan.todosDone[0].Content)
	require.Equal(t, "done second", plan.todosDone[1].Content)
}

// TestRenderSessionTodoLine_CompletedIsMutedAndStrikethrough covers that a
// completed todo's rendered line carries the strikethrough SGR code.
func TestRenderSessionTodoLine_CompletedIsMutedAndStrikethrough(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	todo := session.Todo{Content: "done", Status: session.TodoStatusCompleted}
	line := renderSessionTodoLine(todo, "→", u.com.Styles, 80)

	require.Contains(t, line, ";9m", "expected a strikethrough SGR parameter in the rendered line")
	require.Contains(t, ansi.Strip(line), "done")
}

// TestSessionPanelPlan_AllInProgressTodosGetMarker is the regression test
// for the parallel-in-progress-todos bug: pills.go's old todoPill tracked
// only the first in_progress todo via a single `currentTodo` variable, so
// with parallel subagents/threads writing todos concurrently, any
// additional in-progress todo silently lost its marker. The new renderer
// iterates every todo independently, so each one gets its own icon.
func TestSessionPanelPlan_AllInProgressTodosGetMarker(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.panel.expanded = true
	u.session.Todos = []session.Todo{
		{Content: "first task", Status: session.TodoStatusInProgress},
		{Content: "second task", Status: session.TodoStatusInProgress},
	}

	plan := u.sessionPanelPlan(100)
	require.Len(t, plan.todosInProgress, 2)

	scr := uv.NewScreenBuffer(u.width, 10)
	area := uv.Rectangle{Max: uv.Position{X: u.width, Y: 10}}
	u.drawSessionPanel(scr, area)
	out := ansi.Strip(scr.Render())

	require.Contains(t, out, "first task")
	require.Contains(t, out, "second task")
	// Both rows must carry the in-progress marker (the spinner glyph while
	// not actively spinning), not just the first.
	require.Equal(t, 2, strings.Count(out, styles.SpinnerIcon), "every in-progress todo must get its own marker")
}

// TestSessionPanelPlan_HeaderTextCollapsedVsExpanded covers the header's
// ratio text and disclosure glyph in both states, and that collapsing hides
// the item rows (row count assertion).
func TestSessionPanelPlan_HeaderTextCollapsedVsExpanded(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.session.Todos = []session.Todo{
		{Status: session.TodoStatusCompleted, Content: "a"},
		{Status: session.TodoStatusPending, Content: "b"},
	}

	collapsed := u.sessionPanelPlan(100)
	require.True(t, collapsed.todosVisible)
	require.False(t, collapsed.todosExpanded)
	require.Equal(t, "todos 1/2 ▸", sessionPanelTodosHeaderText(collapsed.todosCompleted, collapsed.todosTotal, collapsed.todosExpanded))
	require.Equal(t, 1, collapsed.totalRows, "collapsed: header row only")

	u.panel.expanded = true
	expanded := u.sessionPanelPlan(100)
	require.True(t, expanded.todosExpanded)
	require.Equal(t, "todos 1/2 ▾", sessionPanelTodosHeaderText(expanded.todosCompleted, expanded.todosTotal, expanded.todosExpanded))
	require.Equal(t, 1, len(expanded.todosPending), "collapsing hides item rows, expanding shows them")
	require.Equal(t, 1, len(expanded.todosDone))
	require.Equal(t, 3, expanded.totalRows, "expanded with ample budget: header + pending + completed rows")
}

// TestSessionPanelPlan_QueueAlwaysVisibleRegardlessOfTodosExpand covers that
// the queue list renders whenever there are queued items, independent of
// whether the todos section is expanded or even present.
func TestSessionPanelPlan_QueueAlwaysVisibleRegardlessOfTodosExpand(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.wsCache.promptQueueItems = []string{"do this", "then that"}

	collapsed := u.sessionPanelPlan(100)
	require.Equal(t, []string{"do this", "then that"}, collapsed.queue)

	u.session.Todos = []session.Todo{{Status: session.TodoStatusPending, Content: "x"}}
	u.panel.expanded = true
	withTodos := u.sessionPanelPlan(100)
	require.Equal(t, []string{"do this", "then that"}, withTodos.queue)
}

// TestSessionPanelPlan_BudgetCapAndPriorityOrder covers the height budget:
// the total never exceeds ~40% of terminal height, and when a scenario
// (active threads + expanded todos + queue) exceeds that budget on a short
// terminal, the todos section's viewport shrinks (becoming scrollable)
// before the queue tail is truncated, and both stay ahead of the
// threads/delegations sections shrinking. Critically — this is the bug fix
// — todosDone/todosPending are never dropped from the plan at any budget:
// they always hold the full lists; only todosViewportRows (how many of
// them get painted this frame) shrinks.
func TestSessionPanelPlan_BudgetCapAndPriorityOrder(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.threadsDock.threads = mkDockThreads(2) // 4 rows
	u.panel.expanded = true
	u.session.Todos = []session.Todo{
		{Status: session.TodoStatusInProgress, Content: "active 1"},
		{Status: session.TodoStatusPending, Content: "active 2"},
		{Status: session.TodoStatusPending, Content: "active 3"},
		{Status: session.TodoStatusCompleted, Content: "done 1"},
		{Status: session.TodoStatusCompleted, Content: "done 2"},
		{Status: session.TodoStatusCompleted, Content: "done 3"},
	}
	u.wsCache.promptQueueItems = []string{"q1", "q2", "q3", "q4"}

	// Natural size: 4 (threads) + 1 (header) + 3 (active) + 3 (completed) + 4 (queue) = 15.
	full := u.sessionPanelPlan(100)
	require.Equal(t, 15, full.totalRows)
	require.False(t, full.todosScrollable, "ample budget: nothing hidden, nothing to scroll")

	// Budget 12: exactly enough room to shrink the todos viewport by 3 rows
	// (hiding, not dropping, the three completed rows) and nothing else.
	p := u.sessionPanelPlan(12)
	require.Equal(t, 4, p.threadsRows, "threads must not shrink yet")
	require.True(t, p.todosExpanded, "todos list must still be expanded")
	require.Len(t, p.todosDone, 3, "completed todos are never dropped from the plan")
	require.Len(t, p.todosInProgress, 1)
	require.Len(t, p.todosPending, 2)
	require.Equal(t, 3, p.todosViewportRows, "viewport shrinks to in-progress+pending only")
	require.True(t, p.todosScrollable, "the hidden completed rows must be reachable by scrolling")
	require.Len(t, p.queue, 4, "queue must be untouched")
	require.LessOrEqual(t, p.totalRows, 12)

	// Budget 9: the todos viewport shrinks to its floor — the in-progress
	// row only, never fewer — and the remaining one row of overage spills
	// onto the queue tail. The data (todosPending/todosDone) still isn't
	// dropped, only painted with 0 rows this frame.
	p = u.sessionPanelPlan(9)
	require.Equal(t, 4, p.threadsRows, "threads still must not shrink")
	require.True(t, p.todosExpanded, "todos stays expanded — budget shedding no longer forces a collapse")
	require.Len(t, p.todosInProgress, 1)
	require.Len(t, p.todosPending, 2, "pending todos are never dropped from the plan")
	require.Len(t, p.todosDone, 3, "completed todos are never dropped from the plan")
	require.Equal(t, 1, p.todosViewportRows, "viewport shrinks to the in-progress row only")
	require.True(t, p.todosScrollable)
	require.Len(t, p.queue, 3, "the one row of overage the viewport floor can't absorb spills onto the queue")
	require.LessOrEqual(t, p.totalRows, 9)

	// Budget 7: todos viewport is already at its floor, so the queue tail
	// keeps truncating, before threads shrink.
	p = u.sessionPanelPlan(7)
	require.Equal(t, 4, p.threadsRows, "threads still must not shrink")
	require.True(t, p.todosExpanded)
	require.Len(t, p.todosDone, 3, "completed todos still never dropped from the plan")
	require.Equal(t, 1, p.todosViewportRows)
	require.Len(t, p.queue, 1, "queue truncated to whatever fits before threads shrink")
	require.LessOrEqual(t, p.totalRows, 7)

	// Pathological budget: even threads must shrink.
	p = u.sessionPanelPlan(3)
	require.Equal(t, 1, p.threadsRows, "threads section is the last resort to shrink")
	require.Len(t, p.todosDone, 3, "even at a pathological budget, completed todos stay in the plan's data")
	require.LessOrEqual(t, p.totalRows, 3)

	// The overall cap: sessionPanelHeight itself must respect the 40% cap
	// against whatever available height it's handed.
	available := 20
	require.LessOrEqual(t, u.sessionPanelHeight(available), int(float64(available)*sessionPanelBudgetFraction))
}

// TestSessionPanelHeight_ZeroContentMatchesBaseline is the regression guard
// for the merged panel: with no active threads, no incomplete todos, and no
// queued prompts, the panel must occupy exactly zero rows, and every other
// layout rectangle must be byte-identical to a UI that never touches
// threads/todos/queue state at all.
func TestSessionPanelHeight_ZeroContentMatchesBaseline(t *testing.T) {
	t.Parallel()

	baseline := newTestUI()
	baseline.updateLayoutAndSize()

	u := sessionUI()
	u.session.Todos = nil
	u.threadsDock.threads = nil
	u.wsCache.promptQueueItems = nil
	u.updateLayoutAndSize()

	require.Zero(t, u.sessionPanelHeight(100))
	require.Zero(t, u.layout.panel, "panel must occupy zero space with no threads/todos/queue")
}

// TestSessionPanelPlan_PanelHidesOnceAllTodosCompleted covers the panel's
// role as the *live* view of active work: it disappears once every todo is
// completed (and can no longer be toggled open), handing off to the chat
// transcript — which always renders the full list (see
// chat.TodosToolRenderContext) — as the permanent record. Nothing is lost;
// it just moves from the docked panel to the scrollback.
func TestSessionPanelPlan_PanelHidesOnceAllTodosCompleted(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.dialog = dialog.NewOverlay()
	u.session.Todos = []session.Todo{
		{Content: "a", Status: session.TodoStatusCompleted},
		{Content: "b", Status: session.TodoStatusCompleted},
	}
	u.updateLayoutAndSize()

	plan := u.sessionPanelPlan(100)
	require.False(t, plan.todosVisible, "an all-completed list must no longer occupy the panel")
	require.Zero(t, u.layout.panel, "panel must occupy zero space once every todo is completed")

	// Nothing left to toggle: an all-completed list can't be expanded via
	// the panel (it isn't there to expand).
	u.toggleTodosExpanded()
	require.False(t, u.panel.expanded)
}

// TestDrawSessionPanel_CollapsedStillShowsInProgressTodo is the regression
// test for "collapsing the panel is never total": even with the todos
// section collapsed (m.panel.expanded == false), a todo that's actively
// in progress right now must still be painted, not hidden behind the
// header until the user expands the section. sessionPanelPlan populating
// plan.todosActive with the in-progress subset isn't enough on its own —
// drawSessionPanel used to re-gate the whole item-drawing loop on
// plan.todosExpanded, which silently swallowed those rows again.
func TestDrawSessionPanel_CollapsedStillShowsInProgressTodo(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.session.Todos = []session.Todo{
		{Content: "in flight", Status: session.TodoStatusInProgress, ActiveForm: "Doing the in-flight task"},
		{Content: "not started", Status: session.TodoStatusPending},
		{Content: "already done", Status: session.TodoStatusCompleted},
	}
	require.False(t, u.panel.expanded, "collapsed by default")

	plan := u.sessionPanelPlan(100)
	require.False(t, plan.todosExpanded)
	require.Equal(t, 2, plan.totalRows, "header + the one always-visible in-progress row")

	scr := uv.NewScreenBuffer(u.width, 3)
	area := uv.Rectangle{Max: uv.Position{X: u.width, Y: 3}}
	u.drawSessionPanel(scr, area)
	out := ansi.Strip(scr.Render())

	require.Contains(t, out, "Doing the in-flight task", "in-progress todo must render even while collapsed")
	require.NotContains(t, out, "not started", "pending todo must stay hidden while collapsed")
	require.NotContains(t, out, "already done", "completed todo must stay hidden while collapsed")
}

// TestSessionPanelPlan_RealisticTerminalNoSheddingForEverydayTodoList is the
// regression test for the 40%-budget bug: sessionPanelHeight used to
// compute its 40% cap against the whole-terminal m.height, which includes
// rows (header, editor, help) that never compete with the panel for space.
// Since generateLayout also hard-clamps the result against mainRect.Dy()
// (the space actually split between chat and the panel) immediately
// afterward, basing the 40% budget on m.height instead of that same
// mainRect.Dy() made the internal budget check inconsistent with the real
// downstream constraint — a small, everyday todo list could get shed
// (completed rows dropped) even though nothing else was competing for the
// space. This covers a small (5-item) list on both a generous terminal
// (140x45, matching newTestUI's convention) and a common, tighter one
// (80x24): in neither case should a handful of todos trigger shedding.
func TestSessionPanelPlan_RealisticTerminalNoSheddingForEverydayTodoList(t *testing.T) {
	t.Parallel()

	todos := []session.Todo{
		{Content: "one", Status: session.TodoStatusCompleted},
		{Content: "two", Status: session.TodoStatusCompleted},
		{Content: "three", Status: session.TodoStatusInProgress, ActiveForm: "Doing three"},
		{Content: "four", Status: session.TodoStatusPending},
		{Content: "five", Status: session.TodoStatusPending},
	}
	// header + all active (in-progress + pending) + all completed.
	wantTotalRows := 1 + 3 + 2

	for _, tc := range []struct {
		name          string
		width, height int
	}{
		{"newTestUI convention (140x45)", 140, 45},
		{"typical screen (80x24)", 80, 24},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			u := sessionUI()
			u.width, u.height = tc.width, tc.height
			u.panel.expanded = true
			u.session.Todos = todos

			u.dialog = dialog.NewOverlay()
			u.updateLayoutAndSize()

			plan := u.sessionPanelPlan(u.layout.panel.Dy())
			require.True(t, plan.todosExpanded, "no shedding: list must stay expanded")
			require.Len(t, plan.todosInProgress, 1, "no shedding: in-progress rows must all be present")
			require.Len(t, plan.todosPending, 2, "no shedding: pending rows must all be present")
			require.Len(t, plan.todosDone, 2, "no shedding: completed rows must not be dropped")
			require.Equal(t, wantTotalRows, plan.totalRows, "unclamped natural size, nothing dropped")
		})
	}
}

// TestMouseClick_ThreadBlockEntersThread covers the click hit-test: a
// tea.MouseClickMsg landing on a rendered thread block's rect must return a
// tea.Cmd that yields enterThreadMsg with that thread's ID/session ID —
// the same drill-in mechanism the threads dashboard uses (see
// Root.attachThreadCmd), not enterChildSession/navStack.
func TestMouseClick_ThreadBlockEntersThread(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.dialog = dialog.NewOverlay()
	u.threadsDock.threads = []proto.Thread{
		{ID: "t1", SessionID: "s-t1", Name: "fix-auth", Status: "running", CreatedAt: time.Now().Unix()},
	}
	u.updateLayoutAndSize()

	// Populate the hit-test state the same way Draw's uiChat case does,
	// without going through the full Draw (newTestUI's minimal setup
	// doesn't wire the sidebar machinery Draw also touches).
	scr := uv.NewScreenBuffer(u.width, u.height)
	u.drawSessionPanel(scr, u.layout.panel)
	require.Len(t, u.panelThreadRects, 1)

	rect := u.panelThreadRects[0]
	_, cmd := u.Update(tea.MouseClickMsg{X: rect.Min.X, Y: rect.Min.Y, Button: tea.MouseLeft})
	require.NotNil(t, cmd)

	msg := cmd()
	entered, ok := msg.(enterThreadMsg)
	require.True(t, ok, "expected enterThreadMsg, got %T", msg)
	require.Equal(t, "t1", entered.id)
	require.Equal(t, "s-t1", entered.sessionID)
}

// TestMouseClick_TodosHeaderTogglesWithoutPriorDraw is the regression test
// for the dead-click-on-first-frame bug: the click hit-test used to read
// m.panelTodosHeaderRect, which is only populated as a side effect of
// drawSessionPanel — itself only reachable through Draw/View. A click
// delivered by Update before the panel's first paint (e.g. immediately
// after updateLayoutAndSize runs synchronously inside Update, which is
// exactly what happens when a session/todos event arrives) hit a
// stale/zero rect and was silently swallowed. This test deliberately never
// calls drawSessionPanel or Draw — only updateLayoutAndSize, the same
// layout step a real running Program takes inside Update — to prove the
// click still works.
func TestMouseClick_TodosHeaderTogglesWithoutPriorDraw(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.dialog = dialog.NewOverlay()
	u.session.Todos = []session.Todo{
		{Content: "write tests", Status: session.TodoStatusPending},
	}
	u.updateLayoutAndSize()
	require.NotZero(t, u.layout.panel, "panel must occupy space with an incomplete todo present")
	require.False(t, u.panel.expanded)

	// Derive the header's expected coordinates the same way the fixed click
	// handler does, independently of any cached Draw-time field.
	plan := u.sessionPanelPlan(u.layout.panel.Dy())
	_, _, headerRect, _ := sessionPanelRowLayout(u.layout.panel, plan)
	require.NotZero(t, headerRect, "expected a non-empty todos header rect")

	_, cmd := u.Update(tea.MouseClickMsg{X: headerRect.Min.X, Y: headerRect.Min.Y, Button: tea.MouseLeft})
	require.True(t, u.panel.expanded, "click on the todos header must toggle expand state on the very first frame")
	_ = cmd
}

// TestMouseClick_ThreadBlockEntersThreadWithoutPriorDraw mirrors
// TestMouseClick_TodosHeaderTogglesWithoutPriorDraw for the thread-block
// hit-test, which shared the same Draw-time-cache bug via
// m.panelThreadRects/m.panelThreads.
func TestMouseClick_ThreadBlockEntersThreadWithoutPriorDraw(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.dialog = dialog.NewOverlay()
	u.threadsDock.threads = []proto.Thread{
		{ID: "t1", SessionID: "s-t1", Name: "fix-auth", Status: "running", CreatedAt: time.Now().Unix()},
	}
	u.updateLayoutAndSize()
	require.Empty(t, u.panelThreadRects, "must not have been populated by any Draw call yet")

	plan := u.sessionPanelPlan(u.layout.panel.Dy())
	threadRects, _, _, _ := sessionPanelRowLayout(u.layout.panel, plan)
	require.Len(t, threadRects, 1)

	rect := threadRects[0]
	_, cmd := u.Update(tea.MouseClickMsg{X: rect.Min.X, Y: rect.Min.Y, Button: tea.MouseLeft})
	require.NotNil(t, cmd)

	msg := cmd()
	entered, ok := msg.(enterThreadMsg)
	require.True(t, ok, "expected enterThreadMsg, got %T", msg)
	require.Equal(t, "t1", entered.id)
	require.Equal(t, "s-t1", entered.sessionID)
}

// nineTodosThreeDone builds the 9-todo (2 in-progress, 4 pending, 3
// completed) fixture used by the scroll tests below — enough rows that,
// combined with a competing thread block, a small-but-common terminal
// height genuinely can't paint all of them at once.
func nineTodosThreeDone() []session.Todo {
	return []session.Todo{
		{Status: session.TodoStatusInProgress, Content: "in progress 1", ActiveForm: "Doing in progress 1"},
		{Status: session.TodoStatusInProgress, Content: "in progress 2", ActiveForm: "Doing in progress 2"},
		{Status: session.TodoStatusPending, Content: "pending 1"},
		{Status: session.TodoStatusPending, Content: "pending 2"},
		{Status: session.TodoStatusPending, Content: "pending 3"},
		{Status: session.TodoStatusPending, Content: "pending 4"},
		{Status: session.TodoStatusCompleted, Content: "done 1"},
		{Status: session.TodoStatusCompleted, Content: "done 2"},
		{Status: session.TodoStatusCompleted, Content: "done 3"},
	}
}

// TestSessionPanelPlan_SmallTerminalNeverDropsTodosOnlyWindows is the bug
// regression test: on a small-but-common terminal (80x24) with a
// competing thread block, the expanded panel's natural size (header +
// 2 in-progress + 4 pending + 3 done = 10 rows, plus the thread block)
// doesn't fit the panel's budget — yet all three completed todos must
// still be present in plan.todosDone (never dropped), and the section must
// report itself scrollable rather than silently truncating them.
func TestSessionPanelPlan_SmallTerminalNeverDropsTodosOnlyWindows(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.dialog = dialog.NewOverlay()
	u.width, u.height = 80, 24
	u.panel.expanded = true
	u.threadsDock.threads = mkDockThreads(1)
	u.session.Todos = nineTodosThreeDone()
	u.updateLayoutAndSize()

	plan := u.sessionPanelPlan(u.layout.panel.Dy())
	require.True(t, plan.todosExpanded)
	require.Equal(t, 2, plan.threadsRows, "the competing thread block must not be shed by this scenario")
	require.Len(t, plan.todosInProgress, 2)
	require.Len(t, plan.todosPending, 4)
	require.Len(t, plan.todosDone, 3, "all three completed todos must still be in the plan, never dropped")
	require.Less(t, plan.todosViewportRows, plan.todosContentRows,
		"the natural size must not fit — otherwise this scenario isn't exercising the budget at all")
	require.True(t, plan.todosScrollable, "the section must report itself scrollable rather than truncating")
}

// TestDrawSessionPanel_TodosScrollRevealsHiddenRows is the rendering-level
// half of the regression test above: at the default (top) scroll offset the
// completed rows are off-screen, but scrolling the todos section — via the
// same CoalescedWheelMsg path a real mouse wheel produces, with the pointer
// over the todos list area — brings them on screen. No todo is ever
// silently unreachable: every one of the 9 is either painted at the current
// offset or reachable by scrolling.
func TestDrawSessionPanel_TodosScrollRevealsHiddenRows(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.dialog = dialog.NewOverlay()
	u.width, u.height = 80, 24
	u.panel.expanded = true
	u.threadsDock.threads = mkDockThreads(1)
	u.session.Todos = nineTodosThreeDone()
	u.updateLayoutAndSize()

	scr := uv.NewScreenBuffer(u.width, u.height)
	u.drawSessionPanel(scr, u.layout.panel)
	atTop := ansi.Strip(scr.Render())
	require.Contains(t, atTop, "in progress 1", "the viewport floor always shows in-progress rows")
	require.NotContains(t, atTop, "done 1", "completed rows start out scrolled off-screen on this small terminal")
	require.NotContains(t, atTop, "done 2")
	require.NotContains(t, atTop, "done 3")

	// Locate the todos list area the same way a real mouse wheel event
	// would hit-test it, then drive enough wheel-down events through
	// Update to reach the bottom of the section.
	plan := u.sessionPanelPlan(u.layout.panel.Dy())
	_, _, _, todosListRect := sessionPanelRowLayout(u.layout.panel, plan)
	require.NotZero(t, todosListRect, "expected a non-empty todos list rect to scroll")

	maxOffset := plan.todosContentRows - plan.todosViewportRows
	require.Positive(t, maxOffset, "fixture must actually need scrolling")
	for range maxOffset {
		u.Update(common.CoalescedWheelMsg{
			Mouse:  tea.Mouse{X: todosListRect.Min.X, Y: todosListRect.Min.Y},
			DeltaY: 1,
		})
	}
	require.Equal(t, maxOffset, u.panelTodosScrollOffset, "wheel-down must reach the section's bottom")

	scr = uv.NewScreenBuffer(u.width, u.height)
	u.drawSessionPanel(scr, u.layout.panel)
	scrolled := ansi.Strip(scr.Render())
	require.Contains(t, scrolled, "done 1", "scrolling to the bottom must reveal every completed todo")
	require.Contains(t, scrolled, "done 2")
	require.Contains(t, scrolled, "done 3")

	// Scrolling past the bottom must clamp, not run off the end of the
	// underlying slice.
	require.NotPanics(t, func() {
		for range 5 {
			u.Update(common.CoalescedWheelMsg{
				Mouse:  tea.Mouse{X: todosListRect.Min.X, Y: todosListRect.Min.Y},
				DeltaY: 1,
			})
		}
	})
	require.Equal(t, maxOffset, u.panelTodosScrollOffset, "offset must clamp at the bottom")
}

// TestRenderSessionTodoLine_CompletedStyleSurvivesBudgetConstrainedPlan
// re-verifies the strikethrough/muted styling chain end to end through
// sessionPanelPlan and drawSessionPanel — not just the low-level
// renderSessionTodoLine unit test above — on the exact small-terminal,
// budget-constrained scenario this bug was found in, so a future change to
// the shedding/windowing mechanic can't silently reintroduce lost styling
// the same way it silently reintroduced lost rows.
func TestRenderSessionTodoLine_CompletedStyleSurvivesBudgetConstrainedPlan(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.dialog = dialog.NewOverlay()
	u.width, u.height = 80, 24
	u.panel.expanded = true
	u.threadsDock.threads = mkDockThreads(1)
	u.session.Todos = nineTodosThreeDone()
	u.updateLayoutAndSize()

	plan := u.sessionPanelPlan(u.layout.panel.Dy())
	require.Len(t, plan.todosDone, 3)

	maxOffset := plan.todosContentRows - plan.todosViewportRows
	u.panelTodosScrollOffset = maxOffset
	scr := uv.NewScreenBuffer(u.width, u.height)
	u.drawSessionPanel(scr, u.layout.panel)
	out := scr.Render()

	require.Contains(t, ansi.Strip(out), "done 1")
	// Find the raw (unstripped) line containing "done 1" and confirm it
	// still carries the strikethrough SGR parameter renderSessionTodoLine
	// applies for session.TodoStatusCompleted.
	lines := strings.Split(out, "\n")
	found := false
	for _, line := range lines {
		if strings.Contains(ansi.Strip(line), "done 1") {
			found = true
			require.Contains(t, line, ";9m", "completed todo row must carry a strikethrough SGR parameter even under budget constraints")
		}
	}
	require.True(t, found, "expected to find the rendered \"done 1\" row")
}
