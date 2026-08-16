package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/stretchr/testify/require"
)

// fakeTaskManager is a minimal tools.TaskManager standing in for
// internal/thread.TaskManager: it records what it was asked to create and
// returns a canned result without doing any actual work. That is enough
// to prove the agent tool's background branch dispatches correctly and
// returns without waiting — a real delegation run is exercised by
// internal/thread's own tests, not here.
type fakeTaskManager struct {
	created      []tools.TaskCreateArgs
	info         tools.TaskInfo
	err          error
	cancelCalled bool
}

func (f *fakeTaskManager) Create(_ context.Context, args tools.TaskCreateArgs) (tools.TaskInfo, error) {
	f.created = append(f.created, args)
	if f.err != nil {
		return tools.TaskInfo{}, f.err
	}
	return f.info, nil
}

// List, Get, Cancel, Send, and Output are unused by these tests (which
// only exercise the "agent" tool's background-create path) and exist
// solely to satisfy tools.TaskManager; see internal/agent/tools' own
// tests for the task_* tools these back.
func (f *fakeTaskManager) List(context.Context) ([]tools.TaskInfo, error) { return nil, nil }

func (f *fakeTaskManager) Get(context.Context, string) (tools.TaskInfo, error) {
	return tools.TaskInfo{}, nil
}

func (f *fakeTaskManager) Cancel(context.Context, string, string) error {
	f.cancelCalled = true
	return nil
}

func (f *fakeTaskManager) Send(context.Context, string, string) error { return nil }

func (f *fakeTaskManager) Output(context.Context, string, int) (tools.TaskOutput, error) {
	return tools.TaskOutput{}, nil
}

// newAgentToolTestCoordinator builds a coordinator with the minimal
// hermetic config buildAgent needs to construct the "agent" tool's own
// sub-agent target — the same config shape newThreadsTestCoordinator uses
// — wired against the given task manager (nil-able).
func newAgentToolTestCoordinator(t *testing.T, tasks tools.TaskManager) *coordinator {
	t.Helper()
	env := testEnv(t)

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

	return &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		history:     env.history,
		filetracker: *env.filetracker,
		mcp:         mcp.NewRegistry(),
		tasks:       tasks,
		background:  shell.NewBackgroundShellManager(),
	}
}

// TestAgentTool_BackgroundCreatesTaskAndReturnsImmediately proves
// background: true dispatches a task delegation and returns its metadata
// without waiting for any subagent work: the fake TaskManager never runs
// anything, yet the call succeeds and reports the task's id, session, and
// status.
func TestAgentTool_BackgroundCreatesTaskAndReturnsImmediately(t *testing.T) {
	fake := &fakeTaskManager{info: tools.TaskInfo{ID: "task-1", SessionID: "child-sess", Status: "running"}}
	coord := newAgentToolTestCoordinator(t, fake)

	tool, err := coord.agentTool(t.Context())
	require.NoError(t, err)

	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "parent-sess")
	input, err := json.Marshal(AgentParams{Prompt: "look into X", Background: true})
	require.NoError(t, err)

	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Input: string(input)})
	require.NoError(t, err)
	require.False(t, resp.IsError, "background call should not be a tool error: %s", resp.Content)

	require.Len(t, fake.created, 1)
	require.Equal(t, tools.TaskCreateArgs{Goal: "look into X", ParentSessionID: "parent-sess"}, fake.created[0])

	var meta AgentBackgroundResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, "task-1", meta.TaskID)
	require.Equal(t, "child-sess", meta.SessionID)
	require.Equal(t, "running", meta.Status)
}

// TestAgentTool_ForegroundUnchanged proves background absent still takes
// today's path byte-for-byte: it reaches the exact same
// agent-message-id-missing guard runSubAgent's callers have always hit
// (no MessageIDContextKey set here), and never touches the task manager.
// If background had silently taken over regardless of the flag, this
// would fail differently — either succeeding unexpectedly, or failing
// with a task-related error instead of this one.
func TestAgentTool_ForegroundUnchanged(t *testing.T) {
	fake := &fakeTaskManager{}
	coord := newAgentToolTestCoordinator(t, fake)

	tool, err := coord.agentTool(t.Context())
	require.NoError(t, err)

	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "parent-sess")
	input, err := json.Marshal(AgentParams{Prompt: "look into X"})
	require.NoError(t, err)

	_, err = tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Input: string(input)})
	require.Error(t, err)
	require.Contains(t, err.Error(), "agent message id missing from context")

	require.Empty(t, fake.created, "foreground path must never touch the task manager")
}

