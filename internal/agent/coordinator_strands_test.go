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

// noopStrandManager is a minimal tools.StrandManager with no behavior:
// these tests only check whether the strand_* tools are offered to an
// agent, not what they do (see internal/agent/tools/strand_tools_test.go
// and internal/strand for that).
type noopStrandManager struct{}

func (noopStrandManager) Create(context.Context, tools.StrandCreateArgs) (tools.StrandInfo, error) {
	return tools.StrandInfo{}, nil
}

func (noopStrandManager) List(context.Context) ([]tools.StrandInfo, error) { return nil, nil }

func (noopStrandManager) Get(context.Context, string) (tools.StrandInfo, error) {
	return tools.StrandInfo{}, nil
}

func (noopStrandManager) Send(context.Context, string, string) error { return nil }
func (noopStrandManager) Wait(context.Context, []string, time.Duration) error {
	return nil
}
func (noopStrandManager) Merge(context.Context, string) error { return nil }
func (noopStrandManager) Remove(context.Context, string, bool, bool) error {
	return nil
}

var strandToolNames = []string{
	tools.StrandCreateToolName,
	tools.StrandListToolName,
	tools.StrandStatusToolName,
	tools.StrandSendToolName,
	tools.StrandWaitToolName,
	tools.StrandMergeToolName,
	tools.StrandRemoveToolName,
}

// newStrandsTestCoordinator builds a coordinator with the minimal
// dependencies buildTools touches, wired against the given strand
// manager (nil-able).
func newStrandsTestCoordinator(t *testing.T, strands tools.StrandManager) (*coordinator, config.Agent) {
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
		strands:     strands,
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

func TestBuildTools_StrandToolsPresentForMainAgentWithManager(t *testing.T) {
	coord, agentCfg := newStrandsTestCoordinator(t, noopStrandManager{})

	built, err := coord.buildTools(t.Context(), agentCfg, false)
	require.NoError(t, err)

	names := toolNames(t, built)
	for _, want := range strandToolNames {
		require.Contains(t, names, want)
	}
}

func TestBuildTools_StrandToolsAbsentWhenManagerNil(t *testing.T) {
	coord, agentCfg := newStrandsTestCoordinator(t, nil)

	built, err := coord.buildTools(t.Context(), agentCfg, false)
	require.NoError(t, err)

	names := toolNames(t, built)
	for _, absent := range strandToolNames {
		require.NotContains(t, names, absent)
	}
}

func TestBuildTools_StrandToolsAbsentForSubAgent(t *testing.T) {
	coord, agentCfg := newStrandsTestCoordinator(t, noopStrandManager{})

	// isSubAgent=true mirrors how the coordinator builds the "agent"
	// delegation tool's target and other sub-agents: strand tools must
	// never be handed to them even when the workspace owns a manager.
	built, err := coord.buildTools(t.Context(), agentCfg, true)
	require.NoError(t, err)

	names := toolNames(t, built)
	for _, absent := range strandToolNames {
		require.NotContains(t, names, absent)
	}
}

func TestCoordinator_SetStrandsTakesEffectOnNextBuild(t *testing.T) {
	coord, agentCfg := newStrandsTestCoordinator(t, nil)

	built, err := coord.buildTools(t.Context(), agentCfg, false)
	require.NoError(t, err)
	require.NotContains(t, toolNames(t, built), tools.StrandCreateToolName)

	coord.SetStrands(noopStrandManager{})

	built, err = coord.buildTools(t.Context(), agentCfg, false)
	require.NoError(t, err)
	require.Contains(t, toolNames(t, built), tools.StrandCreateToolName)
}
