package chat

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// mkNestedToolCall builds a synthetic nested-tool item, mirroring what
// handleChildSessionMessage in internal/ui/model/ui.go creates from a
// live child-session pubsub event.
func mkNestedToolCall(t *testing.T, sty *styles.Styles, id, name, input string) ToolMessageItem {
	t.Helper()
	tc := message.ToolCall{ID: id, Name: name, Input: input, Finished: true}
	return NewToolMessageItem(sty, "child-msg", tc, nil, false, nil)
}

// TestFormatElapsed covers the elapsed-time formatting used by the
// running status line: seconds-only, minutes+seconds, and hours+minutes.
func TestFormatElapsed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		d    time.Duration
		want string
	}{
		{9 * time.Second, "9s"},
		{45 * time.Second, "45s"},
		{4*time.Minute + 12*time.Second, "4m12s"},
		{time.Hour + 2*time.Minute, "1h02m"},
	}
	for _, tt := range tests {
		require.Equal(t, tt.want, formatElapsed(tt.d))
	}
}

// TestLastToolSummary covers the "→ tool arg" fragment of the status
// line for the tool kinds a sub-agent commonly runs — this is the exact
// piece the bug report asked for: "→ grep "Provider" internal/config".
func TestLastToolSummary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		tc   message.ToolCall
		want string
	}{
		{
			name: "grep with path",
			tc:   message.ToolCall{Name: "grep", Input: `{"pattern":"Provider","path":"internal/config"}`},
			want: `grep "Provider" internal/config`,
		},
		{
			name: "grep without path",
			tc:   message.ToolCall{Name: "grep", Input: `{"pattern":"Provider"}`},
			want: `grep "Provider"`,
		},
		{
			name: "view",
			tc:   message.ToolCall{Name: "view", Input: `{"file_path":"internal/foo.go"}`},
			want: "view internal/foo.go",
		},
		{
			name: "bash uses description over command",
			tc:   message.ToolCall{Name: "bash", Input: `{"description":"run tests","command":"go test ./..."}`},
			want: "bash run tests",
		},
		{
			name: "unrecognized tool falls back to bare name",
			tc:   message.ToolCall{Name: "some_mcp_tool", Input: `{}`},
			want: "some_mcp_tool",
		},
		{
			name: "unparsable input falls back to bare name",
			tc:   message.ToolCall{Name: "grep", Input: `not json`},
			want: "grep",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, LastToolSummary(tt.tc))
		})
	}
}

// TestRenderAgentStatusLine_Content builds the line end-to-end from
// synthetic nested-tool events and checks that elapsed time, step count,
// last-tool summary, and token count all show up in the expected order.
func TestRenderAgentStatusLine_Content(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	nested := []ToolMessageItem{
		mkNestedToolCall(t, &sty, "c1", "view", `{"file_path":"internal/foo.go"}`),
		mkNestedToolCall(t, &sty, "c2", "grep", `{"pattern":"Provider","path":"internal/config"}`),
	}

	start := time.Now().Add(-(4*time.Minute + 12*time.Second))
	line := renderAgentStatusLine(&sty, 200, start, nested, 1200, 300)
	plain := ansi.Strip(line)

	require.Contains(t, plain, "4m12s")
	require.Contains(t, plain, "step 2")
	require.Contains(t, plain, `→ grep "Provider" internal/config`)
	require.Contains(t, plain, "1.5k tok")

	// Order matters: elapsed, then step, then last tool, then tokens.
	elapsedIdx := strings.Index(plain, "4m12s")
	stepIdx := strings.Index(plain, "step 2")
	toolIdx := strings.Index(plain, "→ grep")
	tokIdx := strings.Index(plain, "1.5k tok")
	require.True(t, elapsedIdx < stepIdx && stepIdx < toolIdx && toolIdx < tokIdx,
		"status line fields must render in elapsed -> step -> last-tool -> tokens order, got: %s", plain)
}