// TestAgentTool_BackgroundUnavailableReturnsClearError proves a
// background request fails with a clear, model-visible message when no
// task manager is wired, rather than silently running the prompt in the
// foreground instead — which would make the model believe work is
// proceeding in parallel when it is not.
func TestAgentTool_BackgroundUnavailableReturnsClearError(t *testing.T) {
	coord := newAgentToolTestCoordinator(t, nil)

	tool, err := coord.agentTool(t.Context())
	require.NoError(t, err)

	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "parent-sess")
	input, err := json.Marshal(AgentParams{Prompt: "look into X", Background: true})
	require.NoError(t, err)

	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Input: string(input)})
	require.NoError(t, err)
	require.True(t, resp.IsError, "unavailable background must be a clear tool error, not a silent foreground fallback")
	require.Contains(t, resp.Content, "background unset")
}

// TestRunBackgroundAgent_RefusedWhenDisabledByConfig proves
// options.background_agents = false refuses a background dispatch with a
// clear, model-visible message telling it to retry in the foreground - the
// same shape as the no-manager refusal above - and never reaches the task
// manager at all, even though one is wired.
func TestRunBackgroundAgent_RefusedWhenDisabledByConfig(t *testing.T) {
	fake := &fakeTaskManager{info: tools.TaskInfo{ID: "task-1", SessionID: "child-sess", Status: "running"}}
	coord := newAgentToolTestCoordinator(t, fake)
	disabled := false
	coord.cfg.Config().Options.BackgroundAgents = &disabled

	resp, err := coord.runBackgroundAgent(t.Context(), "parent-sess", "look into X")
	require.NoError(t, err)
	require.True(t, resp.IsError, "a disabled switch must refuse, not silently run in the foreground")
	require.Contains(t, resp.Content, "background_agents")
	require.Contains(t, resp.Content, "background unset")
	require.Empty(t, fake.created, "a refused dispatch must never reach the task manager")
}

// TestRunBackgroundAgent_AllowedWhenExplicitlyEnabled proves the option's
// true value dispatches exactly like its unset (default) value already
// covered by TestAgentTool_BackgroundCreatesTaskAndReturnsImmediately.
func TestRunBackgroundAgent_AllowedWhenExplicitlyEnabled(t *testing.T) {
	fake := &fakeTaskManager{info: tools.TaskInfo{ID: "task-1", SessionID: "child-sess", Status: "running"}}
	coord := newAgentToolTestCoordinator(t, fake)
	enabled := true
	coord.cfg.Config().Options.BackgroundAgents = &enabled

	resp, err := coord.runBackgroundAgent(t.Context(), "parent-sess", "look into X")
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Len(t, fake.created, 1)
}

// TestBackgroundAgents_ToggleOffDoesNotTouchInFlightTask proves what the
// gate is documented to mean (see Options.BackgroundAgents and
// coordinator.backgroundAgentsEnabled): turning the switch off only blocks
// *new* dispatch through this same call from here on - it never reaches
// into the task manager to cancel, list, or otherwise touch a task already
// running. A config reload flips the option by swapping in a new *Config
// (ConfigStore.Config's own doc comment), which is simulated here the same
// way the rest of this package's tests mutate live Options in place (see
// coderAgent in common_test.go) - the read path (backgroundAgentsEnabled)
// is identical either way, since it always re-reads c.cfg.Config() fresh.
func TestBackgroundAgents_ToggleOffDoesNotTouchInFlightTask(t *testing.T) {
	fake := &fakeTaskManager{info: tools.TaskInfo{ID: "task-1", SessionID: "child-sess", Status: "running"}}
	coord := newAgentToolTestCoordinator(t, fake)

	// Dispatched while enabled - this is the "in-flight task" the reload
	// below must leave alone.
	resp, err := coord.runBackgroundAgent(t.Context(), "parent-sess", "do work")
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Len(t, fake.created, 1)

	disabled := false
	coord.cfg.Config().Options.BackgroundAgents = &disabled

	// New dispatch is refused from here on...
	resp, err = coord.runBackgroundAgent(t.Context(), "parent-sess", "more work")
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Len(t, fake.created, 1, "the refused call must never reach the task manager")

	// ...but nothing about the toggle itself reaches into the task
	// manager: no Cancel call was ever made as a side effect of it.
	require.False(t, fake.cancelCalled, "toggling the option off must not cancel a task already running")
}
