package model

import (
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
// its name, task, and a live status line via drawDelegationBlocks — the
// same shared geometry (panelBlockGeometry) and block shape (number + bold
// name — task on line 1, status on line 2) as drawThreadBlocks.
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
	rects := u.drawDelegationBlocks(scr, area, plan, -1)
	require.Len(t, rects, 1)
	// Geometry must come from the exact function threads use for their own
	// blocks, not a parallel reimplementation.
	require.Equal(t, panelBlockGeometry(area, 1), rects)

	out := ansi.Strip(scr.Render())
	require.Contains(t, out, "task", "the built-in agent tool always dispatches to config.AgentTask")
	require.Contains(t, out, "fix the auth bug")
}

// TestChatRendersDelegationAsOneLineStubWhilePending covers the chat
// transcript side of the panel/chat split for delegations (mirroring the
// todos split): while a delegation is running, its own chat render must be
// just the one-line pending stub — no task tag, status line, or
// nested-tool tree — since the panel now owns all of that live detail.
func TestChatRendersDelegationAsOneLineStubWhilePending(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	item := chat.NewAgentToolMessageItem(&sty,
		message.ToolCall{ID: "tc-1", Name: "agent", Input: `{"prompt":"fix the auth bug"}`, Finished: false}, nil, false, nil)
	item.AddNestedTool(chat.NewToolMessageItem(&sty, "m1",
		message.ToolCall{ID: "c1", Name: "bash", Input: `{"command":"go test"}`, Finished: true}, nil, false, nil))

	out := ansi.Strip(item.Render(120))
	require.NotContains(t, out, "fix the auth bug", "the task/prompt now belongs to the panel block, not the transcript")
	require.NotContains(t, out, "go test", "nested tool detail must not leak into the pending transcript stub")
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
	u.threadsDock.threads = mkDockThreads(1) // 2 rows
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

	// Natural size: 2 (threads) + 2 (delegations) + 1 (header) + 1 (in-progress) + 1 (done) + 1 (queue) = 8.
	full := u.sessionPanelPlan(100)
	require.Equal(t, 8, full.totalRows)
	require.Len(t, full.delegations, 1)

	// Budget 7: exactly enough to shrink the todos viewport by one row
	// (hiding, not dropping, the completed todo) and nothing else.
	p := u.sessionPanelPlan(7)
	require.Equal(t, 2, p.threadsRows, "threads must not shrink")
	require.Len(t, p.delegations, 1, "delegations must not shrink yet")
	require.Len(t, p.todosDone, 1, "completed todos are never dropped from the plan")
	require.Len(t, p.todosInProgress, 1)
	require.Equal(t, 1, p.todosViewportRows, "viewport shrinks to the in-progress row only")
	require.True(t, p.todosScrollable)
	require.Len(t, p.queue, 1, "queue still untouched")

	// Budget 5: todos viewport is already at its in-progress floor, so the
	// queue is truncated next, before delegations/threads shrink.
	p = u.sessionPanelPlan(5)
	require.Equal(t, 2, p.threadsRows)
	require.Len(t, p.delegations, 1)
	require.Len(t, p.todosInProgress, 1, "in-progress todos are never shed while the section stays visible")
	require.Equal(t, 1, p.todosViewportRows)
	require.Empty(t, p.queue, "queue truncated before delegations shrink")

	// Budget 4: tight enough to also shrink delegations, but threads still
	// hold their full rows.
	p = u.sessionPanelPlan(4)
	require.Equal(t, 2, p.threadsRows, "threads are the last resort to shrink")
	require.Less(t, p.delegationsRows, 2, "delegations shrink before threads")
	require.Len(t, p.todosDone, 1, "completed todos still never dropped from the plan")
}