// TestRenderAgentStatusLine_NoNestedTools covers the very first seconds
// of a delegation, before any child-session event has arrived. Elapsed
// time and step count must still render — this is what keeps the first
// stretch of a run from looking identical to a hang.
func TestRenderAgentStatusLine_NoNestedTools(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	line := renderAgentStatusLine(&sty, 200, time.Now(), nil, 0, 0)
	plain := ansi.Strip(line)

	require.Contains(t, plain, "step 0")
	require.NotContains(t, plain, "→")
	require.NotContains(t, plain, "tok")
}

// TestRenderAgentStatusLine_Truncation guards the width-bounded
// contract: the rendered line (stripped of ANSI styling) must never
// exceed the requested width, and a width of 0 must degrade to "" rather
// than panic or render garbage.
func TestRenderAgentStatusLine_Truncation(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	nested := []ToolMessageItem{
		mkNestedToolCall(t, &sty, "c1", "grep", `{"pattern":"a very very long pattern that should get cut off","path":"internal/config/somewhere/deep"}`),
	}

	for _, width := range []int{0, 1, 10, 20, 40} {
		line := renderAgentStatusLine(&sty, width, time.Now(), nested, 0, 0)
		require.LessOrEqualf(t, ansi.StringWidth(ansi.Strip(line)), width,
			"width %d: rendered line exceeds requested width: %q", width, line)
	}
}

// TestAgentToolMessageItem_PendingStatusLine is the render-path
// regression test for the reported bug: a still-running "agent"
// delegation with no nested tools yet must show more than a bare
// spinner — specifically, an elapsed-time status line — and once a
// child tool call arrives, that tool must appear in the rendered
// output.
func TestAgentToolMessageItem_PendingStatusLine(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	parent := message.ToolCall{ID: "agent-parent", Name: "agent", Input: `{"prompt":"inspect codebase"}`, Finished: false}
	item := NewAgentToolMessageItem(&sty, parent, nil, false, nil)
	item.startTime = time.Now().Add(-9 * time.Second)

	out := ansi.Strip(item.Render(120))
	require.Contains(t, out, "9s", "pending agent tool with no nested tools yet must still show elapsed time")
	require.Contains(t, out, "step 0")

	item.AddNestedTool(mkNestedToolCall(t, &sty, "c1", "grep", `{"pattern":"Provider","path":"internal/config"}`))
	out = ansi.Strip(item.Render(120))
	require.Contains(t, out, `→ grep "Provider" internal/config`,
		"once a child tool call arrives it must show up in the rendered status line")
}

// TestAgentToolMessageItem_SetChildSessionTokensBumpsVersion covers the
// live token-count update path (handleChildSessionUpdate in
// internal/ui/model/ui.go): pushing a new token count must invalidate
// the cached render, and a repeated identical update must not bump
// (matching the no-op dedupe on every other setter in this file).
func TestAgentToolMessageItem_SetChildSessionTokensBumpsVersion(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	parent := message.ToolCall{ID: "agent-parent", Name: "agent", Input: `{}`, Finished: false}
	item := NewAgentToolMessageItem(&sty, parent, nil, false, nil)

	requireBump(t, "SetChildSessionTokens[first update]", item, func() {
		item.SetChildSessionTokens(100, 50)
	})

	before := item.Version()
	item.SetChildSessionTokens(100, 50)
	require.Equal(t, before, item.Version(), "identical token counts must not bump the version")

	requireBump(t, "SetChildSessionTokens[changed]", item, func() {
		item.SetChildSessionTokens(200, 50)
	})
}

// TestAgentToolRenderCapsNestedTools covered the display density cap for a
// *running* delegation's nested-tool tree (last few visible, "+N earlier
// steps" summary for the rest). Now that a finished delegation collapses
// to a compact summary (see TestAgentToolRenderFinishedCollapses below),
// the cap only matters while still running — see
// TestRenderAgentStatusLine_* in this file for that coverage. This test is
// kept as the still-running counterpart: caps apply, full nested tree
// still renders.
func TestAgentToolRenderCapsNestedTools(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	parent := message.ToolCall{ID: "agent-parent", Name: "agent", Input: `{"prompt":"inspect codebase"}`, Finished: false}
	item := NewAgentToolMessageItem(&sty, parent, nil, false, nil)

	for i := 1; i <= 6; i++ {
		id := "tool-" + string(rune('0'+i))
		item.AddNestedTool(mkNestedToolCall(t, &sty, id, "bash", `{"command":"echo `+id+`"}`))
	}
	require.Len(t, item.NestedTools(), 6, "capping display must not truncate the underlying slice")

	out := ansi.Strip(item.Render(120))

	require.Contains(t, out, "+3 earlier steps")
	for i := 4; i <= 6; i++ {
		id := "echo tool-" + string(rune('0'+i))
		require.Contains(t, out, id, "last 3 nested tools must be rendered")
	}
	for i := 1; i <= 3; i++ {
		id := "echo tool-" + string(rune('0'+i))
		require.NotContains(t, out, id, "dropped earlier nested tools must not be rendered")
	}
}

