package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rave-soft/braid/internal/agent/tools"
	"github.com/rave-soft/braid/internal/agent/tools/mcp"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/shell"
	"github.com/stretchr/testify/require"
)

// noopTaskManager is a minimal tools.TaskManager with no behavior: these
// tests only check whether the task_* tools are offered to an agent, not
// what they do (see internal/agent/tools/task_tools_test.go and
// internal/thread for that).
type noopTaskManager struct{}

func (noopTaskManager) Create(context.Context, tools.TaskCreateArgs) (tools.TaskInfo, error) {
	return tools.TaskInfo{}, nil
}

func (noopTaskManager) List(context.Context) ([]tools.TaskInfo, error) { return nil, nil }

func (noopTaskManager) Get(context.Context, string) (tools.TaskInfo, error) {
	return tools.TaskInfo{}, nil
}

func (noopTaskManager) Cancel(context.Context, string, string) error { return nil }

func (noopTaskManager) Send(context.Context, string, string) error { return nil }

func (noopTaskManager) Output(context.Context, string, int) (tools.TaskOutput, error) {
	return tools.TaskOutput{}, nil
}

var taskToolNames = []string{
	tools.TaskListToolName,
	tools.TaskResultToolName,
	tools.TaskCancelToolName,
	tools.TaskSendToolName,
	tools.TaskOutputToolName,
}

// newTasksTestCoordinator mirrors newThreadsTestCoordinator, wired
// against the given task manager (nil-able) instead of a thread one.
func newTasksTestCoordinator(t *testing.T, taskManager tools.TaskManager) (*coordinator, config.Agent) {
	t.Helper()
	env := testEnv(t)

	// Minimal hermetic config: one openai-typed provider with a selected
	// model, so buildAgentModel (reached through the "agent" delegation
	// tool that buildTools always tries to build) succeeds without any
	// real network access — see newThreadsTestCoordinator's identical
	// setup in coordinator_threads_test.go.
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
		tasks:       taskManager,
		background:  shell.NewBackgroundShellManager(),
	}
	return coord, cfg.Config().Agents[config.AgentCoder]
}

func TestBuildTools_TaskToolsPresentForMainAgentWithManager(t *testing.T) {
	coord, agentCfg := newTasksTestCoordinator(t, noopTaskManager{})

	built, err := coord.buildTools(t.Context(), agentCfg, false)
	require.NoError(t, err)

	names := toolNames(t, built)
	for _, want := range taskToolNames {
		require.Contains(t, names, want)
	}
}

func TestBuildTools_TaskToolsAbsentWhenManagerNil(t *testing.T) {
	coord, agentCfg := newTasksTestCoordinator(t, nil)

	built, err := coord.buildTools(t.Context(), agentCfg, false)
	require.NoError(t, err)

	names := toolNames(t, built)
	for _, absent := range taskToolNames {
		require.NotContains(t, names, absent)
	}
}

func TestBuildTools_TaskToolsAbsentForSubAgent(t *testing.T) {
	coord, agentCfg := newTasksTestCoordinator(t, noopTaskManager{})

	built, err := coord.buildTools(t.Context(), agentCfg, true)
	require.NoError(t, err)

	names := toolNames(t, built)
	for _, absent := range taskToolNames {
		require.NotContains(t, names, absent)
	}
}

// TestBuildTools_TaskToolsAbsentWhenBackgroundAgentsDisabled proves
// options.background_agents is a further, explicit gate on top of "is a
// task manager wired": even with a real manager, an off switch hides the
// task_* tools entirely.
func TestBuildTools_TaskToolsAbsentWhenBackgroundAgentsDisabled(t *testing.T) {
	coord, agentCfg := newTasksTestCoordinator(t, noopTaskManager{})
	disabled := false
	coord.cfg.Config().Options.BackgroundAgents = &disabled

	built, err := coord.buildTools(t.Context(), agentCfg, false)
	require.NoError(t, err)

	names := toolNames(t, built)
	for _, absent := range taskToolNames {
		require.NotContains(t, names, absent, "task tools must not be registered when options.background_agents is off")
	}
}

// TestBuildTools_TaskToolsPresentWhenBackgroundAgentsExplicitlyEnabled
// proves the option's true value behaves the same as its unset (default)
// value already covered by TestBuildTools_TaskToolsPresentForMainAgentWithManager.
func TestBuildTools_TaskToolsPresentWhenBackgroundAgentsExplicitlyEnabled(t *testing.T) {
	coord, agentCfg := newTasksTestCoordinator(t, noopTaskManager{})
	enabled := true
	coord.cfg.Config().Options.BackgroundAgents = &enabled

	built, err := coord.buildTools(t.Context(), agentCfg, false)
	require.NoError(t, err)

	names := toolNames(t, built)
	for _, want := range taskToolNames {
		require.Contains(t, names, want)
	}
}

func TestCoordinator_SetTasksTakesEffectImmediately(t *testing.T) {
	coord, agentCfg := newTasksTestCoordinator(t, nil)

	built, err := coord.buildTools(t.Context(), agentCfg, false)
	require.NoError(t, err)
	require.NotContains(t, toolNames(t, built), tools.TaskListToolName)

	coord.SetTasks(noopTaskManager{})

	built, err = coord.buildTools(t.Context(), agentCfg, false)
	require.NoError(t, err)
	require.Contains(t, toolNames(t, built), tools.TaskListToolName)
}
