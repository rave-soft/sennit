package chat

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	tools "github.com/rave-soft/sennit/internal/proto"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/presentation"
	"github.com/rave-soft/sennit/internal/ui/styles"
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
		require.Equal(t, tt.want, presentation.FormatElapsed(tt.d))
	}
}

func TestAgentDelegationWholeItemIsHoverable(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	item := NewAgentToolMessageItem(&sty, message.ToolCall{
		ID:       "agent-hover",
		Name:     "agent",
		Input:    `{"prompt":"inspect this"}`,
		Finished: false,
	}, nil, false, nil)
	height := lipgloss.Height(item.Render(80))

	require.True(t, item.HoverableAt(MessageLeftPaddingTotal, 0, 80))
	require.True(t, item.HoverableAt(MessageLeftPaddingTotal, height-1, 80))
	require.False(t, item.HoverableAt(MessageLeftPaddingTotal, height, 80))

	version := item.Version()
	item.SetHovered(true)
	require.True(t, item.hovered)
	require.Greater(t, item.Version(), version)
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

	sty := styles.SennitDark()
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

// TestPanelStatusLine_MatchesUnderlyingStatusLine covers the new
// PanelLiveActivityProvider accessor the session panel's delegations
// section (internal/ui/model/session_panel.go) uses: it must reuse the
// exact same renderAgentStatusLine formatting/data (elapsed, step count,
// last tool, tokens) the old inline pending render used, for both
// AgentToolMessageItem and AgenticFetchToolMessageItem, with no extra IO —
// everything comes from fields already pushed in via AddNestedTool /
// SetChildSessionTokens.
func TestPanelStatusLine_MatchesUnderlyingStatusLine(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()

	agentItem := NewAgentToolMessageItem(&sty, message.ToolCall{ID: "a1", Name: "agent", Input: `{}`, Finished: false}, nil, false, nil)
	agentItem.startTime = time.Now().Add(-30 * time.Second)
	agentItem.AddNestedTool(mkNestedToolCall(t, &sty, "c1", "bash", `{"command":"echo hi"}`))
	agentItem.SetChildSessionTokens(100, 50)

	var provider PanelLiveActivityProvider = agentItem
	line := ansi.Strip(provider.PanelStatusLine(&sty, 200))
	require.Contains(t, line, "30s")
	require.Contains(t, line, "step 1")
	require.Contains(t, line, "150 tok")

	fetchItem := NewAgenticFetchToolMessageItem(&sty, message.ToolCall{ID: "f1", Name: "agentic_fetch", Input: `{}`, Finished: false}, nil, false)
	fetchItem.startTime = time.Now().Add(-5 * time.Second)
	var fetchProvider PanelLiveActivityProvider = fetchItem
	fline := ansi.Strip(fetchProvider.PanelStatusLine(&sty, 200))
	require.Contains(t, fline, "5s")
	require.Contains(t, fline, "step 0")
}

// TestRenderAgentStatusLine_NoNestedTools covers the very first seconds
// of a delegation, before any child-session event has arrived. Elapsed
// time and step count must still render — this is what keeps the first
// stretch of a run from looking identical to a hang.
func TestRenderAgentStatusLine_NoNestedTools(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
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

	sty := styles.SennitDark()
	nested := []ToolMessageItem{
		mkNestedToolCall(t, &sty, "c1", "grep", `{"pattern":"a very very long pattern that should get cut off","path":"internal/config/somewhere/deep"}`),
	}

	for _, width := range []int{0, 1, 10, 20, 40} {
		line := renderAgentStatusLine(&sty, width, time.Now(), nested, 0, 0)
		require.LessOrEqualf(t, ansi.StringWidth(ansi.Strip(line)), width,
			"width %d: rendered line exceeds requested width: %q", width, line)
	}
}

// TestAgentToolMessageItem_PendingShowsCurrentActivity covers the running
// delegation's transcript render: a pending stub (name + spinner) with one
// status line underneath showing elapsed time, step count, and the current
// child tool call — so what the task is doing is visible without opening
// the session panel. Deeper detail (todos, full nested-tool tree) still
// belongs to the panel only.
func TestAgentToolMessageItem_PendingShowsCurrentActivity(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	parent := message.ToolCall{ID: "agent-parent", Name: "agent", Input: `{"prompt":"inspect codebase"}`, Finished: false}
	item := NewAgentToolMessageItem(&sty, parent, nil, false, nil)
	item.startTime = time.Now().Add(-9 * time.Second)

	out := ansi.Strip(item.Render(120))
	require.Contains(t, out, "9s", "elapsed time must show under the pending stub")
	require.Contains(t, out, "step 0")

	item.AddNestedTool(mkNestedToolCall(t, &sty, "c1", "grep", `{"pattern":"Provider","path":"internal/config"}`))
	out = ansi.Strip(item.Render(120))
	require.Contains(t, out, "grep",
		"the current child tool call must show in the status line")
	require.Len(t, strings.Split(strings.TrimRight(out, " \n"), "\n"), 2,
		"pending render is exactly stub + one status line")
}

func TestAgentToolMessageItem_FinishedInputWithoutResultShowsCurrentActivity(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	item := NewAgentToolMessageItem(&sty,
		message.ToolCall{ID: "tc-agent", Name: "agent", Input: `{"prompt":"fix it"}`, Finished: true},
		nil, false, nil)
	item.AddNestedTool(mkNestedToolCall(t, &sty, "tc-child", "grep", `{"pattern":"Provider"}`))

	out := ansi.Strip(item.Render(100))
	require.Contains(t, out, "task")
	require.Contains(t, out, `→ grep "Provider"`)
}

// TestAgenticFetchToolMessageItem_FinishedInputWithoutResultShowsCurrentActivity
// mirrors TestAgentToolMessageItem_FinishedInputWithoutResultShowsCurrentActivity
// for agentic_fetch. ToolCall.Finished only means the model finished
// streaming the tool call's *arguments* — a delegation that has no result
// yet must still render the pending/spinner form, not the collapsed
// finished summary. Before delegationStillRunning replaced the bare
// opts.IsPending() check in AgenticFetchToolRenderContext.RenderTool, this
// case rendered as a collapsed, finished-looking block with no spinner for
// the whole (possibly multi-minute) run.
func TestAgenticFetchToolMessageItem_FinishedInputWithoutResultShowsCurrentActivity(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	item := NewAgenticFetchToolMessageItem(&sty,
		message.ToolCall{ID: "tc-fetch", Name: tools.AgenticFetchToolName, Input: `{"url":"https://example.com","prompt":"summarize"}`, Finished: true},
		nil, false)
	item.AddNestedTool(mkNestedToolCall(t, &sty, "tc-child", "grep", `{"pattern":"Provider"}`))

	out := ansi.Strip(item.Render(100))
	require.Contains(t, out, "fetch")
	require.Contains(t, out, `→ grep "Provider"`,
		"still-running agentic_fetch must show the pending status line, not the collapsed summary")
}

// TestAgentToolMessageItem_SetChildSessionTokensBumpsVersion covers the
// live token-count update path (handleChildSessionUpdate in
// internal/ui/model/ui.go): pushing a new token count must invalidate
// the cached render, and a repeated identical update must not bump
// (matching the no-op dedupe on every other setter in this file).
func TestAgentToolMessageItem_SetChildSessionTokensBumpsVersion(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
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

// TestAgentToolRenderPending_ManyNestedToolsStayOneStatusLine confirms the
// transcript render stays compact no matter how many child tool calls have
// landed: a stub line plus a single status line naming only the latest
// call — never an inline nested-tool tree or "+N earlier steps" cap (that
// detail lives in the session panel).
func TestAgentToolRenderPending_ManyNestedToolsStayOneStatusLine(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	parent := message.ToolCall{ID: "agent-parent", Name: "agent", Input: `{"prompt":"inspect codebase"}`, Finished: false}
	item := NewAgentToolMessageItem(&sty, parent, nil, false, nil)

	for i := 1; i <= 6; i++ {
		id := "tool-" + string(rune('0'+i))
		item.AddNestedTool(mkNestedToolCall(t, &sty, id, "bash", `{"command":"echo `+id+`"}`))
	}
	require.Len(t, item.NestedTools(), 6, "the underlying slice is still tracked, for PanelStatusLine/the panel block")

	out := ansi.Strip(item.Render(120))
	require.NotContains(t, out, "earlier steps")
	require.Contains(t, out, "step 6")
	require.Contains(t, out, "echo tool-6", "only the latest child call shows")
	require.NotContains(t, out, "echo tool-5", "earlier calls must not accumulate")
	lines := strings.Split(strings.TrimRight(out, " \n"), "\n")
	require.Len(t, lines, 2, "pending transcript render is exactly stub + one status line")
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

	sty := styles.SennitDark()
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

	sty := styles.SennitDark()
	parent := message.ToolCall{ID: "agent-parent", Name: "agent", Input: `{"prompt":"inspect codebase"}`, Finished: true}
	item := NewAgentToolMessageItem(&sty, parent, nil, true, nil)

	out := ansi.Strip(item.Render(120))
	require.Contains(t, out, "canceled")
	require.NotContains(t, out, "Task")
}

// TestAgentToolRenderBackgroundDispatch covers a background agent-tool
// dispatch (tools.AgentParams.Background): runBackgroundAgent returns
// synchronously with an acknowledgment, so HasResult is true immediately —
// this must render as a distinct "just dispatched" block, not fall into
// renderCollapsedDelegation's finished-with-an-answer shape.
func TestAgentToolRenderBackgroundDispatch(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	parent := message.ToolCall{
		ID:       "agent-bg",
		Name:     "agent",
		Input:    `{"prompt":"scan the repo for TODOs","background":true}`,
		Finished: true,
	}
	metaJSON, err := json.Marshal(tools.AgentBackgroundResponseMetadata{
		TaskID:    "t1",
		SessionID: "s1",
		Status:    "running",
	})
	require.NoError(t, err)
	result := &message.ToolResult{
		ToolCallID: "agent-bg",
		Content:    "Started background task t1 (session s1, status=running). It is running independently; its result will follow separately.",
		Metadata:   string(metaJSON),
	}
	item := NewAgentToolMessageItem(&sty, parent, result, false, nil)

	out := ansi.Strip(item.Render(120))

	// Two lines: the agent and what it was asked to do, then how long it
	// has been at it.
	require.Contains(t, out, "scan the repo for TODOs")
	require.Len(t, strings.Split(strings.TrimRight(out, " \n"), "\n"), 2)
	// No badge and no task uuid — see renderBackgroundDispatch on why
	// neither earned its line.
	require.NotContains(t, out, "background")
	require.NotContains(t, out, "t1")
	// ...and the ack text is not shown as if it were a finished result
	// preview, which is what renderCollapsedDelegation would have made of
	// it. The live second line reports the work still going; it is not
	// that block's outcome-and-result shape.
	require.NotContains(t, out, "It is running independently")

	// The tool call itself is genuinely finished — never mistaken for
	// still streaming.
	require.False(t, item.isSpinning())
	require.True(t, item.Finished())
}

// TestAgentToolRenderPending_ShowsElapsedAndTokens pins the shape of a
// running delegation in the transcript: two lines, the second saying how
// long it has been going and what it has spent. A block that only names
// the agent answers nothing about a task four minutes in, and the session
// panel is not always on screen to answer it instead.
func TestAgentToolRenderPending_ShowsElapsedAndTokens(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	parent := message.ToolCall{ID: "agent-parent", Name: "agent", Input: `{"prompt":"inspect codebase"}`, Finished: false}
	item := NewAgentToolMessageItem(&sty, parent, nil, false, nil)
	item.AddNestedTool(mkNestedToolCall(t, &sty, "tool-1", "bash", `{"command":"echo hi"}`))
	item.SetChildSessionTokens(120_000, 3_000)

	out := ansi.Strip(item.Render(120))
	lines := strings.Split(strings.TrimRight(out, " \n"), "\n")
	require.Len(t, lines, 2, "a running delegation is stub plus one live line")
	require.Contains(t, lines[1], "step 1")
	require.Contains(t, lines[1], "tok", "the live line must say what the delegation has spent")
}

// TestDelegationHeadline covers the header line a delegation shows beside
// the agent's name. The case it exists for is a structured prompt, whose
// literal first line ("ROLE: reviewer") names the agent the header has
// just named and says nothing about the work.
func TestDelegationHeadline(t *testing.T) {
	t.Parallel()

	const pipelinePrompt = "ROLE: reviewer\n" +
		"TASK: keep LSP restarts isolated from stale runtime state\n" +
		"ORIGINAL USER REQUEST:\n" +
		"the LSP dies after a restart\n"

	for _, tc := range []struct {
		name   string
		agent  string
		prompt string
		want   string
	}{
		{
			name:   "structured prompt skips the label that repeats the agent",
			agent:  "reviewer",
			prompt: pipelinePrompt,
			want:   "keep LSP restarts isolated from stale runtime state",
		},
		{
			name:   "a label whose value is not the agent's name is still the answer",
			agent:  "developer",
			prompt: pipelinePrompt,
			want:   "reviewer",
		},
		{
			name:   "a label with no value is skipped too",
			agent:  "reviewer",
			prompt: "ROLE:\nTASK: split the C4 packages\n",
			want:   "split the C4 packages",
		},
		{
			name:   "prose with a colon is not a label",
			agent:  "developer",
			prompt: "Fix this: the parser drops newlines\nsecond line\n",
			want:   "Fix this: the parser drops newlines",
		},
		{
			name:   "a label in another script still counts",
			agent:  "reviewer",
			prompt: "РОЛЬ: reviewer\nЗАДАЧА: убрать вторую строчку\n",
			want:   "убрать вторую строчку",
		},
		{
			name:   "an ordinary prompt keeps its first line",
			agent:  "task",
			prompt: "scan the repo for TODOs\nand report them\n",
			want:   "scan the repo for TODOs",
		},
		{
			name:   "leading blank lines are not the headline",
			agent:  "task",
			prompt: "\n\n  scan the repo  \n",
			want:   "scan the repo",
		},
		{
			name:   "nothing but redundant labels falls back to the first line",
			agent:  "reviewer",
			prompt: "ROLE: reviewer\n",
			want:   "ROLE: reviewer",
		},
		{
			name:   "an empty prompt stays empty",
			agent:  "task",
			prompt: "",
			want:   "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, delegationHeadline(tc.agent, tc.prompt))
		})
	}
}

// TestAgentToolToggleExpandedIsNoOp covers the removal of inline
// expansion for agent tools: the full result is only reachable by
// drilling into the child session, not by toggling Expandable.
func TestAgentToolToggleExpandedIsNoOp(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
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

	sty := styles.SennitDark()
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

	sty := styles.SennitDark()
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

	sty := styles.SennitDark()
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

	sty := styles.SennitDark()
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

	sty := styles.SennitDark()
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

	sty := styles.SennitDark()
	require.Equal(t, "", renderAgentSubtitle(&sty, 200, "", ""))
}

// TestRenderAgentSubtitle_NarrowWidthDropsProvider covers the fallback for
// a width too narrow for the full "provider/model-id" string: it retries
// with just the model id.
func TestRenderAgentSubtitle_NarrowWidthDropsProvider(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	line := ansi.Strip(renderAgentSubtitle(&sty, 20, "qwen36-local/Qwen3-Coder-Next", ""))
	require.Contains(t, line, "Qwen3-Coder-Next")
	require.NotContains(t, line, "qwen36-local")
}

// TestRenderAgentSubtitle_Truncation guards the same width-bounded
// contract as TestRenderAgentStatusLine_Truncation: the rendered line
// (stripped of ANSI styling) must never exceed the requested width.
func TestRenderAgentSubtitle_Truncation(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
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

	sty := styles.SennitDark()
	cfg := &config.Config{Agents: map[string]config.Agent{
		"developer": {ID: "developer", Model: "qwen36-local/Qwen3-Coder-Next", ReasoningEffort: "high"},
	}}
	// A pending delegation now renders just the bare stub (see
	// TestAgentToolMessageItem_PendingRendersBareStub) — the model/effort
	// subtitle only shows once finished, in the collapsed summary.
	parent := message.ToolCall{ID: "dev-parent", Name: "developer", Input: `{"prompt":"fix the bug"}`, Finished: true}
	result := &message.ToolResult{ToolCallID: "dev-parent", Content: "done"}
	item := NewToolMessageItem(&sty, "msg", parent, result, false, cfg)

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

	sty := styles.SennitDark()
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

	sty := styles.SennitDark()
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

	sty := styles.SennitDark()
	parent := message.ToolCall{ID: "fetch-parent", Name: "agentic_fetch", Input: `{}`, Finished: false}
	item := NewAgenticFetchToolMessageItem(&sty, parent, nil, false)

	todos := []session.Todo{{Content: "Do X", Status: session.TodoStatusInProgress}}
	requireBump(t, "SetChildSessionTodos[first update]", item, func() {
		item.SetChildSessionTodos(todos)
	})
}

// TestAgentToolRender_RunningHidesTodosFromTranscript covers the flip side
// of TestAgentToolRender_FinishedHidesTodos: a *running* delegation's
// child-session todos must not render in the chat transcript either — the
// panel is the only place a running delegation's live todos show now (via
// PanelStatusLine's caller in internal/ui/model), matching the pending
// stub's "just the header" behavior.
func TestAgentToolRender_RunningHidesTodosFromTranscript(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	parent := message.ToolCall{ID: "agent-parent", Name: "agent", Input: `{"prompt":"inspect codebase"}`, Finished: false}
	item := NewAgentToolMessageItem(&sty, parent, nil, false, nil)
	item.SetChildSessionTodos([]session.Todo{
		{Content: "Read the file", Status: session.TodoStatusCompleted},
		{Content: "Fix the bug", Status: session.TodoStatusInProgress, ActiveForm: "Fixing the bug"},
		{Content: "Write a test", Status: session.TodoStatusPending},
	})

	out := ansi.Strip(item.Render(120))
	require.NotContains(t, out, "Fixing the bug")
	require.NotContains(t, out, "Write a test")
	require.NotContains(t, out, "Read the file")
}

// TestAgentToolRender_FinishedHidesTodos covers requirement 5: once a
// delegation finishes and collapses to its compact summary, its todos
// must not render — only reachable by drilling into the child session.
func TestAgentToolRender_FinishedHidesTodos(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
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
		{Content: "active 1", Status: session.TodoStatusInProgress},
		{Content: "unknown", Status: "future"},
		{Content: "pending", Status: session.TodoStatusPending},
		{Content: "active 2", Status: session.TodoStatusInProgress},
		{Content: "done 2", Status: session.TodoStatusCompleted},
	}
	names := func(todos []session.Todo) []string {
		out := make([]string, len(todos))
		for i, todo := range todos {
			out[i] = todo.Content
		}
		return out
	}

	require.Equal(t, []string{"active 1", "active 2", "unknown"}, names(capTodosForDelegation(todos, 3)))
	require.Equal(t, []string{"active 1", "active 2", "unknown", "pending", "done 1", "done 2"}, names(capTodosForDelegation(todos, len(todos))))

	// In-progress rows are never sacrificed to the cap, even when their count
	// exceeds it.
	require.Equal(t, []string{"active 1", "active 2"}, names(capTodosForDelegation(todos, 1)))
}