// TestAgentToolRenderNoCapBelowThreshold is the regression guard for the
// cap: with maxVisibleNestedTools or fewer nested tools on a still-running
// delegation, no "+N earlier" summary line should appear at all (e.g.
// never "+0 earlier steps").
func TestAgentToolRenderNoCapBelowThreshold(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	parent := message.ToolCall{ID: "agent-parent", Name: "agent", Input: `{"prompt":"inspect codebase"}`, Finished: false}
	item := NewAgentToolMessageItem(&sty, parent, nil, false, nil)

	item.AddNestedTool(mkNestedToolCall(t, &sty, "tool-1", "bash", `{"command":"echo tool-1"}`))
	item.AddNestedTool(mkNestedToolCall(t, &sty, "tool-2", "bash", `{"command":"echo tool-2"}`))

	out := ansi.Strip(item.Render(120))
	require.NotContains(t, out, "earlier steps")
	require.Contains(t, out, "echo tool-1")
	require.Contains(t, out, "echo tool-2")
}

// TestAgentToolRenderFinishedCollapses is the render-path regression test
// for the "still looks the same as a running one" bug report: a finished
// top-level agent delegation must render as a compact 2-3 line block, not
// the full nested-tool tree plus body content it used to. It must show the
// prompt, an outcome line (steps + tokens), a preview of the result's
// first line, and it must NOT show the nested tool calls or the raw
// "Task" tag/tree formatting that a running (or still-nested) delegation
// uses.
func TestAgentToolRenderFinishedCollapses(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	parent := message.ToolCall{ID: "agent-parent", Name: "agent", Input: `{"prompt":"inspect codebase for bug X"}`, Finished: true}
	result := &message.ToolResult{ToolCallID: "agent-parent", Content: "Found the bug in foo.go.\nMore detail below."}
	item := NewAgentToolMessageItem(&sty, parent, result, false, nil)
	item.AddNestedTool(mkNestedToolCall(t, &sty, "c1", "grep", `{"pattern":"Provider"}`))
	item.AddNestedTool(mkNestedToolCall(t, &sty, "c2", "view", `{"file_path":"foo.go"}`))

	out := ansi.Strip(item.Render(120))
	lines := strings.Split(strings.TrimRight(out, " \n"), "\n")

	require.LessOrEqual(t, len(lines), 3, "finished delegation must collapse to at most 3 lines, got: %q", out)
	require.Contains(t, out, "task", "the built-in agent tool always dispatches to config.AgentTask")
	require.Contains(t, out, "inspect codebase for bug X")
	require.Contains(t, out, "step 2")
	require.Contains(t, out, "Found the bug in foo.go.")
	require.NotContains(t, out, "More detail below.", "only the first line of the result should preview")
	require.NotContains(t, out, "grep", "nested tool calls must not render inline once finished")
	require.NotContains(t, out, "Task", "the running-state Task tag must not render once finished")
}

// TestAgentToolRenderCanceledCollapses covers the canceled variant: same
// compact shape, with "canceled" surfaced in the outcome line instead of a
// result preview (a canceled delegation has no result).
func TestAgentToolRenderCanceledCollapses(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	parent := message.ToolCall{ID: "agent-parent", Name: "agent", Input: `{"prompt":"inspect codebase"}`, Finished: true}
	item := NewAgentToolMessageItem(&sty, parent, nil, true, nil)

	out := ansi.Strip(item.Render(120))
	require.Contains(t, out, "canceled")
	require.NotContains(t, out, "Task")
}

