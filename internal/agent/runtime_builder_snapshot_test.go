package agent

import (
	"context"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	agenttools "github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	providerruntime "github.com/rave-soft/sennit/internal/providers/runtime"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/stretchr/testify/require"
)

func snapshotStubTool(name string) fantasy.AgentTool {
	return fantasy.NewAgentTool(name, name, func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.ToolResponse{}, nil
	})
}

func TestRuntimeForUsesCapturedPublishedConfigGeneration(t *testing.T) {
	server, captured := newCaptureServer(t)
	t.Setenv("SNAPSHOT_KEY", "process-key")
	t.Setenv("SNAPSHOT_URL", server.URL)

	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("snapshot", config.ProviderConfig{
		ID:      "snapshot",
		Type:    catwalk.Type("openai"),
		APIKey:  "$SNAPSHOT_KEY",
		BaseURL: "$SNAPSHOT_URL",
		Models:  []catwalk.Model{{ID: "snapshot-model", ContextWindow: 1024, DefaultMaxTokens: 256}},
	})
	cfg := &config.Config{
		Model:     config.SelectedModel{Provider: "snapshot", Model: "snapshot-model"},
		Providers: providers,
		Options: &config.Options{
			Attribution:          &config.Attribution{},
			Debug:                true,
			DisableAutoSummarize: true,
			AutoSummarizeAt:      111,
		},
		Agents: map[string]config.Agent{config.AgentCoder: {AllowedTools: coreToolNames()}},
		Env: map[string]string{
			"SNAPSHOT_KEY": "old-overlay-key",
			"SNAPSHOT_URL": server.URL,
		},
	}
	configured, ok := providers.Get("snapshot")
	require.True(t, ok)
	effective, err := providerruntime.FromConfig(configured, cfg.RuntimeResolver())
	require.NoError(t, err)
	cfg.SetRuntimeProvider("snapshot", effective)
	store := config.NewStore(config.StoreOptions{
		Config:     cfg,
		WorkingDir: t.TempDir(),
	})
	env := testEnv(t)
	builder := &runtimeBuilder{cfg: store, runtime: newRuntimeCache()}

	runtime, err := builder.runtimeFor(context.Background(), runtimeToolInputs{
		permissions: env.permissions,
		history:     env.history,
		filetracker: *env.filetracker,
		background:  shell.NewBackgroundShellManager(),
		delegationToolsBuilt: map[string]fantasy.AgentTool{
			AgentToolName:                   snapshotStubTool(AgentToolName),
			agenttools.AgenticFetchToolName: snapshotStubTool(agenttools.AgenticFetchToolName),
			"ask_parent":                    snapshotStubTool("ask_parent"),
		},
	})
	require.NoError(t, err)

	store.OverridePreferredModel(config.SelectedModel{Provider: "new-generation", Model: "new-model"})

	_, _ = runtime.model.Model.Generate(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hi")}})
	require.Equal(t, "Bearer old-overlay-key", captured.Header.Get("Authorization"))
	require.True(t, runtime.disableAutoSummarize)
	require.Equal(t, int64(111), runtime.autoSummarizeAt)
}
