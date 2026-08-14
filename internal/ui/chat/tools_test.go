package chat

import (
	"testing"

	"github.com/rave-soft/braid/internal/agent"
	"github.com/rave-soft/braid/internal/agent/tools"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// toolsHandledOutsideFactoryMap lists built-in tools with a dedicated
// renderer that NewToolMessageItem special-cases before consulting
// toolMessageItemFactories, instead of registering a map entry — because
// their constructor needs an argument (cfg, to resolve a delegation's
// display name and model/effort override) that toolMessageItemFactory's
// signature has no room for. See the map's comment on agent.AgentToolName.
var toolsHandledOutsideFactoryMap = []string{
	agent.AgentToolName,
}

// toolsWithoutDedicatedRenderer lists the built-in tools (from
// config.AllToolNames, the actual source of truth for what tools exist)
// that intentionally have no entry in toolMessageItemFactories and fall
// back to the generic renderer. Anything not on this list (and not in
// toolsHandledOutsideFactoryMap) must have a dedicated factory.
var toolsWithoutDedicatedRenderer = []string{
	tools.BraidInfoToolName,
	tools.BraidLogsToolName,
	tools.ListMCPResourcesToolName,
	tools.ReadMCPResourceToolName,
	// Thread tools (internal/agent/tools/thread_*.go) don't have a
	// dedicated renderer yet; they fall back to the generic one until the
	// TUI grows one.
	tools.ThreadCreateToolName,
	tools.ThreadListToolName,
	tools.ThreadStatusToolName,
	tools.ThreadSendToolName,
	tools.ThreadWaitToolName,
	tools.ThreadMergeToolName,
	tools.ThreadRemoveToolName,
	// Task tools (internal/agent/tools/task_*.go), same story: no
	// dedicated renderer yet.
	tools.TaskListToolName,
	tools.TaskResultToolName,
	tools.TaskCancelToolName,
	tools.TaskSendToolName,
	tools.TaskOutputToolName,
}

// TestToolMessageItemFactories_MatchExpectedNames checks
// toolMessageItemFactories against config.AllToolNames instead of a
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
			require.NotContainsf(t, toolMessageItemFactories, name,
				"tool %q is listed in toolsHandledOutsideFactoryMap but also has a map entry; the map entry is unreachable dead code, remove one or the other", name)
			continue
		}
		if noRenderer[name] {
			require.NotContainsf(t, toolMessageItemFactories, name,
				"tool %q is listed as having no dedicated renderer, but one is registered; remove it from toolsWithoutDedicatedRenderer", name)
			continue
		}
		require.Containsf(t, toolMessageItemFactories, name,
			"tool %q has no registered factory and will fall back to the generic renderer", name)
	}

	// toolsHandledOutsideFactoryMap's whole premise is that
	// NewToolMessageItem special-cases these tools before the map lookup —
	// verify that dispatch actually happens, not just that the map omits
	// them (which noRenderer-listed tools also do, for the opposite
	// reason).
	sty := styles.CharmtonePantera()
	item := NewToolMessageItem(&sty, "msg", message.ToolCall{ID: "tc-agent", Name: agent.AgentToolName, Input: "{}"}, nil, false, nil)
	require.IsType(t, &AgentToolMessageItem{}, item,
		"the built-in agent tool must dispatch to AgentToolMessageItem, not fall back to the generic renderer")

	// Every registered factory must correspond to a real, known tool —
	// this catches dead renderer registrations for tools that no longer
	// exist, the same kind of debris a previously removed built-in tool
	// left behind elsewhere in the repo.
	known := make(map[string]bool, len(config.AllToolNames()))
	for _, name := range config.AllToolNames() {
		known[name] = true
	}
	for name := range toolMessageItemFactories {
		require.Truef(t, known[name], "tool %q has a registered factory but is not in config.AllToolNames()", name)
	}
}

// TestNewToolMessageItem_CustomAgentDispatch covers the documented gap
// closed here: a user-defined agent tool (internal/agent/custom_agent_tool.go
// registers one per entry in cfg.Agents, named after the agent's id) must
// get the same AgentToolMessageItem renderer as the built-in "agent" tool —
// status line, collapse-on-finish, click-to-drill — not the generic
// fallback. "coder" and "task" are config.Agents entries too, but they're
// roles, not tools a model can call, so they must never dispatch here.
func TestNewToolMessageItem_CustomAgentDispatch(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
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

	sty := styles.CharmtonePantera()
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

	sty := styles.CharmtonePantera()
	header := "header"

	for _, junk := range []string{"", "None", "none", "  NULL  ", "n/a", "N/A", "nil", "-", "undefined"} {
		out := appendResultSummary(&sty, header, junk)
		require.Equal(t, header, out, "junk summary %q must be omitted entirely", junk)
	}

	out := appendResultSummary(&sty, header, "2.1s")
	require.NotEqual(t, header, out, "a real summary must still be appended")
	require.Contains(t, out, "2.1s")
}
