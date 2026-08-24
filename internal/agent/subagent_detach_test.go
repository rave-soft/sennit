package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/stretchr/testify/require"
)

// capturingSessionAgent is a minimal SessionAgent fake standing in for
// currentAgent in the detach tests: its only reachable method is
// DeliverTaskCompletion, which records what it was handed instead of
// running the real dispatcher/continuation machinery those calls would
// otherwise trigger - that machinery is already exercised by
// continuation_test.go, and is not what these tests are about. Every
// other SessionAgent method is promoted from the embedded (nil)
// interface and would panic loudly if reached.
type capturingSessionAgent struct {
	SessionAgent
	notify chan struct{}

	mu           sync.Mutex
	delivered    []capturedCompletion
	cancelled    []string
	allCancelled bool
}

// Cancel and CancelAll stand in for the dispatcher-backed currentAgent
// the coordinator would normally hold, recording what they were called
// with instead of reaching into real dispatch state - the detach-cancel
// tests only need to see that coordinator.Cancel/CancelAll still reach
// currentAgent, not exercise the dispatcher itself (already covered
// elsewhere).
func (f *capturingSessionAgent) Cancel(sessionID string) {
	f.mu.Lock()
	f.cancelled = append(f.cancelled, sessionID)
	f.mu.Unlock()
}

func (f *capturingSessionAgent) CancelAll() {
	f.mu.Lock()
	f.allCancelled = true
	f.mu.Unlock()
}

func (f *capturingSessionAgent) cancelledSnapshot() (ids []string, all bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cancelled...), f.allCancelled
}

type capturedCompletion struct {
	sessionID  string
	completion TaskCompletion
}

func (f *capturingSessionAgent) IsSessionBusy(string) bool { return false }

func (f *capturingSessionAgent) DeliverTaskCompletion(_ context.Context, sessionID string, completion TaskCompletion) {
	f.mu.Lock()
	f.delivered = append(f.delivered, capturedCompletion{sessionID: sessionID, completion: completion})
	f.mu.Unlock()
	if f.notify != nil {
		f.notify <- struct{}{}
	}
}

func (f *capturingSessionAgent) snapshot() []capturedCompletion {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]capturedCompletion(nil), f.delivered...)
}

