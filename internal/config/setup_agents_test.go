package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfig_setupAgentsWithNoDisabledTools(t *testing.T) {
	cfg := &Config{
		Options: &Options{
			DisabledTools: []string{},
		},
	}

	cfg.SetupAgents()
	coderAgent, ok := cfg.Agents[AgentCoder]
	require.True(t, ok)
	assert.Equal(t, allToolNames(), coderAgent.AllowedTools)

	taskAgent, ok := cfg.Agents[AgentTask]
	require.True(t, ok)
	assert.Equal(t, []string{"git_status", "git_diff", "git_log", "agent_trace", "lsp_symbols", "lsp_workspace_symbols", "lsp_hover", "lsp_definition", "lsp_call_hierarchy", "fetch", "web_fetch", "web_search", "glob", "grep", "ripgrep", "ls", "read", "multi_read"}, taskAgent.AllowedTools)
}

func TestConfig_setupAgentsWithDisabledTools(t *testing.T) {
	cfg := &Config{
		Options: &Options{
			DisabledTools: []string{
				"edit",
				"download",
				"grep",
				"ripgrep",
			},
		},
	}

	cfg.SetupAgents()
	coderAgent, ok := cfg.Agents[AgentCoder]
	require.True(t, ok)

	assert.Equal(t, []string{"agent", "bash", "git_status", "git_diff", "git_log", "sennit_info", "sennit_logs", "agent_trace", "job_output", "job_kill", "multiedit", "lsp_diagnostics", "lsp_references", "lsp_restart", "lsp_symbols", "lsp_workspace_symbols", "lsp_hover", "lsp_definition", "lsp_call_hierarchy", "lsp_rename", "lsp_replace_symbol", "fetch", "agentic_fetch", "web_fetch", "web_search", "glob", "ls", "question", "todos", "read", "multi_read", "write", "list_mcp_resources", "read_mcp_resource", "thread_create", "thread_list", "thread_status", "thread_wait", "thread_merge", "thread_remove", "task_list", "task_result", "task_cancel", "task_send", "task_output", "ask_parent"}, coderAgent.AllowedTools)

	taskAgent, ok := cfg.Agents[AgentTask]
	require.True(t, ok)
	assert.Equal(t, []string{"git_status", "git_diff", "git_log", "agent_trace", "lsp_symbols", "lsp_workspace_symbols", "lsp_hover", "lsp_definition", "lsp_call_hierarchy", "fetch", "web_fetch", "web_search", "glob", "ls", "read", "multi_read"}, taskAgent.AllowedTools)
}

func TestConfig_setupAgentsWithWebSearchDisabled(t *testing.T) {
	cfg := &Config{
		Options: &Options{
			DisabledTools: []string{
				"web_search",
			},
		},
	}

	cfg.SetupAgents()
	coderAgent, ok := cfg.Agents[AgentCoder]
	require.True(t, ok)

	assert.Contains(t, coderAgent.AllowedTools, "web_fetch")
	assert.NotContains(t, coderAgent.AllowedTools, "web_search")
}

func TestConfig_setupAgentsWithEveryReadOnlyToolDisabled(t *testing.T) {
	cfg := &Config{
		Options: &Options{
			DisabledTools: []string{
				"agent_trace",
				"git_status",
				"git_diff",
				"git_log",
				"glob",
				"grep",
				"ripgrep",
				"ls",
				"lsp_call_hierarchy",
				"lsp_definition",
				"lsp_symbols", "lsp_workspace_symbols", "lsp_hover",
				"read",
				"multi_read",
				"fetch",
				"web_fetch",
				"web_search",
			},
		},
	}

	cfg.SetupAgents()
	coderAgent, ok := cfg.Agents[AgentCoder]
	require.True(t, ok)
	assert.Equal(t, []string{"agent", "bash", "sennit_info", "sennit_logs", "job_output", "job_kill", "download", "edit", "multiedit", "lsp_diagnostics", "lsp_references", "lsp_restart", "lsp_rename", "lsp_replace_symbol", "agentic_fetch", "question", "todos", "write", "list_mcp_resources", "read_mcp_resource", "thread_create", "thread_list", "thread_status", "thread_wait", "thread_merge", "thread_remove", "task_list", "task_result", "task_cancel", "task_send", "task_output", "ask_parent"}, coderAgent.AllowedTools)

	taskAgent, ok := cfg.Agents[AgentTask]
	require.True(t, ok)
	assert.Len(t, taskAgent.AllowedTools, 0)
}
