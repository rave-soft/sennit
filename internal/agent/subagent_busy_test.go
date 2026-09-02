package agent

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/configruntime"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/stretchr/testify/require"
)

// busyProbeAgent is a SessionAgent that runs a callback instead of a turn.
// Only Run and Model are reachable from runSubAgent; the embedded (nil)
// interface supplies the rest of the method set, and calling any of them
// would panic loudly rather than pass silently.
type busyProbeAgent struct {
	SessionAgent
	model Model
	run   func(call SessionAgentCall)
}

func (b *busyProbeAgent) Model() Model { return b.model }

func (b *busyProbeAgent) Run(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	b.run(call)
	return &fantasy.AgentResult{
		Response: fantasy.Response{
			Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}},
		},
	}, nil
}

// TestRunSubAgentReportsChildSessionBusyWhileRunning pins the fix for a
// sub-agent view whose spinner never ticked: a delegate runs on its own
// SessionAgent, hence its own dispatcher, so the coordinator's
// currentAgent has no dispatch state for the child session and reported
// a delegation in full flight as idle. The UI arms a loaded session's
// animations only for a session the agent calls busy, so navigating into
// a working sub-agent drew a frozen spinner.
func TestRunSubAgentReportsChildSessionBusyWhileRunning(t *testing.T) {
	coord := newSubAgentBusyTestCoordinator(t)

	parent, err := coord.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)

	childID := session.CreateAgentToolSessionID("msg-1", "call-1")

	var busyDuringRun, busyAfterRun bool
	delegate := &busyProbeAgent{
		model: Model{ModelCfg: config.SelectedModel{Provider: "mock", Model: "mock-model"}},
		run: func(call SessionAgentCall) {
			require.Equal(t, childID, call.SessionID)
			busyDuringRun = coord.IsSessionBusy(call.SessionID)
		},
	}

	resp, err := coord.delegation.runSubAgent(t.Context(), subAgentParams{
		Agent:          delegate,
		SessionID:      parent.ID,
		AgentMessageID: "msg-1",
		ToolCallID:     "call-1",
		Prompt:         "look into X",
		SessionTitle:   "probe",
		AgentID:        "probe",
	})
	require.NoError(t, err)
	require.False(t, resp.IsError, "delegation should have succeeded: %s", resp.Content)

	busyAfterRun = coord.IsSessionBusy(childID)
	require.True(t, busyDuringRun, "child session must read as busy while its delegation runs")
	require.False(t, busyAfterRun, "child session must read as idle once its delegation returns")
}

// TestSubSessionBusyCountsConcurrentDelegations proves the release is
// per-run rather than a plain flag: a session id is derived from the
// message and tool-call ids, so a retried call reuses it, and a first
// release must not report the second run as idle.
func TestSubSessionBusyCountsConcurrentDelegations(t *testing.T) {
	coord := &coordinator{}
	coord.newCoordinatorComponents()
	coord.dispatcher.agentPort.set(&sessionAgent{dispatcher: newDispatcher()})

	releaseFirst := coord.delegation.markSubSessionBusy("child")
	releaseSecond := coord.delegation.markSubSessionBusy("child")
	require.True(t, coord.IsSessionBusy("child"))

	releaseFirst()
	require.True(t, coord.IsSessionBusy("child"), "the second run is still in flight")

	releaseFirst() // idempotent: must not decrement a second time
	require.True(t, coord.IsSessionBusy("child"))

	releaseSecond()
	require.False(t, coord.IsSessionBusy("child"))
}

// newSubAgentBusyTestCoordinator builds a coordinator with the hermetic
// mock provider runSubAgent needs to resolve the delegate's model, plus a
// real session store so the child session can actually be created.
func newSubAgentBusyTestCoordinator(t *testing.T) *coordinator {
	t.Helper()
	env := testEnv(t)

	writeGlobalConfig(t, `{
  "options": {"disable_default_providers": true},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192, "default_max_tokens": 128}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}
}`)

	cfg, err := configruntime.Load(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.SetupAgents()

	c := &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		history:     env.history,
		filetracker: *env.filetracker,
		mcp:         mcp.NewRegistry(),
		background:  shell.NewBackgroundShellManager(),
	}
	c.newCoordinatorComponents()
	c.dispatcher.agentPort.set(&sessionAgent{dispatcher: newDispatcher()})
	return c
}