// newSubAgentDetachTestCoordinator builds a coordinator with the same
// hermetic mock provider newSubAgentBusyTestCoordinator uses, wired
// against the given task manager and currentAgent, plus extraOptions -
// raw JSON object fields (leading comma included) merged into the
// config's "options" block, for the gate tests that need
// background_agents turned off.
func newSubAgentDetachTestCoordinator(t *testing.T, tasks tools.TaskManager, currentAgent SessionAgent, extraOptions string) *coordinator {
	t.Helper()
	env := testEnv(t)

	writeGlobalConfig(t, fmt.Sprintf(`{
  "options": {"disable_default_providers": true%s},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192, "default_max_tokens": 128}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}
}`, extraOptions))

	cfg, err := config.Load(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.SetupAgents()

	return &coordinator{
		cfg:          cfg,
		sessions:     env.sessions,
		messages:     env.messages,
		permissions:  env.permissions,
		history:      env.history,
		filetracker:  *env.filetracker,
		mcp:          mcp.NewRegistry(),
		background:   shell.NewBackgroundShellManager(),
		tasks:        tasks,
		currentAgent: currentAgent,
	}
}

// blockingDelegate is a busyProbeAgent whose Run blocks until told to
// proceed, closing entered the moment it starts so a test can wait for
// the child run to actually be in flight before acting on it.
func blockingDelegate(t *testing.T, entered, proceed chan struct{}, sessionID string) *busyProbeAgent {
	t.Helper()
	return &busyProbeAgent{
		model: Model{ModelCfg: config.SelectedModel{Provider: "mock", Model: "mock-model"}},
		run: func(call SessionAgentCall) {
			require.Equal(t, sessionID, call.SessionID)
			close(entered)
			<-proceed
		},
	}
}

// cancelableDelegate is a SessionAgent whose Run blocks until either
// proceed is closed (a normal finish, reporting "done" like
// busyProbeAgent) or its own context is canceled, in which case it
// reports context.Canceled - standing in for a detached delegation's
// child run reacting to Cancel/CancelAll/Close cancelling it through
// the detached-delegation registry (see coordinator.
// cancelDetachedDelegations). Unlike blockingDelegate/busyProbeAgent,
// which ignore ctx entirely, this is the fixture the cancel-ownership
// tests need; existing tests keep using busyProbeAgent unchanged.
type cancelableDelegate struct {
	SessionAgent
	model   Model
	entered chan struct{}
	proceed chan struct{}
}

func (d *cancelableDelegate) Model() Model { return d.model }

func (d *cancelableDelegate) Run(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	close(d.entered)
	select {
	case <-d.proceed:
		return &fantasy.AgentResult{
			Response: fantasy.Response{
				Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}},
			},
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type subAgentRunOutcome struct {
	resp fantasy.ToolResponse
	err  error
}

// TestRunSubAgent_DetachesOnUserInput_DeliversCompletionLater is the
// core proof for the detach feature: while a Detachable delegation is
// still running, closing the user-input signal returns runSubAgent
// immediately with AgentDetachedResponseMetadata, without waiting for
// the child run - which keeps going on its own and, once it finishes,
// delivers a TaskCompletion to the parent with the fields the model
// needs to make sense of it.
func TestRunSubAgent_DetachesOnUserInput_DeliversCompletionLater(t *testing.T) {
	fake := &fakeTaskManager{info: tools.TaskInfo{ID: "task-1", SessionID: "child-sess", Status: "running"}}
	capture := &capturingSessionAgent{notify: make(chan struct{}, 1)}
	coord := newSubAgentDetachTestCoordinator(t, fake, capture, "")

	parent, err := coord.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	childID := coord.sessions.CreateAgentToolSessionID("msg-1", "call-1")

	entered := make(chan struct{})
	proceed := make(chan struct{})
	delegate := blockingDelegate(t, entered, proceed, childID)

	userInput := make(chan struct{})
	ctx := tools.WithUserInput(t.Context(), func() <-chan struct{} { return userInput })

	respCh := make(chan subAgentRunOutcome, 1)
	go func() {
		resp, err := coord.runSubAgent(ctx, subAgentParams{
			Agent:          delegate,
			SessionID:      parent.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "look into X",
			SessionTitle:   "probe",
			AgentID:        "probe",
			Detachable:     true,
		})
		respCh <- subAgentRunOutcome{resp: resp, err: err}
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("delegate never entered Run")
	}
	require.True(t, coord.IsSessionBusy(childID), "child must read as busy before detaching")

	// The person sends a new message to the parent session.
	close(userInput)

	var out subAgentRunOutcome
	select {
	case out = <-respCh:
	case <-time.After(5 * time.Second):
		t.Fatal("runSubAgent did not detach promptly")
	}
	require.NoError(t, out.err)
	require.False(t, out.resp.IsError, "detaching must not be reported as a tool error: %s", out.resp.Content)
	require.Contains(t, out.resp.Content, childID)
	require.Contains(t, out.resp.Content, "moved to the background")

	var meta AgentDetachedResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(out.resp.Metadata), &meta))
	require.Equal(t, childID, meta.SessionID)

	// The child keeps running after detaching - nothing about the
	// delegation's own progress changed, only who is waiting on it.
	require.True(t, coord.IsSessionBusy(childID))
	require.Empty(t, capture.snapshot(), "no completion before the child run actually finishes")

	close(proceed)

	select {
	case <-capture.notify:
	case <-time.After(5 * time.Second):
		t.Fatal("detached delegation's completion was never delivered")
	}

	delivered := capture.snapshot()
	require.Len(t, delivered, 1)
	got := delivered[0]
	require.Equal(t, parent.ID, got.sessionID, "completion must target the parent session, never the child")
	require.Equal(t, childID, got.completion.DelegationID)
	require.Equal(t, "delegation", got.completion.Kind)
	require.Equal(t, "probe", got.completion.Name)
	require.Equal(t, "look into X", got.completion.Goal)
	require.Equal(t, "completed", got.completion.Status)
	require.Equal(t, childID, got.completion.ChildSessionID)
	require.Equal(t, "done", got.completion.ResultText)
	require.Equal(t, 0, got.completion.Depth)
	require.False(t, got.completion.TerminalAt.IsZero())
	require.Empty(t, got.completion.Error)

	require.False(t, coord.IsSessionBusy(childID), "child must no longer be busy once its detached run finishes")
}