// TestDelegationItemsKeepTheirRestylableAndAnimatableContracts pins the
// two delegation blocks against the shared embedding they were collapsed
// into. Both satisfy these through promoted methods now, and a promotion
// that silently stops applying — an override added on one type, a field
// renamed — is not something the compiler complains about anywhere else.
func TestDelegationItemsKeepTheirRestylableAndAnimatableContracts(t *testing.T) {
	t.Parallel()

	var (
		_ Restylable = (*AgentToolMessageItem)(nil)
		_ Restylable = (*AgenticFetchToolMessageItem)(nil)
		_ Animatable = (*AgentToolMessageItem)(nil)
		_ Animatable = (*AgenticFetchToolMessageItem)(nil)
		_ Expandable = (*AgentToolMessageItem)(nil)
		_ Expandable = (*AgenticFetchToolMessageItem)(nil)
	)
}

// TestExtractMessageItems_RestoresDelegationStartTime pins the fix for a
// running delegation whose elapsed time restarted from zero on every
// session load. Chat items are rebuilt from stored messages on each load —
// and navigating into a sub-agent session and back out performs two of
// them — so a delegation item that anchors its elapsed time to
// time.Now() at construction forgets how long it has actually been
// running. The assistant message that issued the tool call carries the
// real start.
func TestExtractMessageItems_RestoresDelegationStartTime(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	start := time.Now().Add(-7 * time.Minute).Truncate(time.Second)
	msg := &message.Message{
		ID:        "m1",
		Role:      message.Assistant,
		CreatedAt: start.Unix(),
		Parts: []message.ContentPart{
			message.ToolCall{ID: "tc-1", Name: "agent", Input: `{"prompt":"fix the bug"}`},
		},
	}

	items := ExtractMessageItems(&sty, msg, nil, nil)
	require.Len(t, items, 1)

	provider, ok := items[0].(DelegationInfoProvider)
	require.True(t, ok, "an agent tool call must rebuild as a delegation item")
	_, _, _, gotStart, _ := provider.DelegationInfo()
	require.WithinDuration(t, start, gotStart, time.Second,
		"the delegation must keep the start time recorded on its message, not restart at load time")
}

