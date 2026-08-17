package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/message"
	tools "github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// toolsHandledOutsideFactoryMap lists built-in tools with a dedicated
// renderer that NewToolMessageItem special-cases before consulting
// toolRenderers, instead of registering a map entry — because their
// constructor needs an argument (cfg, to resolve a delegation's display
// name and model/effort override) that the registry's signature has no
// room for. See the comment on tools.AgentToolName in
// registerAgentToolRenderers.
var toolsHandledOutsideFactoryMap = []string{
	tools.AgentToolName,
}

// toolsWithoutDedicatedRenderer lists the built-in tools (from
// config.AllToolNames, the actual source of truth for what tools exist)
// that intentionally have no entry in toolRenderers and fall back to the
// generic renderer. Anything not on this list (and not in
// toolsHandledOutsideFactoryMap) must have a dedicated renderer.
var toolsWithoutDedicatedRenderer = []string{
	tools.SennitInfoToolName,
	tools.SennitLogsToolName,
	tools.ListMCPResourcesToolName,
	tools.ReadMCPResourceToolName,
	// Thread tools (domain/agent/tools/thread_*.go) don't have a
	// dedicated renderer yet; they fall back to the generic one until the
	// TUI grows one.
	tools.ThreadCreateToolName,
	tools.ThreadListToolName,
	tools.ThreadStatusToolName,
	tools.ThreadSendToolName,
	tools.ThreadWaitToolName,
	tools.ThreadMergeToolName,
	tools.ThreadRemoveToolName,
	// Task tools (domain/agent/tools/task_*.go), same story: no
	// dedicated renderer yet.
	tools.TaskListToolName,
	tools.TaskResultToolName,
	tools.TaskCancelToolName,
	tools.TaskSendToolName,
	tools.TaskOutputToolName,
	// ask_parent (domain/agent/tools/ask_parent.go), same story: no
	// dedicated renderer yet.
	tools.AskParentToolName,
}

// TestToolMessageItemFactories_MatchExpectedNames checks
// toolRenderers against config.AllToolNames instead of a
// second, hand-maintained list of tool names living in this test file.
// Two hand-maintained lists drift silently: a new tool can be added to
// allToolNames() without anyone remembering to update a duplicate here,
// which defeats the entire point of this test — catching a tool that
// falls through to the generic renderer unnoticed.
func TestToolMessageItemFactories_MatchExpectedNames(t *testing.T) {
	t.Parallel()

	noRenderer := make(map[string]bool, len(toolsWithoutDedicatedRenderer))
	for _, name := range toolsWithoutDedicatedRenderer {
		noRenderer[name] = true
	}
	specialCased := make(map[string]bool, len(toolsHandledOutsideFactoryMap))
	for _, name := range toolsHandledOutsideFactoryMap {
		specialCased[name] = true
	}

	for _, name := range config.AllToolNames() {
		if specialCased[name] {
			require.NotContainsf(t, toolRenderers, name,
				"tool %q is listed in toolsHandledOutsideFactoryMap but also has a registry entry; the entry is unreachable dead code, remove one or the other", name)
			continue
		}
		if noRenderer[name] {
			require.NotContainsf(t, toolRenderers, name,
				"tool %q is listed as having no dedicated renderer, but one is registered; remove it from toolsWithoutDedicatedRenderer", name)
			continue
		}
		require.Containsf(t, toolRenderers, name,
			"tool %q has no registered renderer and will fall back to the generic renderer", name)
	}

	// toolsHandledOutsideFactoryMap's whole premise is that
	// NewToolMessageItem special-cases these tools before the map lookup —
	// verify that dispatch actually happens, not just that the map omits
	// them (which noRenderer-listed tools also do, for the opposite
	// reason).
	sty := styles.SennitDark()
	item := NewToolMessageItem(&sty, "msg", message.ToolCall{ID: "tc-agent", Name: tools.AgentToolName, Input: "{}"}, nil, false, nil)
	require.IsType(t, &AgentToolMessageItem{}, item,
		"the built-in agent tool must dispatch to AgentToolMessageItem, not fall back to the generic renderer")

	// Every registered factory must correspond to a real, known tool —
	// this catches dead renderer registrations for tools that no longer
	// exist, the same kind of debris a previously removed built-in tool
	// left behind elsewhere in the repo.
	//
	// A renamed tool's old name is not debris: sessions recorded before
	// the rename still hold calls under it and must keep rendering, so a
	// registration is also legitimate when the name folds onto a known
	// tool (see config.CanonicalToolName).
	known := make(map[string]bool, len(config.AllToolNames()))
	for _, name := range config.AllToolNames() {
		known[name] = true
	}
	for name := range toolRenderers {
		require.Truef(t, known[name] || known[config.CanonicalToolName(name)],
			"tool %q has a registered renderer but is neither in config.AllToolNames() nor a legacy name of a tool that is", name)
	}
}

