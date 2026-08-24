package agent

import (
	"slices"
	"testing"

	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/rave-soft/sennit/internal/toolmeta"
	"github.com/stretchr/testify/require"
)

var expectedGateByName = map[toolmeta.Gate][]string{
	toolmeta.GateAlways: {
		"bash", "git_status", "git_diff", "git_log", "sennit_info", "sennit_logs", "job_output", "job_kill", "download", "edit", "multiedit",
		"fetch", "web_fetch", "web_search", "glob", "grep", "ripgrep", "ls", "todos", "read", "multi_read", "write",
	},
	toolmeta.GateAllowed:     {"agent", "agentic_fetch"},
	toolmeta.GateNotSubAgent: {"ask_parent"},
	toolmeta.GateThreads:     {"thread_create", "thread_list", "thread_status", "thread_send", "thread_wait", "thread_merge", "thread_remove"},
	toolmeta.GateTasks:       {"task_list", "task_result", "task_cancel", "task_send", "task_output"},
	toolmeta.GateLSP:         {"lsp_diagnostics", "lsp_references", "lsp_restart", "lsp_symbols", "lsp_definition", "lsp_call_hierarchy", "lsp_rename", "lsp_replace_symbol"},
	toolmeta.GateMCP:         {"list_mcp_resources", "read_mcp_resource"},
	toolmeta.GateInteractive: {"question"},
}

func TestToolSpecsMatchFrozenGateMatrixAndBuildNames(t *testing.T) {
	coord, _ := newThreadsTestCoordinator(t, noopThreadManager{})
	coord.tasks = noopTaskManager{}
	b := &buildToolsCtx{
		agent:              config.Agent{AllowedTools: toolmeta.NamesAll()},
		interactive:        true,
		cfg:                newAgentConfig(coord.cfg.Config()),
		threads:            noopThreadManager{},
		taskManager:        noopTaskManager{},
		backgroundAgentsOn: true,
	}
	seen := make(map[string]bool)
	for _, spec := range toolSpecs() {
		require.NotEmpty(t, spec.Names)
		for _, name := range spec.Names {
			descriptor, exists := toolmeta.Lookup(name)
			require.Truef(t, exists, "spec name %q lacks metadata", name)
			require.Contains(t, expectedGateByName[descriptor.Gate], name, "gate changed for %q", name)
			require.Falsef(t, seen[name], "tool %q is built by more than one spec", name)
			seen[name] = true
		}
		built, err := spec.Build(t.Context(), coord, b)
		require.NoError(t, err)
		actual := make([]string, len(built))
		for i, tool := range built {
			actual[i] = tool.Info().Name
		}
		require.Equal(t, spec.Names, actual, "spec builder output names must exactly match its declaration")
	}
	for _, name := range toolmeta.NamesAll() {
		if name == tools.GrepToolName || name == tools.RipgrepToolName {
			require.Equal(t, name == searchToolName(), seen[name], "exactly the available search implementation belongs to toolSpecs")
			continue
		}
		require.Truef(t, seen[name], "metadata tool %q has no static builder spec", name)
	}
}

func TestBuildToolsMatchesFrozenGateScenarios(t *testing.T) {
	without := func(names []string, excludedNames ...string) []string {
		excluded := make(map[string]bool, len(excludedNames)+2)
		for _, name := range excludedNames {
			excluded[name] = true
		}
		var expected []string
		for _, name := range names {
			if !excluded[name] && name != tools.GrepToolName && name != tools.RipgrepToolName {
				expected = append(expected, name)
			}
		}
		if !excluded[searchToolName()] {
			expected = append(expected, searchToolName())
		}
		slices.Sort(expected)
		return expected
	}
	withoutGates := func(names []string, gates ...toolmeta.Gate) []string {
		var excluded []string
		for _, gate := range gates {
			excluded = append(excluded, expectedGateByName[gate]...)
		}
		return without(names, excluded...)
	}
	allNames := toolmeta.NamesAll()
	falseValue := false

	tests := []struct {
		name         string
		allowedTools []string
		isSubAgent   bool
		configure    func(*config.Config)
		expected     []string
	}{
		{
			name:         "all gates enabled",
			allowedTools: allNames,
			expected:     without(allNames),
		},
		{
			name:         "sub-agent gates disabled",
			allowedTools: allNames,
			isSubAgent:   true,
			expected:     withoutGates(allNames, toolmeta.GateNotSubAgent, toolmeta.GateThreads, toolmeta.GateTasks, toolmeta.GateInteractive),
		},
		{
			name:         "agent denied while agentic fetch allowed",
			allowedTools: without(allNames, AgentToolName),
			expected:     without(allNames, AgentToolName),
		},
		{
			name:         "agentic fetch denied while agent allowed",
			allowedTools: without(allNames, tools.AgenticFetchToolName),
			expected:     without(allNames, tools.AgenticFetchToolName),
		},
		{
			name:         "no explicit LSP with auto LSP disabled",
			allowedTools: allNames,
			configure: func(cfg *config.Config) {
				cfg.LSP = nil
				cfg.Options.AutoLSP = &falseValue
			},
			expected: withoutGates(allNames, toolmeta.GateLSP),
		},
		{
			name:         "explicit LSP with auto LSP disabled",
			allowedTools: allNames,
			configure: func(cfg *config.Config) {
				cfg.LSP = config.LSPs{"test": {Command: "unused"}}
				cfg.Options.AutoLSP = &falseValue
			},
			expected: without(allNames),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			coord, _ := newThreadsTestCoordinator(t, noopThreadManager{})
			coord.tasks = noopTaskManager{}
			coord.interactive = true
			cfg := coord.cfg.Config()
			cfg.MCP["test"] = config.MCPConfig{Type: config.MCPStdio, Command: "unused"}
			if tt.configure != nil {
				tt.configure(cfg)
			}

			built, err := coord.buildTools(t.Context(), config.Agent{Name: "coder", AllowedTools: tt.allowedTools}, tt.isSubAgent)
			require.NoError(t, err)
			require.Equal(t, tt.expected, toolNames(t, built))
		})
	}
}

func TestCoordinatorBuiltToolMetadataMatchesInfo(t *testing.T) {
	coord, _ := newThreadsTestCoordinator(t, noopThreadManager{})
	coord.background = shell.NewBackgroundShellManager()
	built, err := coord.buildTools(t.Context(), config.Agent{Name: "coder", AllowedTools: toolmeta.NamesAll()}, false)
	require.NoError(t, err)
	seen := make(map[string]bool)
	for _, tool := range built {
		info := tool.Info()
		descriptor, ok := toolmeta.Lookup(info.Name)
		require.Truef(t, ok, "built static tool %q lacks metadata", info.Name)
		require.Equalf(t, descriptor.ParallelSafe, info.Parallel, "tool %q parallel flag", info.Name)
		require.NotEmptyf(t, info.InputSchema, "tool %q constructor must own a non-empty schema", info.Name)
		seen[info.Name] = true
	}
	for _, descriptor := range toolmeta.Builtins() {
		if descriptor.Builder == toolmeta.BuilderCoordinator {
			require.Truef(t, seen[descriptor.Name], "coordinator-built tool %q was not checked", descriptor.Name)
		}
	}
}
