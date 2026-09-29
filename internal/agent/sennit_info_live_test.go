package agent

import (
	"context"
	"testing"

	"charm.land/fantasy"
	agenttools "github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/configruntime"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/stretchr/testify/require"
)

// TestSennitInfoSeesConfigPublishedAfterBuild pins sennit_info to the live
// store rather than the runtime's build-time snapshot. A run keeps the tools
// it was compiled with, and a message sent mid-run joins that run, so a
// snapshot-bound sennit_info reported a model list refreshed a minute
// earlier as missing: the agent was asked to switch subagents to a model
// that had just appeared and answered that no such model existed.
func TestSennitInfoSeesConfigPublishedAfterBuild(t *testing.T) {
	writeGlobalConfig(t, `{
  "options": {"disable_default_providers": true},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192, "default_max_tokens": 128}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}
}`)
	env := testEnv(t)

	store, err := configruntime.Load(env.workingDir, "", false)
	require.NoError(t, err)
	store.SetupAgents()

	builder := &runtimeBuilder{agentDeps: &agentDeps{cfg: store}, runtime: newRuntimeCache()}
	runtime, err := builder.runtimeFor(context.Background(), runtimeToolInputs{
		permissions: env.permissions,
		fileHistory: newFileHistory(env.history),
		filetracker: newFileTracking(*env.filetracker),
		background:  shell.NewBackgroundShellManager(),
		delegationToolsBuilt: map[string]fantasy.AgentTool{
			AgentToolName:                   snapshotStubTool(AgentToolName),
			agenttools.AgenticFetchToolName: snapshotStubTool(agenttools.AgenticFetchToolName),
			"ask_parent":                    snapshotStubTool("ask_parent"),
		},
	})
	require.NoError(t, err)

	var info fantasy.AgentTool
	for _, tool := range runtime.tools {
		if tool.Info().Name == agenttools.SennitInfoToolName {
			info = tool
		}
	}
	require.NotNil(t, info, "coder runtime has no sennit_info tool")

	before := store.Version()
	require.NoError(t, store.SetConfigField(config.ScopeGlobal, "providers.mock.models", []map[string]any{
		{"id": "mock-model", "name": "Mock", "context_window": 8192, "default_max_tokens": 128},
		{"id": "mock-model-new", "name": "Mock New", "context_window": 8192, "default_max_tokens": 128},
	}))
	require.Greater(t, store.Version(), before, "the write did not publish a new config generation")

	resp, err := info.Run(context.Background(), fantasy.ToolCall{
		ID:    "call-1",
		Name:  agenttools.SennitInfoToolName,
		Input: `{"models_for":"mock"}`,
	})
	require.NoError(t, err)
	require.Contains(t, resp.Content, "mock-model-new")
}