// TestNewToolMessageItem_RendersALegacyToolName covers reopening a session
// recorded before the read tool was renamed: its calls are stored under the
// old name, and a name the renderer no longer knows would fall through to
// the generic renderer — the history would visibly degrade just by having
// been recorded a version earlier.
func TestNewToolMessageItem_RendersALegacyToolName(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	item := NewToolMessageItem(&sty, "msg-1", message.ToolCall{
		ID:       "tc-1",
		Name:     tools.LegacyReadToolName,
		Input:    `{"file_path":"internal/foo.go"}`,
		Finished: true,
	}, nil, false, nil)

	registered, ok := item.(*baseToolMessageItem)
	require.Truef(t, ok, "a legacy tool name must reach a registered renderer, got %T", item)
	require.IsType(t, &ReadToolRenderContext{}, registered.toolRenderer,
		"a legacy read call must render through the read renderer, not the generic one")
}

func TestNewToolMessageItem_SearchRendererTitles(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	for _, test := range []struct {
		name      string
		toolName  string
		wantTitle string
	}{
		{name: "grep", toolName: tools.GrepToolName, wantTitle: "Grep"},
		{name: "ripgrep", toolName: tools.RipgrepToolName, wantTitle: "Ripgrep"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			item := NewToolMessageItem(&sty, "msg", message.ToolCall{
				ID:       test.name,
				Name:     test.toolName,
				Input:    `{"pattern":"needle"}`,
				Finished: true,
			}, nil, false, nil)

			require.Contains(t, item.Render(80), test.wantTitle)
		})
	}
}

// TestNewToolMessageItem_CustomAgentDispatch covers the documented gap
// closed here: a user-defined agent tool (domain/agent/custom_agent_tool.go
// registers one per entry in cfg.Agents, named after the agent's id) must
// get the same AgentToolMessageItem renderer as the built-in "agent" tool —
// status line, collapse-on-finish, click-to-drill — not the generic
// fallback. "coder" and "task" are config.Agents entries too, but they're
// roles, not tools a model can call, so they must never dispatch here.
func TestNewToolMessageItem_CustomAgentDispatch(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	cfg := &config.Config{
		Agents: map[string]config.Agent{
			config.AgentCoder: {ID: config.AgentCoder},
			config.AgentTask:  {ID: config.AgentTask},
			"reviewer":        {ID: "reviewer", Name: "Reviewer"},
		},
	}

	item := NewToolMessageItem(&sty, "msg1",
		message.ToolCall{ID: "tc-1", Name: "reviewer", Input: `{"prompt":"review the diff"}`, Finished: false}, nil, false, cfg)
	require.IsType(t, &AgentToolMessageItem{}, item,
		"a tool call named after a config.Agents entry must dispatch to the agent renderer")

	// "coder"/"task" are agent roles, not callable tool names — a stray
	// tool call with that name (shouldn't happen, but mustn't be
	// misrendered as a delegation) falls back to the generic renderer.
	roleItem := NewToolMessageItem(&sty, "msg1",
		message.ToolCall{ID: "tc-2", Name: config.AgentCoder, Input: `{}`, Finished: false}, nil, false, cfg)
	require.NotEqual(t, &AgentToolMessageItem{}, roleItem, "coder/task must not dispatch to the agent renderer")
	_, isAgentItem := roleItem.(*AgentToolMessageItem)
	require.False(t, isAgentItem, "coder/task must not dispatch to the agent renderer")

	// A nil cfg (e.g. some call sites that don't have config handy) must
	// never dispatch to the agent renderer, only fall back to generic.
	nilCfgItem := NewToolMessageItem(&sty, "msg1",
		message.ToolCall{ID: "tc-3", Name: "reviewer", Input: `{}`, Finished: false}, nil, false, nil)
	_, isAgentItem = nilCfgItem.(*AgentToolMessageItem)
	require.False(t, isAgentItem, "nil cfg must never dispatch a tool name to the agent renderer")

	// An unrelated tool name that happens to not match any config.Agents
	// entry keeps falling back to the generic renderer, unaffected.
	genericItem := NewToolMessageItem(&sty, "msg1",
		message.ToolCall{ID: "tc-4", Name: "some_random_tool", Input: `{}`, Finished: false}, nil, false, cfg)
	_, isAgentItem = genericItem.(*AgentToolMessageItem)
	require.False(t, isAgentItem)
}