// TestExtractMessageItems_RestoresDelegationDuration: a delegation
// rebuilt from history used to lose its runtime entirely — duration is
// frozen by SetResult, which a rebuilt item never gets, since its result
// arrives at construction. The tool message carrying that result knows
// when it landed.
func TestExtractMessageItems_RestoresDelegationDuration(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	start := time.Now().Add(-30 * time.Minute).Truncate(time.Second)
	end := start.Add(4 * time.Minute)
	msg := &message.Message{
		ID:        "m1",
		Role:      message.Assistant,
		CreatedAt: start.Unix(),
		Parts: []message.ContentPart{
			message.ToolCall{ID: "tc-1", Name: "agent", Input: `{"prompt":"fix the bug"}`},
		},
	}
	results := map[string]ToolResultRef{
		"tc-1": {Result: message.ToolResult{ToolCallID: "tc-1", Content: "done"}, CreatedAt: end.Unix()},
	}

	items := ExtractMessageItems(&sty, msg, results, nil)
	require.Len(t, items, 1)
	provider, ok := items[0].(DelegationInfoProvider)
	require.True(t, ok)
	_, _, _, gotStart, gotDuration := provider.DelegationInfo()
	require.WithinDuration(t, start, gotStart, time.Second)
	require.Equal(t, 4*time.Minute, gotDuration,
		"a finished delegation must report the runtime its two messages bracket")
}