// TestAgentToolToggleExpandedIsNoOp covers the removal of inline
// expansion for agent tools: the full result is only reachable by
// drilling into the child session, not by toggling Expandable.
func TestAgentToolToggleExpandedIsNoOp(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	parent := message.ToolCall{ID: "agent-parent", Name: "agent", Input: `{"prompt":"inspect codebase"}`, Finished: true}
	result := &message.ToolResult{ToolCallID: "agent-parent", Content: "done"}
	item := NewAgentToolMessageItem(&sty, parent, result, false, nil)

	require.False(t, item.ToggleExpanded())
	require.False(t, item.ToggleExpanded(), "must stay false regardless of how many times it's toggled")
}

// TestAgenticFetchToolMessageItem_SetChildSessionTokensBumpsVersion is
// the agentic-fetch counterpart of the token-update bump test above.
func TestAgenticFetchToolMessageItem_SetChildSessionTokensBumpsVersion(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	parent := message.ToolCall{ID: "fetch-parent", Name: "agentic_fetch", Input: `{}`, Finished: false}
	item := NewAgenticFetchToolMessageItem(&sty, parent, nil, false)

	requireBump(t, "SetChildSessionTokens[first update]", item, func() {
		item.SetChildSessionTokens(100, 50)
	})

	before := item.Version()
	item.SetChildSessionTokens(100, 50)
	require.Equal(t, before, item.Version(), "identical token counts must not bump the version")
}

// -----------------------------------------------------------------------------
// Delegation block naming, model/effort subtitle, and child-session todos
// -----------------------------------------------------------------------------

// TestAgentDisplayName_BuiltInTask covers the built-in "agent" tool: since
// AgentParams carries no field identifying a target sub-agent (it always
// dispatches to the fixed config.AgentTask role), the block must render
// "task", not the old hardcoded "Agent".
func TestAgentDisplayName_BuiltInTask(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	parent := message.ToolCall{ID: "agent-parent", Name: "agent", Input: `{"prompt":"inspect"}`, Finished: false}
	item := NewAgentToolMessageItem(&sty, parent, nil, false, nil)

	out := ansi.Strip(item.Render(120))
	require.Contains(t, out, "task")
	require.NotContains(t, out, "Agent")
}

// TestAgentDisplayName_CustomAgentTool covers a user-defined agent tool
// (custom_agent_tool.go registers one per cfg.Agents entry, named after
// the map key): the block must show that name, e.g. "developer", exactly
// as NewToolMessageItem routes it (isCustomAgentTool).
func TestAgentDisplayName_CustomAgentTool(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	cfg := &config.Config{Agents: map[string]config.Agent{"developer": {ID: "developer"}}}
	parent := message.ToolCall{ID: "dev-parent", Name: "developer", Input: `{"prompt":"fix the bug"}`, Finished: false}
	item := NewToolMessageItem(&sty, "msg", parent, nil, false, cfg)

	require.IsType(t, &AgentToolMessageItem{}, item)
	out := ansi.Strip(item.Render(120))
	require.Contains(t, out, "developer")
	require.NotContains(t, out, "Agent")
}

// TestAgenticFetchDisplayName_ShowsFetch covers requirement 1's third
// case: agentic_fetch renders as "fetch", not its longer prettified name
// ("Agentic Fetch", still used elsewhere e.g. clipboard copy).
func TestAgenticFetchDisplayName_ShowsFetch(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	parent := message.ToolCall{ID: "fetch-parent", Name: "agentic_fetch", Input: `{"prompt":"summarize"}`, Finished: false}
	item := NewAgenticFetchToolMessageItem(&sty, parent, nil, false)

	out := ansi.Strip(item.Render(120))
	require.Contains(t, out, "fetch")
	require.NotContains(t, out, "Agentic Fetch")
}

// TestRenderAgentSubtitle_Content covers the model/effort subtitle line:
// both fields render, joined by " · ", in "model · effort X" order.
func TestRenderAgentSubtitle_Content(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	line := ansi.Strip(renderAgentSubtitle(&sty, 200, "qwen36-local/Qwen3-Coder-Next", "high"))
	require.Contains(t, line, "qwen36-local/Qwen3-Coder-Next")
	require.Contains(t, line, "effort high")
	require.Less(t, strings.Index(line, "qwen36-local"), strings.Index(line, "effort high"))
}

