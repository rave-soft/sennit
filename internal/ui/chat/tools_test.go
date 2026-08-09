package chat

import (
	"testing"

	"github.com/rave-soft/braid/internal/agent"
	"github.com/rave-soft/braid/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

// expectedToolMessageItemNames is the set of built-in tool names that must
// have a dedicated renderer registered in toolMessageItemFactories. Adding a
// new built-in tool with a dedicated renderer should add its name here too
// — this is a manual reminder, not automatic coverage of every agent tool;
// MCP tools and tools that intentionally fall back to the generic renderer
// are excluded.
var expectedToolMessageItemNames = []string{
	tools.BashToolName,
	tools.JobOutputToolName,
	tools.JobKillToolName,
	tools.ViewToolName,
	tools.WriteToolName,
	tools.EditToolName,
	tools.MultiEditToolName,
	tools.GlobToolName,
	tools.GrepToolName,
	tools.LSToolName,
	tools.DownloadToolName,
	tools.FetchToolName,
	tools.DiagnosticsToolName,
	agent.AgentToolName,
	tools.AgenticFetchToolName,
	tools.WebFetchToolName,
	tools.WebSearchToolName,
	tools.TodosToolName,
	tools.QuestionToolName,
	tools.ReferencesToolName,
	tools.DefinitionToolName,
	tools.RenameToolName,
	tools.ReplaceSymbolToolName,
	tools.CallHierarchyToolName,
	tools.SymbolsToolName,
	tools.LSPRestartToolName,
}

// TestToolMessageItemFactories_MatchExpectedNames asserts that
// toolMessageItemFactories has exactly the expected set of registered tool
// names — no more, no less. This guards against a factory being dropped
// silently (falling back to the generic renderer) and against dead entries
// accumulating for tools that no longer exist.
func TestToolMessageItemFactories_MatchExpectedNames(t *testing.T) {
	t.Parallel()

	expected := make(map[string]bool, len(expectedToolMessageItemNames))
	for _, name := range expectedToolMessageItemNames {
		expected[name] = true
	}

	got := make(map[string]bool, len(toolMessageItemFactories))
	for name := range toolMessageItemFactories {
		got[name] = true
	}

	for name := range expected {
		require.Truef(t, got[name], "expected tool name %q to have a registered factory", name)
	}
	for name := range got {
		require.Truef(t, expected[name], "unexpected tool name %q has a registered factory but is not in expectedToolMessageItemNames", name)
	}
	require.Len(t, toolMessageItemFactories, len(expectedToolMessageItemNames),
		"toolMessageItemFactories must have exactly the expected number of entries")
}