// TestExtractMessageItems_SubSecondDelegationReportsNoDuration: message
// timestamps have second granularity, so a delegation that starts and
// finishes inside the same second has no runtime this can express.
// Duration must stay zero ("unknown") rather than become a misleading
// "0s".
func TestExtractMessageItems_SubSecondDelegationReportsNoDuration(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	at := time.Now().Add(-time.Minute).Truncate(time.Second)
	msg := &message.Message{
		ID:        "m1",
		Role:      message.Assistant,
		CreatedAt: at.Unix(),
		Parts: []message.ContentPart{
			message.ToolCall{ID: "tc-1", Name: "agent", Input: `{"prompt":"fix the bug"}`},
		},
	}
	results := map[string]ToolResultRef{
		"tc-1": {Result: message.ToolResult{ToolCallID: "tc-1"}, CreatedAt: at.Unix()},
	}

	items := ExtractMessageItems(&sty, msg, results, nil)
	require.Len(t, items, 1)
	provider, ok := items[0].(DelegationInfoProvider)
	require.True(t, ok)
	_, _, _, _, gotDuration := provider.DelegationInfo()
	require.Zero(t, gotDuration)
}

// TestExtractMessageItems_IgnoresMissingCreatedAt: a message with no
// stored timestamp must leave the construction-time default alone rather
// than dating the delegation to the Unix epoch, which would render as
// decades of elapsed time.
func TestExtractMessageItems_IgnoresMissingCreatedAt(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	msg := &message.Message{
		ID:   "m1",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{ID: "tc-1", Name: "agent", Input: `{"prompt":"fix the bug"}`},
		},
	}

	items := ExtractMessageItems(&sty, msg, nil, nil)
	require.Len(t, items, 1)
	provider, ok := items[0].(DelegationInfoProvider)
	require.True(t, ok)
	_, _, _, gotStart, _ := provider.DelegationInfo()
	require.WithinDuration(t, time.Now(), gotStart, time.Minute)
}