// TestRenderAgentSubtitle_Empty covers the "inherits the parent's
// model/effort" case: an agent with neither field configured must render
// no subtitle at all, not an empty styled line.
func TestRenderAgentSubtitle_Empty(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	require.Equal(t, "", renderAgentSubtitle(&sty, 200, "", ""))
}

// TestRenderAgentSubtitle_NarrowWidthDropsProvider covers the fallback for
// a width too narrow for the full "provider/model-id" string: it retries
// with just the model id.
func TestRenderAgentSubtitle_NarrowWidthDropsProvider(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	line := ansi.Strip(renderAgentSubtitle(&sty, 20, "qwen36-local/Qwen3-Coder-Next", ""))
	require.Contains(t, line, "Qwen3-Coder-Next")
	require.NotContains(t, line, "qwen36-local")
}

// TestRenderAgentSubtitle_Truncation guards the same width-bounded
// contract as TestRenderAgentStatusLine_Truncation: the rendered line
// (stripped of ANSI styling) must never exceed the requested width.
func TestRenderAgentSubtitle_Truncation(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	for _, width := range []int{0, 1, 10, 20, 40} {
		line := renderAgentSubtitle(&sty, width, "a-very-long-provider-name/a-very-long-model-id", "high")
		require.LessOrEqualf(t, ansi.StringWidth(ansi.Strip(line)), width,
			"width %d: rendered line exceeds requested width: %q", width, line)
	}
}

// TestAgentToolRender_CustomAgentShowsModelAndEffort is the end-to-end
// render-path test for requirement 2: a custom agent tool configured with
// a model/effort override must show it in the rendered block; one with
// neither field set must show nothing extra.
func TestAgentToolRender_CustomAgentShowsModelAndEffort(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	cfg := &config.Config{Agents: map[string]config.Agent{
		"developer": {ID: "developer", Model: "qwen36-local/Qwen3-Coder-Next", ReasoningEffort: "high"},
	}}
	parent := message.ToolCall{ID: "dev-parent", Name: "developer", Input: `{"prompt":"fix the bug"}`, Finished: false}
	item := NewToolMessageItem(&sty, "msg", parent, nil, false, cfg)

	out := ansi.Strip(item.Render(120))
	require.Contains(t, out, "qwen36-local/Qwen3-Coder-Next")
	require.Contains(t, out, "effort high")
}

// TestAgentToolRender_NoConfigOverrideShowsNoSubtitle covers the built-in
// "agent" tool (no cfg.Agents entry configures its model/effort in this
// test) and a custom agent left at its defaults: neither must show a
// subtitle line, matching "empty → inherits, no clutter".
func TestAgentToolRender_NoConfigOverrideShowsNoSubtitle(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	parent := message.ToolCall{ID: "agent-parent", Name: "agent", Input: `{"prompt":"inspect"}`, Finished: false}
	item := NewAgentToolMessageItem(&sty, parent, nil, false, nil)

	out := ansi.Strip(item.Render(120))
	require.NotContains(t, out, "effort")
}

// TestAgentToolMessageItem_SetChildSessionTodosBumpsVersion is the todos
// counterpart of TestAgentToolMessageItem_SetChildSessionTokensBumpsVersion:
// pushing a new todo list must invalidate the cached render, and a
// repeated identical update must not bump.
func TestAgentToolMessageItem_SetChildSessionTodosBumpsVersion(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	parent := message.ToolCall{ID: "agent-parent", Name: "agent", Input: `{}`, Finished: false}
	item := NewAgentToolMessageItem(&sty, parent, nil, false, nil)

	todos := []session.Todo{{Content: "Do X", Status: session.TodoStatusInProgress, ActiveForm: "Doing X"}}

	requireBump(t, "SetChildSessionTodos[first update]", item, func() {
		item.SetChildSessionTodos(todos)
	})

	before := item.Version()
	item.SetChildSessionTodos(todos)
	require.Equal(t, before, item.Version(), "identical todo list must not bump the version")

	requireBump(t, "SetChildSessionTodos[changed]", item, func() {
		item.SetChildSessionTodos([]session.Todo{{Content: "Do Y", Status: session.TodoStatusPending}})
	})
}

