package model

import (
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/ui/chat"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// TestSessionPanelPlan_RunningDelegationProducesBlock covers the
// delegations section: a running (unfinished) top-level agent tool call in
// the current session's chat must show up as a panel block, sized exactly
// like a thread block (two rows) — not a compressed one-liner.
func TestSessionPanelPlan_RunningDelegationProducesBlock(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	item := chat.NewAgentToolMessageItem(u.com.Styles,
		message.ToolCall{ID: "tc-1", Name: "agent", Input: `{"prompt":"fix the auth bug"}`, Finished: false}, nil, false, nil)
	item.SetMessageID("m1")
	u.chat.SetMessages(item)

	plan := u.sessionPanelPlan(100)
	require.Len(t, plan.delegations, 1)
	require.Equal(t, 2, plan.delegationsRows, "delegation blocks are two rows, exactly like thread blocks")
	require.Equal(t, "m1", plan.delegations[0].messageID)
	require.Equal(t, "tc-1", plan.delegations[0].toolCallID)
}

// TestDrawSessionPanel_DelegationBlockRendersNameTaskAndStatus is the
// end-to-end draw check: a running delegation's block must actually paint
// its name, task, and a live status line via the shared draw path — the
// same geometry (panelBlockGeometry) and block shape (number + bold name —
// task on line 1, status on line 2) as threads.
func TestDrawSessionPanel_DelegationBlockRendersNameTaskAndStatus(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	item := chat.NewAgentToolMessageItem(u.com.Styles,
		message.ToolCall{ID: "tc-1", Name: "agent", Input: `{"prompt":"fix the auth bug"}`, Finished: false}, nil, false, nil)
	item.SetMessageID("m1")
	u.chat.SetMessages(item)

	plan := u.sessionPanelPlan(100)
	require.Equal(t, 2, plan.delegationsRows)

	scr := uv.NewScreenBuffer(u.width, 2)
	area := uv.Rectangle{Max: uv.Position{X: u.width, Y: 2}}
	rects := u.drawPanelBlocks(scr, area, -1, panelBlockDrawSpec{
		count: len(plan.delegations), more: plan.delegationsMore, footer: "…and %d more delegations",
		name: func(i int) string { return delegationName(plan.delegations[i].item) },
		task: func(i int) string { return delegationTask(plan.delegations[i].item) },
		line2: func(i int) string {
			return "  " + u.panelActivityIcon() + " " + delegationStatusLine(plan.delegations[i].item, u.com.Styles, u.width-4)
		},
	})
	require.Len(t, rects, 1)
	// Geometry must come from the exact function threads use for their own
	// blocks, not a parallel reimplementation.
	require.Equal(t, panelBlockGeometry(area, 1), rects)

	out := ansi.Strip(scr.Render())
	require.Contains(t, out, "task", "the built-in agent tool always dispatches to config.AgentTask")
	require.Contains(t, out, "fix the auth bug")
}

// TestChatRendersDelegationAsCompactStubWhilePending covers the chat
// transcript side of the panel/chat split for delegations (mirroring the
// todos split): while a delegation is running, its own chat render is the
// pending stub plus a single current-activity status line (elapsed, step,
// latest tool) — no task tag, todos, or nested-tool tree; that deeper live
// detail belongs to the panel.
func TestChatRendersDelegationAsCompactStubWhilePending(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	item := chat.NewAgentToolMessageItem(&sty,
		message.ToolCall{ID: "tc-1", Name: "agent", Input: `{"prompt":"fix the auth bug"}`, Finished: false}, nil, false, nil)
	item.AddNestedTool(chat.NewToolMessageItem(&sty, "m1",
		message.ToolCall{ID: "c1", Name: "bash", Input: `{"command":"go test"}`, Finished: true}, nil, false, nil))

	out := ansi.Strip(item.Render(120))
	require.NotContains(t, out, "fix the auth bug", "the task/prompt belongs to the panel block, not the transcript")
	require.Contains(t, out, "go test", "the current tool must show in the status line under the stub")
	require.Len(t, strings.Split(strings.TrimRight(out, " \n"), "\n"), 2,
		"pending transcript render is exactly stub + one status line")
}

// TestSessionPanelPlan_FinishedDelegationLeavesPanel covers the handoff:
// once a delegation's tool result lands, RunningDelegations must stop
// returning it (it's no longer pending), so it drops out of the panel —
// and the chat's own collapsed-delegation summary (pre-existing behavior,
// untouched by this pass) takes over as the permanent record.
func TestSessionPanelPlan_FinishedDelegationLeavesPanel(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	tc := message.ToolCall{ID: "tc-1", Name: "agent", Input: `{"prompt":"fix the auth bug"}`, Finished: false}
	item := chat.NewAgentToolMessageItem(u.com.Styles, tc, nil, false, nil)
	item.SetMessageID("m1")
	u.chat.SetMessages(item)

	require.Len(t, u.sessionPanelPlan(100).delegations, 1, "must be in the panel while running")

	finished := tc
	finished.Finished = true
	item.SetToolCall(finished)
	item.SetResult(&message.ToolResult{ToolCallID: "tc-1", Content: "Fixed it."})

	plan := u.sessionPanelPlan(100)
	require.Empty(t, plan.delegations, "a finished delegation must leave the panel")

	out := ansi.Strip(item.Render(120))
	require.Contains(t, out, "task", "the transcript's collapsed summary must take over once finished")
	require.Contains(t, out, "Fixed it.", "collapsed summary previews the result")
}

// TestSessionPanelPlan_ShedPriority_ThreadsThenDelegationsThenTodosViewportThenQueue
// covers the full shedding priority order once delegations are in the mix:
// threads > delegations > todos-in-progress (the viewport floor, never
// shrunk below it) > todos pending/done (windowed via todosViewportRows,
// never dropped from the plan) > queue > delegations row budget > threads
// row budget.
func TestSessionPanelPlan_ShedPriority_ThreadsThenDelegationsThenTodosViewportThenQueue(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	// A task, not a thread: this test needs the todos section present, and
	// a running thread deliberately replaces it (see sessionPanelPlan).
	u.threadsDock.cache.value = mkDockTasks(1) // 2 rows
	item := chat.NewAgentToolMessageItem(u.com.Styles,
		message.ToolCall{ID: "tc-1", Name: "agent", Input: `{"prompt":"do the thing"}`, Finished: false}, nil, false, nil)
	item.SetMessageID("m1")
	u.chat.SetMessages(item) // 2 rows
	u.panel.expanded = true
	u.session.Todos = []session.Todo{
		{Status: session.TodoStatusInProgress, Content: "in flight"}, // 1 row, the viewport floor
		{Status: session.TodoStatusCompleted, Content: "done"},       // 1 row, expanded-only, windows first
	}
	u.wsCache.promptQueueItems = []string{"q1"} // 1 row

	// Natural size: 1 (threads header) + 2 (threads) + 1 (delegations
	// header) + 2 (delegations) + 1 (todos header) + 1 (in-progress) +
	// 1 (done) + 1 (queue header) + 1 (queue) = 11. Each non-empty section
	// now carries its own section-separator header row — see
	// sessionPanelPlan's threadsHeaderRows/delegationsHeaderRows/
	// queueHeaderRows.
	full := u.sessionPanelPlan(100)
	require.Equal(t, 11, full.totalRows)
	require.Len(t, full.delegations, 1)

	// Budget 10: exactly enough to shrink the todos viewport by one row
	// (hiding, not dropping, the completed todo) and nothing else — threads,
	// delegations, and the queue all stay untouched.
	p := u.sessionPanelPlan(10)
	require.Equal(t, 2, p.threadsRows, "threads must not shrink")
	require.Len(t, p.delegations, 1, "delegations must not shrink yet")
	require.Equal(t, 2, p.delegationsRows, "delegations must not shrink yet")
	require.Len(t, p.todosDone, 1, "completed todos are never dropped from the plan")
	require.Len(t, p.todosInProgress, 1)
	require.Equal(t, 1, p.todosViewportRows, "viewport shrinks to the in-progress row only")
	require.True(t, p.todosScrollable)
	require.Len(t, p.queue, 1, "queue still untouched")

	// Budget 9: todos viewport is already at its in-progress floor, so the
	// (single-item) queue — and its now-freed header row — is truncated
	// next, before delegations/threads shrink.
	p = u.sessionPanelPlan(9)
	require.Equal(t, 2, p.threadsRows)
	require.Equal(t, 2, p.delegationsRows, "delegations must not shrink yet")
	require.Len(t, p.todosInProgress, 1, "in-progress todos are never shed while the section stays visible")
	require.Equal(t, 1, p.todosViewportRows)
	require.Empty(t, p.queue, "queue truncated before delegations shrink")
	require.Zero(t, p.queueHeaderRows, "an emptied queue's header disappears for free")

	// Budget 7: the queue is already empty, so the next row of overage
	// shrinks the delegations section (partially — one of its two rows),
	// but threads still hold their full rows.
	p = u.sessionPanelPlan(7)
	require.Equal(t, 2, p.threadsRows, "threads are the last resort to shrink")
	require.Less(t, p.delegationsRows, 2, "delegations shrink before threads")
	require.Len(t, p.todosDone, 1, "completed todos still never dropped from the plan")

	// Budget 4: tight enough that delegations are shed entirely and threads
	// themselves must start shrinking too.
	p = u.sessionPanelPlan(4)
	require.Zero(t, p.delegationsRows, "delegations are fully shed before threads shrink")
	require.Zero(t, p.delegationsHeaderRows, "a fully-shed delegations section's header disappears for free")
	require.Less(t, p.threadsRows, 2, "threads shrink once delegations are already at zero")
	require.Len(t, p.todosDone, 1, "completed todos still never dropped from the plan")
}
