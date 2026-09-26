package agent

import (
	"slices"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/configruntime"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/stretchr/testify/require"
)

// checkBusyActivityInvariant asserts, for sessionID, that
// Coordinator.BusySessions and Coordinator.SessionsWithQueuedPrompts agree
// with coord.IsSessionBusy and coord.QueuedPromptsList exactly as the
// build step's brief requires: the enumeration must never say something
// different from what the parameterized getters themselves would answer
// for the same session id, at any point in the sequence below.
func checkBusyActivityInvariant(t *testing.T, coord *coordinator, sessionID string) {
	t.Helper()

	wantBusy := coord.IsSessionBusy(sessionID)
	gotBusy := slices.Contains(coord.BusySessions(), sessionID)
	require.Equal(t, wantBusy, gotBusy,
		"BusySessions() must agree with IsSessionBusy(%q)", sessionID)

	wantQueue := coord.QueuedPromptsList(sessionID)
	gotQueue, present := coord.SessionsWithQueuedPrompts()[sessionID]
	if len(wantQueue) == 0 {
		require.False(t, present,
			"SessionsWithQueuedPrompts() must omit %q when it has no queue", sessionID)
	} else {
		require.True(t, present, "SessionsWithQueuedPrompts() must include %q", sessionID)
		require.Equal(t, wantQueue, gotQueue)
	}
}

// TestBusyActivityInvariant_MatchesIsSessionBusyAndQueuedPromptsList drives
// a sequence covering every source IsSessionBusy/QueuedPromptsList
// consult — a turn starting, a follow-up queuing behind it, the turn
// finishing (draining the queue), a turn being cancelled outright, and a
// delegation child tracked only by the delegation finalizer's sub-session
// counter — checking the invariant above for every session id involved
// after each step.
//
// The delegation branch is the one a fix can close on only one of two
// surfaces (see AGENTS.md's "enumerate every surface" rule):
// BusySessions must fold in delegationFinalizer.subSessions as well as
// the dispatcher's own active-run state, or a sub-agent with nothing
// queued and no top-level dispatch entry would silently vanish from the
// enumeration while IsSessionBusy still reports it busy.
func TestBusyActivityInvariant_MatchesIsSessionBusyAndQueuedPromptsList(t *testing.T) {
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

	coord := &coordinator{
		agentDeps: agentDeps{
			cfg:         cfg,
			sessions:    env.sessions,
			messages:    env.messages,
			permissions: env.permissions,
			history:     env.history,
			filetracker: *env.filetracker,
			mcp:         mcp.NewRegistry(),
			background:  shell.NewBackgroundShellManager(),
		},
	}
	coord.newCoordinatorComponents()

	gated := &gatedStreamModel{
		text:    "done",
		gate:    make(chan struct{}),
		entered: make(chan struct{}),
	}
	sa := NewSessionAgent(SessionAgentOptions{
		Model:    Model{Model: gated, CatalogCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)
	coord.dispatcher.agentPort.set(sa)

	parent, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)

	// Step 0: nothing running yet.
	checkBusyActivityInvariant(t, coord, parent.ID)

	// Step 1: start a turn; it blocks inside Stream once active.
	mainDone := make(chan error, 1)
	go func() {
		_, runErr := sa.Run(t.Context(), SessionAgentCall{SessionID: parent.ID, Prompt: "main"})
		mainDone <- runErr
	}()
	select {
	case <-gated.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("main run never entered Stream")
	}
	require.True(t, coord.IsSessionBusy(parent.ID))
	checkBusyActivityInvariant(t, coord, parent.ID)

	// Step 2: queue a follow-up behind the busy session.
	res, err := sa.Run(t.Context(), SessionAgentCall{SessionID: parent.ID, Prompt: "follow"})
	require.NoError(t, err)
	require.Nil(t, res)
	require.Equal(t, 1, coord.QueuedPrompts(parent.ID))
	checkBusyActivityInvariant(t, coord, parent.ID)

	// Step 3: a delegation child, tracked only by the delegation
	// finalizer, is busy at the same time as the parent's own turn.
	childID := "sub-agent-child"
	releaseChild := coord.delegation.markSubSessionBusy(childID)
	require.True(t, coord.IsSessionBusy(childID))
	checkBusyActivityInvariant(t, coord, childID)
	checkBusyActivityInvariant(t, coord, parent.ID)

	// Step 4: the turn finishes, draining the queued follow-up into a
	// recursive run that then also finishes.
	close(gated.gate)
	require.NoError(t, <-mainDone)
	require.Eventually(t, func() bool {
		return !coord.IsSessionBusy(parent.ID) && coord.QueuedPrompts(parent.ID) == 0
	}, 5*time.Second, 10*time.Millisecond)
	checkBusyActivityInvariant(t, coord, parent.ID)

	// The delegation child is still busy: finishing the parent's own
	// turn must not affect a session busy only via subSessions.
	checkBusyActivityInvariant(t, coord, childID)
	releaseChild()
	require.False(t, coord.IsSessionBusy(childID))
	checkBusyActivityInvariant(t, coord, childID)

	// Step 5: start another turn and cancel it outright.
	gated2 := &gatedStreamModel{
		text:    "done",
		gate:    make(chan struct{}),
		entered: make(chan struct{}),
	}
	sa2 := NewSessionAgent(SessionAgentOptions{
		Model:    Model{Model: gated2, CatalogCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)
	coord.dispatcher.agentPort.set(sa2)

	cancelDone := make(chan error, 1)
	go func() {
		_, runErr := sa2.Run(t.Context(), SessionAgentCall{SessionID: parent.ID, Prompt: "to-cancel"})
		cancelDone <- runErr
	}()
	select {
	case <-gated2.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel-bound run never entered Stream")
	}
	// Queue a follow-up too, so Cancel's queue-drop path is exercised.
	res, err = sa2.Run(t.Context(), SessionAgentCall{SessionID: parent.ID, Prompt: "queued-then-dropped"})
	require.NoError(t, err)
	require.Nil(t, res)
	require.Equal(t, 1, coord.QueuedPrompts(parent.ID))
	checkBusyActivityInvariant(t, coord, parent.ID)

	coord.Cancel(parent.ID)
	<-cancelDone
	require.Eventually(t, func() bool {
		return !coord.IsSessionBusy(parent.ID) && coord.QueuedPrompts(parent.ID) == 0
	}, 5*time.Second, 10*time.Millisecond)
	checkBusyActivityInvariant(t, coord, parent.ID)
}