// TestAgenticFetchToolMessageItem_SetChildSessionTodosBumpsVersion is the
// agentic-fetch counterpart of the todos bump test above.
func TestAgenticFetchToolMessageItem_SetChildSessionTodosBumpsVersion(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	parent := message.ToolCall{ID: "fetch-parent", Name: "agentic_fetch", Input: `{}`, Finished: false}
	item := NewAgenticFetchToolMessageItem(&sty, parent, nil, false)

	todos := []session.Todo{{Content: "Do X", Status: session.TodoStatusInProgress}}
	requireBump(t, "SetChildSessionTodos[first update]", item, func() {
		item.SetChildSessionTodos(todos)
	})
}

// TestAgentToolRender_RunningShowsTodos covers requirement 4: a running
// delegation's child-session todos must render, with the in-progress
// item's ActiveForm preferred over its Content.
func TestAgentToolRender_RunningShowsTodos(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	parent := message.ToolCall{ID: "agent-parent", Name: "agent", Input: `{"prompt":"inspect codebase"}`, Finished: false}
	item := NewAgentToolMessageItem(&sty, parent, nil, false, nil)
	item.SetChildSessionTodos([]session.Todo{
		{Content: "Read the file", Status: session.TodoStatusCompleted},
		{Content: "Fix the bug", Status: session.TodoStatusInProgress, ActiveForm: "Fixing the bug"},
		{Content: "Write a test", Status: session.TodoStatusPending},
	})

	out := ansi.Strip(item.Render(120))
	require.Contains(t, out, "Fixing the bug", "in-progress todo must show its ActiveForm")
	require.Contains(t, out, "Write a test")
	require.Contains(t, out, "Read the file")
}

// TestAgentToolRender_FinishedHidesTodos covers requirement 5: once a
// delegation finishes and collapses to its compact summary, its todos
// must not render — only reachable by drilling into the child session.
func TestAgentToolRender_FinishedHidesTodos(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	parent := message.ToolCall{ID: "agent-parent", Name: "agent", Input: `{"prompt":"inspect codebase"}`, Finished: false}
	item := NewAgentToolMessageItem(&sty, parent, nil, false, nil)
	item.SetChildSessionTodos([]session.Todo{
		{Content: "Fix the bug", Status: session.TodoStatusInProgress, ActiveForm: "Fixing the bug"},
	})

	// Finish the delegation the same way the live update path does:
	// SetToolCall(Finished: true) then SetResult.
	finishedTC := parent
	finishedTC.Finished = true
	item.SetToolCall(finishedTC)
	item.SetResult(&message.ToolResult{ToolCallID: parent.ID, Content: "done"})

	out := ansi.Strip(item.Render(120))
	require.NotContains(t, out, "Fixing the bug", "a finished delegation must not render its child-session todos")
}

// TestCapTodosForDelegation covers the compact pane's selection priority:
// every in-progress item is kept, then pending items fill the remaining
// budget, then completed items — dropping completed first since they're
// already done and least useful to a user checking in on progress.
func TestCapTodosForDelegation(t *testing.T) {
	t.Parallel()

	todos := []session.Todo{
		{Content: "done 1", Status: session.TodoStatusCompleted},
		{Content: "done 2", Status: session.TodoStatusCompleted},
		{Content: "active", Status: session.TodoStatusInProgress},
		{Content: "next 1", Status: session.TodoStatusPending},
		{Content: "next 2", Status: session.TodoStatusPending},
		{Content: "next 3", Status: session.TodoStatusPending},
	}

	capped := capTodosForDelegation(todos, 3)
	require.Len(t, capped, 3)

	var contents []string
	for _, td := range capped {
		contents = append(contents, td.Content)
	}
	require.Contains(t, contents, "active", "the in-progress item must always be kept")
	require.NotContains(t, contents, "done 1", "completed items are dropped first")
	require.NotContains(t, contents, "done 2", "completed items are dropped first")

	// Below the cap, nothing is dropped.
	require.Equal(t, todos, capTodosForDelegation(todos, len(todos)))
}
