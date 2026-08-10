package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/braid/internal/agent/tools"
	"github.com/rave-soft/braid/internal/agent/tools/mcp"
	"github.com/rave-soft/braid/internal/config"
	"github.com/stretchr/testify/require"
)

// noopThreadManager is a minimal tools.ThreadManager with no behavior:
// these tests only check whether the thread_* tools are offered to an
// agent, not what they do (see internal/agent/tools/thread_tools_test.go
// and internal/thread for that).
type noopThreadManager struct{}

func (noopThreadManager) Create(context.Context, tools.ThreadCreateArgs) (tools.ThreadInfo, error) {
	return tools.ThreadInfo{}, nil
}

func (noopThreadManager) List(context.Context) ([]tools.ThreadInfo, error) { return nil, nil }

func (noopThreadManager) Get(context.Context, string) (tools.ThreadInfo, error) {
	return tools.ThreadInfo{}, nil
}

func (noopThreadManager) Send(context.Context, string, string) error { return nil }
func (noopThreadManager) Wait(context.Context, []string, time.Duration) error {
	return nil
}
func (noopThreadManager) Merge(context.Context, string) error { return nil }
func (noopThreadManager) Remove(context.Context, string, bool, bool) error {
	return nil
}

var threadToolNames = []string{
	tools.ThreadCreateToolName,
	tools.ThreadListToolName,
	tools.ThreadStatusToolName,
	tools.ThreadSendToolName,
	tools.ThreadWaitToolName,
	tools.ThreadMergeToolName,
	tools.ThreadRemoveToolName,
}

// newThreadsTestCoordinator builds a coordinator with the minimal
// dependencies buildTools touches, wired against the given thread
// manager (nil-able).
func newThreadsTestCoordinator(t *testing.T, threads tools.ThreadManager) (*coordinator, config.Agent) {
	t.Helper()
	env := testEnv(t)

	// Minimal hermetic config: one openai-typed provider with selected
	// large and small models, so buildAgentModels (reached through the
	// "agent" delegation tool that buildTools always tries to build)
	// succeeds without any real network access.
	braidJSON := `{
  "options": {"disable_default_providers": true},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192, "default_max_tokens": 128}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}
}`
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "braid.json"), []byte(braidJSON), 0o644))

	cfg, err := config.Init(env.workingDir, "", false)
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
		threads:     threads,
	}
	return coord, cfg.Config().Agents[config.AgentCoder]
}

func toolNames(t *testing.T, agentTools []fantasy.AgentTool) []string {
	t.Helper()
	names := make([]string, len(agentTools))
	for i, tool := range agentTools {
		names[i] = tool.Info().Name
	}
	return names
}

func TestBuildTools_ThreadToolsPresentForMainAgentWithManager(t *testing.T) {
	coord, agentCfg := newThreadsTestCoordinator(t, noopThreadManager{})

	built, err := coord.buildTools(t.Context(), agentCfg, false)
	require.NoError(t, err)

	names := toolNames(t, built)
	for _, want := range threadToolNames {
		require.Contains(t, names, want)
	}
}

func TestBuildTools_ThreadToolsAbsentWhenManagerNil(t *testing.T) {
	coord, agentCfg := newThreadsTestCoordinator(t, nil)

	built, err := coord.buildTools(t.Context(), agentCfg, false)
	require.NoError(t, err)

	names := toolNames(t, built)
	for _, absent := range threadToolNames {
		require.NotContains(t, names, absent)
	}
}

func TestBuildTools_ThreadToolsAbsentForSubAgent(t *testing.T) {
	coord, agentCfg := newThreadsTestCoordinator(t, noopThreadManager{})

	// isSubAgent=true mirrors how the coordinator builds the "agent"
	// delegation tool's target and other sub-agents: thread tools must
	// never be handed to them even when the workspace owns a manager.
	built, err := coord.buildTools(t.Context(), agentCfg, true)
	require.NoError(t, err)

	names := toolNames(t, built)
	for _, absent := range threadToolNames {
		require.NotContains(t, names, absent)
	}
}

func TestCoordinator_SetThreadsTakesEffectOnNextBuild(t *testing.T) {
	coord, agentCfg := newThreadsTestCoordinator(t, nil)

	built, err := coord.buildTools(t.Context(), agentCfg, false)
	require.NoError(t, err)
	require.NotContains(t, toolNames(t, built), tools.ThreadCreateToolName)

	coord.SetThreads(noopThreadManager{})

	built, err = coord.buildTools(t.Context(), agentCfg, false)
	require.NoError(t, err)
	require.Contains(t, toolNames(t, built), tools.ThreadCreateToolName)
}