// TestOneLine covers the general multi-line-param normalization every
// toolParamList caller gets for free: embedded newlines, tabs, CRLF, and
// repeated whitespace all collapse to single spaces, with ends trimmed.
func TestOneLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello world", "hello world"},
		{"newlines", "line1\nline2\nline3", "line1 line2 line3"},
		{"crlf", "line1\r\nline2", "line1 line2"},
		{"tabs and repeats", "a\t\tb   c", "a b c"},
		{"leading/trailing whitespace", "\n  hello  \n", "hello"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, oneLine(tt.in))
		})
	}
}

// TestToolParamList_MultilineMainParamStaysOneLine covers a generic tool
// (not just Bash) with a multi-line main param: the rendered header must
// never contain a literal newline, regardless of how the caller built
// params — toolParamList normalizes centrally.
func TestToolParamList_MultilineMainParamStaysOneLine(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	out := toolParamList(&sty, []string{"line one\nline two\nline three"}, 80)
	require.NotContains(t, out, "\n")
	require.Contains(t, out, "line one line two line three")
}

// TestAppendResultSummary_NeverPrintsJunkPlaceholder covers the "None"
// regression directly at the shared helper every one-line tool renderer
// funnels its outcome suffix through: a junk placeholder value (whatever
// its source) must be treated exactly like "" — omitted, not printed.
func TestAppendResultSummary_NeverPrintsJunkPlaceholder(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	header := "header"

	for _, junk := range []string{"", "None", "none", "  NULL  ", "n/a", "N/A", "nil", "-", "undefined"} {
		out := appendResultSummary(&sty, header, junk)
		require.Equal(t, header, out, "junk summary %q must be omitted entirely", junk)
	}

	out := appendResultSummary(&sty, header, "2.1s")
	require.NotEqual(t, header, out, "a real summary must still be appended")
	require.Contains(t, out, "2.1s")
}

// TestToolParamList_LongPathKeepsFileName is the point of path-aware
// truncation: a header narrow enough to cut the path must lose the head of
// it, not the file name. "…/tools_render.go" says which file; the old
// right truncation produced "internal/ui/chat/tool…", which says none.
func TestToolParamList_LongPathKeepsFileName(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	path := "internal/ui/chat/very/deeply/nested/package/tools_render.go"

	out := ansi.Strip(toolParamList(&sty, []string{path}, 30))
	require.LessOrEqual(t, ansi.StringWidth(out), 30)
	require.Contains(t, out, "tools_render.go")
	require.True(t, strings.HasPrefix(out, "…"), "the head of the path is what gets elided: %q", out)
}

// TestToolParamList_LongPathWithParamsKeepsFileName covers the same rule
// when key=value pairs share the line: the main param is fitted to its own
// budget, so the pairs cannot push the file name off the end.
func TestToolParamList_LongPathWithParamsKeepsFileName(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	path := "internal/ui/chat/very/deeply/nested/package/tools_render.go"

	out := ansi.Strip(toolParamList(&sty, []string{path, "edits", "3"}, 60))
	require.LessOrEqual(t, ansi.StringWidth(out), 60)
	require.Contains(t, out, "tools_render.go")
	require.Contains(t, out, "edits=3")
}