// TestRunSubAgent_DetachedRunFailureDeliversFailedCompletion proves the
// failure half of the same path: a detached delegation whose child run
// errors delivers a "failed" completion carrying the error text, not a
// "completed" one with an empty result.
func TestRunSubAgent_DetachedRunFailureDeliversFailedCompletion(t *testing.T) {
	fake := &fakeTaskManager{info: tools.TaskInfo{ID: "task-1", SessionID: "child-sess", Status: "running"}}
	capture := &capturingSessionAgent{notify: make(chan struct{}, 1)}
	coord := newSubAgentDetachTestCoordinator(t, fake, capture, "")

	parent, err := coord.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	childID := coord.sessions.CreateAgentToolSessionID("msg-1", "call-1")

	entered := make(chan struct{})
	proceed := make(chan struct{})
	delegate := &busyProbeAgent{
		model: Model{ModelCfg: config.SelectedModel{Provider: "mock", Model: "mock-model"}},
	}
	delegate.run = func(call SessionAgentCall) {
		require.Equal(t, childID, call.SessionID)
		close(entered)
		<-proceed
	}
	// busyProbeAgent.Run always returns a fixed success; override Run
	// through an embedding wrapper so this one test can report a
	// failure instead, without changing the shared fixture other tests
	// rely on.
	failingDelegate := &failingProbeAgent{busyProbeAgent: delegate}

	userInput := make(chan struct{})
	ctx := tools.WithUserInput(t.Context(), func() <-chan struct{} { return userInput })

	respCh := make(chan subAgentRunOutcome, 1)
	go func() {
		resp, err := coord.runSubAgent(ctx, subAgentParams{
			Agent:          failingDelegate,
			SessionID:      parent.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "look into X",
			SessionTitle:   "probe",
			AgentID:        "probe",
			Detachable:     true,
		})
		respCh <- subAgentRunOutcome{resp: resp, err: err}
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("delegate never entered Run")
	}
	close(userInput)

	select {
	case <-respCh:
	case <-time.After(5 * time.Second):
		t.Fatal("runSubAgent did not detach promptly")
	}

	close(proceed)

	select {
	case <-capture.notify:
	case <-time.After(5 * time.Second):
		t.Fatal("detached delegation's completion was never delivered")
	}

	delivered := capture.snapshot()
	require.Len(t, delivered, 1)
	require.Equal(t, "failed", delivered[0].completion.Status)
	require.Equal(t, "boom", delivered[0].completion.Error)
	require.Empty(t, delivered[0].completion.ResultText)
}

// failingProbeAgent wraps busyProbeAgent to report a run failure instead
// of its fixed success, for
// TestRunSubAgent_DetachedRunFailureDeliversFailedCompletion.
type failingProbeAgent struct {
	*busyProbeAgent
}

func (f *failingProbeAgent) Run(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	_, _ = f.busyProbeAgent.Run(ctx, call)
	return nil, fmt.Errorf("boom")
}

