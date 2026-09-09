package agent

import (
	"encoding/json"
	"testing"

	"github.com/rave-soft/sennit/internal/agent/prompt"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/stretchr/testify/require"
)

func TestDelegationRuntimeOptionsOverrideChildDefaults(t *testing.T) {
	coordinator := authTestCoordinator(t)
	definition := config.Agent{ID: "reviewer", Name: "Reviewer", Prompt: "Review", AllowedTools: []string{"read"}}
	args := tools.TaskCreateArgs{AgentID: definition.ID, ParentSessionID: "parent", SessionID: "child"}
	coordinator.cfg.Config().Options.DisableAutoSummarize = true
	coordinator.cfg.Config().Options.AutoSummarizeAt = 2048
	require.NoError(t, coordinator.delegation.snapshotDelegation(&args, &definition, t.Context()))
	var spec DelegationExecution
	require.NoError(t, json.Unmarshal([]byte(args.Execution), &spec))
	coordinator.cfg.Config().Options.DisableAutoSummarize = false
	coordinator.cfg.Config().Options.AutoSummarizeAt = 8192
	parsed, err := prompt.NewPrompt("reviewer", "Review", prompt.WithWorkingDir(coordinator.cfg.WorkingDir()), prompt.ForSubagent())
	require.NoError(t, err)
	built, err := coordinator.delegation.buildAgentWithOptions(t.Context(), parsed, definition, true, &spec.Options, spec.Model)
	require.NoError(t, err)
	runner := built.(*sessionAgent)
	require.True(t, runner.disableAutoSummarize)
	require.EqualValues(t, 2048, runner.autoSummarizeAt)
}

func TestDelegationSnapshotFreezesSelectedDefinition(t *testing.T) {
	t.Parallel()
	temperature := 0.4
	cfg := &config.Config{Model: config.SelectedModel{Provider: "mock", Model: "selected", ReasoningEffort: "high", Think: true, MaxTokens: 4096, Temperature: &temperature, ProviderOptions: map[string]any{"mode": "selected"}}}
	d := &delegationFinalizer{agentDeps: &agentDeps{cfg: configtest.NewStore(t, cfg)}}
	definition := config.Agent{ID: "reviewer", Prompt: "Selected instructions", AllowedTools: []string{"read"}}
	args := tools.TaskCreateArgs{AgentID: "reviewer", ParentSessionID: "parent", SessionID: "message$$call", Goal: "inspect", Depth: 2}
	require.NoError(t, d.snapshotDelegation(&args, &definition))
	definition.Prompt = "Changed instructions"
	definition.AllowedTools[0] = "write"
	var captured DelegationExecution
	require.NoError(t, json.Unmarshal([]byte(args.Execution), &captured))
	require.Equal(t, "Selected instructions", captured.Definition.Prompt)
	require.Equal(t, []string{"read"}, captured.Definition.AllowedTools)
	require.Equal(t, "mock/selected", captured.Definition.Model)
	require.Equal(t, "high", captured.Definition.ReasoningEffort)
	require.True(t, captured.Model.Think)
	require.EqualValues(t, 4096, captured.Model.MaxTokens)
	require.Equal(t, 0.4, *captured.Model.Temperature)
	require.Equal(t, "selected", captured.Model.ProviderOptions["mode"])
	require.NotNil(t, args.Factory)
	require.Equal(t, "message$$call", captured.SessionID)
	require.Equal(t, 2, captured.Depth)
}