// TestToolParamList_NonPathTruncatesOnTheRight proves the rule is scoped to
// paths: a bash command carries its meaning in the head, so it keeps the
// ordinary right truncation.
func TestToolParamList_NonPathTruncatesOnTheRight(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	cmd := "go test ./internal/ui/chat/ -run TestSomethingWithAVeryLongName -timeout 120s"

	out := ansi.Strip(toolParamList(&sty, []string{cmd}, 30))
	require.LessOrEqual(t, ansi.StringWidth(out), 30)
	require.True(t, strings.HasPrefix(out, "go test "), "got %q", out)
	require.True(t, strings.HasSuffix(out, "…"), "got %q", out)
}

// TestTruncateToolParam_FileNameAloneTooLong covers the last resort: when
// even the file name does not fit, it is cut on the right rather than
// leaving a bare ellipsis.
func TestTruncateToolParam_FileNameAloneTooLong(t *testing.T) {
	t.Parallel()

	out := truncateToolParam("internal/ui/chat/a-very-long-file-name-indeed.go", 12)
	require.LessOrEqual(t, ansi.StringWidth(out), 12)
	require.True(t, strings.HasPrefix(out, "a-very"), "got %q", out)
}

// TestTruncateToolParam_FitsUnchanged proves a path that already fits is
// returned untouched — no gratuitous ellipsis.
func TestTruncateToolParam_FitsUnchanged(t *testing.T) {
	t.Parallel()

	path := "internal/ui/chat/tools_render.go"
	require.Equal(t, path, truncateToolParam(path, 80))
}

// TestToolSpinStopsOnResult pins the pair of predicates that decide whether a
// tool call is still in progress: a recorded result ends the spin even when the
// call was never marked Finished. The agent flips Finished on a cleanup path a
// hard kill can skip, so such a call outlives its turn in the database, and
// without the result check it spun for the rest of the session — while
// computeStatus already reported it as a success.
func TestToolSpinStopsOnResult(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	unfinished := message.ToolCall{ID: "tc1", Name: tools.ReadToolName, Input: "{}", Finished: false}
	result := &message.ToolResult{ToolCallID: "tc1", Name: tools.ReadToolName, Content: "file contents"}

	newItem := func(res *message.ToolResult) *baseToolMessageItem {
		return newBaseToolMessageItem(&sty, unfinished, res, &GenericToolRenderContext{}, false)
	}

	t.Run("unfinished without result spins", func(t *testing.T) {
		t.Parallel()

		require.True(t, newItem(nil).isSpinning())
		require.True(t, (&ToolRenderOpts{ToolCall: unfinished}).IsPending())
	})

	t.Run("unfinished with result does not spin", func(t *testing.T) {
		t.Parallel()

		item := newItem(result)
		require.False(t, item.isSpinning(), "a recorded result means the call is not in progress")
		require.Equal(t, ToolStatusSuccess, item.computeStatus(),
			"guards against the state that made this a bug: spinning while reported successful")
		require.True(t, item.Finished(), "a resulted call must be freezable, or its animation never stops")
		// IsPending drives the renderer's spinner block and has to agree,
		// otherwise the item draws a spinner that gets no further ticks.
		require.False(t, (&ToolRenderOpts{ToolCall: unfinished, Result: result}).IsPending())
	})

	t.Run("late result stops an already spinning item", func(t *testing.T) {
		t.Parallel()

		item := newItem(nil)
		require.True(t, item.isSpinning())
		item.SetResult(result)
		require.False(t, item.isSpinning())
		require.Nil(t, item.StartAnimation(), "a stopped item must not restart when scrolled back into view")
	})

	t.Run("canceled call does not spin", func(t *testing.T) {
		t.Parallel()

		item := newBaseToolMessageItem(&sty, unfinished, nil, &GenericToolRenderContext{}, true)
		require.False(t, item.isSpinning())
	})
}
