package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/shell"
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

func (noopThreadManager) Merge(context.Context, string) (tools.ThreadInfo, error) {
	return tools.ThreadInfo{}, nil
}

func (noopThreadManager) Remove(context.Context, string, bool, bool) error {
	return nil
}

// threadToolNames lists the thread_* tools expected under the coder
// agent's *default* AllowedTools. tools.ThreadWaitToolName is
// deliberately excluded: it is no longer in the default set (a thread's
// completion now arrives on its own — see internal/config's
// allToolNames), though it is still constructed and offered to any
// agent config that explicitly allows it — see
// TestBuildTools_ThreadWaitAbsentByDefaultButAvailableWhenExplicitlyAllowed.
var threadToolNames = []string{
	tools.ThreadCreateToolName,
	tools.ThreadListToolName,
	tools.ThreadStatusToolName,
	tools.ThreadSendToolName,
	tools.ThreadMergeToolName,
	tools.ThreadRemoveToolName,
}

// newThreadsTestCoordinator builds a coordinator with the minimal
// dependencies buildTools touches, wired against the given thread
// manager (nil-able).
func newThreadsTestCoordinator(t *testing.T, threads tools.ThreadManager) (*coordinator, config.Agent) {
	t.Helper()
	env := testEnv(t)

	// Minimal hermetic config: one openai-typed provider with a selected
	// model, so buildAgentModel (reached through the
	// "agent" delegation tool that buildTools always tries to build)
	// succeeds without any real network access.
	sennitJSON := `{
  "options": {"disable_default_providers": true},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192, "default_max_tokens": 128}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}
}`
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "sennit.json"), []byte(sennitJSON), 0o644))

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
		background:  shell.NewBackgroundShellManager(),
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

// TestBuildTools_ThreadWaitAbsentByDefaultButAvailableWhenExplicitlyAllowed
// covers the removal side of the "allowed-tools list is where a tool
// silently stops existing" trap: dropping thread_wait from the default
// set (internal/config's allToolNames) must not make it uninstallable -
// buildTools still constructs it unconditionally and offers it to any
// agent config whose own AllowedTools names it explicitly, exactly like
// any other tool not in the default set.
func TestBuildTools_ThreadWaitAbsentByDefaultButAvailableWhenExplicitlyAllowed(t *testing.T) {
	coord, agentCfg := newThreadsTestCoordinator(t, noopThreadManager{})

	built, err := coord.buildTools(t.Context(), agentCfg, false)
	require.NoError(t, err)
	require.NotContains(t, toolNames(t, built), tools.ThreadWaitToolName,
		"thread_wait must not be offered under the default AllowedTools")

	allowed := agentCfg
	allowed.AllowedTools = append(slices.Clone(agentCfg.AllowedTools), tools.ThreadWaitToolName)
	built, err = coord.buildTools(t.Context(), allowed, false)
	require.NoError(t, err)
	require.Contains(t, toolNames(t, built), tools.ThreadWaitToolName,
		"thread_wait must still be constructible/registerable when an agent config explicitly allows it")
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
