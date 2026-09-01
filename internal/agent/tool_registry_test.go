package agent

import (
	"slices"
	"testing"

	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/configruntime"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/stretchr/testify/require"
)

// pinTestAllowedTools is a superset covering every tool name buildTools
// can register (coder or sub-agent). Passing it to both builds in the
// tests below means the *difference* in what actually comes out is driven
// only by buildTools' own gates — not by AllowedTools trimming the list
// for us — which is exactly the distinction these tests exist to pin.
var pinTestAllowedTools = []string{
	AgentToolName, tools.AgenticFetchToolName,
	"bash", tools.SennitInfoToolName, tools.SennitLogsToolName, tools.JobOutputToolName, tools.JobKillToolName,
	tools.DownloadToolName, tools.EditToolName, tools.MultiEditToolName, tools.FetchToolName, tools.WebFetchToolName,
	tools.WebSearchToolName, tools.GlobToolName, tools.GrepToolName, tools.RipgrepToolName, tools.LSToolName,
	tools.TodosToolName, tools.ReadToolName, tools.WriteToolName,
	tools.ThreadCreateToolName, tools.AgentListToolName, tools.AgentResultToolName, tools.AgentSendToolName,
	tools.ThreadMergeToolName, tools.ThreadRemoveToolName,
	tools.AgentListToolName, tools.AgentResultToolName, tools.AgentCancelToolName, tools.AgentSendToolName, tools.AgentOutputToolName,
	tools.AskParentToolName, tools.QuestionToolName,
	tools.DiagnosticsToolName, tools.ReferencesToolName, tools.LSPRestartToolName, tools.SymbolsToolName,
	tools.DefinitionToolName, tools.CallHierarchyToolName, tools.RenameToolName, tools.ReplaceSymbolToolName,
	tools.ListMCPResourcesToolName, tools.ReadMCPResourceToolName,
}

// pinTestCoordinator builds a coordinator with the minimal hermetic
// config buildTools needs (see newAgentToolTestCoordinator), with no
// thread manager, no task manager, and no MCP servers wired — so the
// thread_*, task_*, and MCP-server-dependent tools never gate open here,
// and the resulting set is driven only by isSubAgent/interactive/
// AllowedTools, which is what these tests pin.
func pinTestCoordinator(t *testing.T, interactive bool) *coordinator {
	t.Helper()
	env := testEnv(t)
	sennitJSON := `{
  "options": {"disable_default_providers": true},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192, "default_max_tokens": 128}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}
}`
	writeGlobalConfig(t, sennitJSON)

	cfg, err := configruntime.Load(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.SetupAgents()

	coord := &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		history:     env.history,
		filetracker: *env.filetracker,
		mcp:         mcp.NewRegistry(),
		background:  shell.NewBackgroundShellManager(),
		interactive: interactive,
	}
	coord.newCoordinatorComponents()
	return coord
}

// toolNames is defined in coordinator_threads_test.go.

// searchToolName is whichever of grep/ripgrep NewSearchTool registers on
// this machine (see tools.HasRipgrep) — buildTools treats the two as one
// interchangeable slot, and the expected set below has to match whichever
// one actually built.
func searchToolName() string {
	if tools.HasRipgrep() {
		return tools.RipgrepToolName
	}
	return tools.GrepToolName
}

// TestBuildToolsPinnedSet_Coder pins the exact tool set a top-level
// (coder) build produces, given every tool name is allowed. This is the
// regression net for buildTools' registry table: dropping a row, or
// loosening a gate, changes what the model is allowed to do — see
// tool_registry.go's toolSpec doc comment.
func TestBuildToolsPinnedSet_Coder(t *testing.T) {
	coord := pinTestCoordinator(t, false)
	agent := config.Agent{Name: "coder", AllowedTools: pinTestAllowedTools}

	built, err := coord.delegation.buildTools(t.Context(), agent, false)
	require.NoError(t, err)

	expected := []string{
		AgentToolName, tools.AgenticFetchToolName,
		"bash", tools.SennitInfoToolName, tools.SennitLogsToolName, tools.JobOutputToolName, tools.JobKillToolName,
		tools.DownloadToolName, tools.EditToolName, tools.MultiEditToolName, tools.FetchToolName, tools.WebFetchToolName,
		tools.WebSearchToolName, tools.GlobToolName, searchToolName(), tools.LSToolName,
		tools.TodosToolName, tools.ReadToolName, tools.WriteToolName,
		tools.AskParentToolName,
		tools.DiagnosticsToolName, tools.ReferencesToolName, tools.LSPRestartToolName, tools.SymbolsToolName,
		tools.DefinitionToolName, tools.CallHierarchyToolName, tools.RenameToolName, tools.ReplaceSymbolToolName,
		// Absent despite being allowed: no thread manager, no task
		// manager, and not interactive — question, thread_*, and task_*
		// all gate on those, not on AllowedTools.
	}
	slices.Sort(expected)
	require.Equal(t, expected, toolNames(t, built))
}

// TestBuildToolsPinnedSet_SubAgent pins the sub-agent build against the
// exact same AllowedTools superset as the coder test above. The only
// thing that can explain a difference between the two sets is
// isSubAgent: ask_parent, question, thread_*, and task_* are gated on
// !isSubAgent regardless of what AllowedTools says, which is the
// coder/sub-agent split this task calls out as security-relevant.
func TestBuildToolsPinnedSet_SubAgent(t *testing.T) {
	coord := pinTestCoordinator(t, false)
	agent := config.Agent{Name: "task", AllowedTools: pinTestAllowedTools}

	built, err := coord.delegation.buildTools(t.Context(), agent, true)
	require.NoError(t, err)

	expected := []string{
		AgentToolName, tools.AgenticFetchToolName,
		"bash", tools.SennitInfoToolName, tools.SennitLogsToolName, tools.JobOutputToolName, tools.JobKillToolName,
		tools.DownloadToolName, tools.EditToolName, tools.MultiEditToolName, tools.FetchToolName, tools.WebFetchToolName,
		tools.WebSearchToolName, tools.GlobToolName, searchToolName(), tools.LSToolName,
		tools.TodosToolName, tools.ReadToolName, tools.WriteToolName,
		tools.DiagnosticsToolName, tools.ReferencesToolName, tools.LSPRestartToolName, tools.SymbolsToolName,
		tools.DefinitionToolName, tools.CallHierarchyToolName, tools.RenameToolName, tools.ReplaceSymbolToolName,
		// No ask_parent, no question, no thread_*, no task_*: gated on
		// !isSubAgent (see toolSpecs), unlike everything else here, which
		// is gated on AllowedTools or LSP config only.
	}
	slices.Sort(expected)
	require.Equal(t, expected, toolNames(t, built))
}