// TestRunSubAgent_NoUserInputSignal_BlocksAndReturnsResultNormally
// proves the feature is additive: a Detachable delegation with no
// user-input signal wired into its context (a build path that never
// arms one) behaves exactly as before - runSubAgent blocks for the
// child's own result and no completion is ever delivered.
func TestRunSubAgent_NoUserInputSignal_BlocksAndReturnsResultNormally(t *testing.T) {
	fake := &fakeTaskManager{info: tools.TaskInfo{ID: "task-1", SessionID: "child-sess", Status: "running"}}
	capture := &capturingSessionAgent{notify: make(chan struct{}, 1)}
	coord := newSubAgentDetachTestCoordinator(t, fake, capture, "")

	parent, err := coord.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	childID := coord.sessions.CreateAgentToolSessionID("msg-1", "call-1")

	delegate := &busyProbeAgent{
		model: Model{ModelCfg: config.SelectedModel{Provider: "mock", Model: "mock-model"}},
		run:   func(call SessionAgentCall) {},
	}

	resp, err := coord.runSubAgent(t.Context(), subAgentParams{
		Agent:          delegate,
		SessionID:      parent.ID,
		AgentMessageID: "msg-1",
		ToolCallID:     "call-1",
		Prompt:         "look into X",
		SessionTitle:   "probe",
		AgentID:        "probe",
		Detachable:     true,
	})
	require.NoError(t, err)
	require.False(t, resp.IsError, "delegation should have succeeded: %s", resp.Content)
	require.Equal(t, "done", resp.Content)
	require.Empty(t, resp.Metadata, "an ordinary completion must not carry detach metadata")
	require.False(t, coord.IsSessionBusy(childID))
	require.Empty(t, capture.snapshot(), "no completion is delivered when the delegation never detached")
}

// TestRunSubAgent_DetachGatedOff proves the two config-level gates:
// with the user-input signal present and Detachable set, a delegation
// still does not detach when either options.background_agents is off or
// no task manager is wired - runSubAgent keeps blocking for the child's
// own result exactly as if Detachable had never been set.
func TestRunSubAgent_DetachGatedOff(t *testing.T) {
	cases := []struct {
		name        string
		tasks       tools.TaskManager
		extraOption string
	}{
		{
			name:        "background_agents disabled",
			tasks:       &fakeTaskManager{info: tools.TaskInfo{ID: "task-1", SessionID: "child-sess", Status: "running"}},
			extraOption: `, "background_agents": false`,
		},
		{
			name:  "no task manager wired",
			tasks: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capture := &capturingSessionAgent{notify: make(chan struct{}, 1)}
			coord := newSubAgentDetachTestCoordinator(t, tc.tasks, capture, tc.extraOption)

			parent, err := coord.sessions.Create(t.Context(), "parent")
			require.NoError(t, err)
			childID := coord.sessions.CreateAgentToolSessionID("msg-1", "call-1")

			entered := make(chan struct{})
			proceed := make(chan struct{})
			delegate := blockingDelegate(t, entered, proceed, childID)

			userInput := make(chan struct{})
			ctx := tools.WithUserInput(t.Context(), func() <-chan struct{} { return userInput })

			respCh := make(chan subAgentRunOutcome, 1)
			go func() {
				resp, err := coord.runSubAgent(ctx, subAgentParams{
					Agent:          delegate,
					SessionID:      parent.ID,
					AgentMessageID: "msg-1",
					ToolCallID:     "call-1",
					Prompt:         "look into X",
					SessionTitle:   "probe",
					AgentID:        "probe",
					Detachable:     true,
				})
				respCh <- subAgentRunOutcome{resp: resp, err: err}
			}()

			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("delegate never entered Run")
			}

			// A message arrives, but the gate is closed: this must not
			// be enough to detach.
			close(userInput)

			select {
			case out := <-respCh:
				t.Fatalf("runSubAgent returned before the child run finished: %+v", out)
			case <-time.After(200 * time.Millisecond):
			}

			close(proceed)

			var out subAgentRunOutcome
			select {
			case out = <-respCh:
			case <-time.After(5 * time.Second):
				t.Fatal("runSubAgent never returned once the child run finished")
			}
			require.NoError(t, out.err)
			require.False(t, out.resp.IsError, "delegation should have succeeded: %s", out.resp.Content)
			require.Equal(t, "done", out.resp.Content)
			require.Empty(t, out.resp.Metadata)
			require.Empty(t, capture.snapshot())
		})
	}
}
