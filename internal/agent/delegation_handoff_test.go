package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

type handoffManager struct {
	fakeTaskManager
	entered chan struct{}
	gate    chan struct{}
	exited  chan struct{}
}

func (m *handoffManager) Wait(ctx context.Context, _ []string) error {
	close(m.entered)
	defer close(m.exited)
	select {
	case <-m.gate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type handoffModel struct {
	toolStepModel
	next chan string
}

func (m *handoffModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if m.steps.Load() == 1 && !isTitleCall(call) {
		body, _ := json.Marshal(call)
		m.next <- string(body)
	}
	stream, err := m.toolStepModel.Stream(ctx, call)
	if err != nil {
		return nil, err
	}
	return func(yield func(fantasy.StreamPart) bool) {
		stream(func(part fantasy.StreamPart) bool {
			if part.ToolCallName == "hold" {
				part.ToolCallName = "agent"
			}
			if part.Type == fantasy.StreamPartTypeToolInputDelta {
				part.Delta = `{"prompt":"investigate"}`
			}
			if part.Type == fantasy.StreamPartTypeToolCall {
				part.ToolCallInput = `{"prompt":"investigate"}`
			}
			return yield(part)
		})
	}, nil
}

func TestDelegationHandoffNextProviderStep(t *testing.T) {
	for _, steer := range []bool{false, true} {
		t.Run(map[bool]string{false: "terminal", true: "steering"}[steer], func(t *testing.T) {
			env := testEnv(t)
			manager := &handoffManager{fakeTaskManager: fakeTaskManager{info: tools.TaskInfo{ID: "task-1", SessionID: "child", Status: "completed", ResultSummary: "unique delegated report"}}, entered: make(chan struct{}), gate: make(chan struct{}), exited: make(chan struct{})}
			coord := newAgentToolTestCoordinator(t, manager)
			tool, err := coord.delegation.agentTool(t.Context(), newAgentConfig(coord.cfg.Config()), true)
			require.NoError(t, err)
			model := &handoffModel{toolStepModel: toolStepModel{text: "done", toolID: "agent-call"}, next: make(chan string, 1)}
			sa := NewSessionAgent(SessionAgentOptions{Model: Model{Model: model, CatalogCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}}, Sessions: env.sessions, Messages: env.messages, Tools: []fantasy.AgentTool{tool}}).(*sessionAgent)
			sess, err := env.sessions.Create(t.Context(), "parent")
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := sa.Run(ctx, SessionAgentCall{SessionID: sess.ID, Prompt: "delegate"}); done <- err }()
			select {
			case <-manager.entered:
			case <-ctx.Done():
				t.Fatal("delegation wait not entered")
			}
			select {
			case body := <-model.next:
				t.Fatalf("provider advanced before completion: %s", body)
			default:
			}
			var acknowledged atomic.Bool
			if steer {
				outcome, _, err := sa.Steer(ctx, SessionAgentCall{SessionID: sess.ID, Prompt: "new person instruction"})
				require.NoError(t, err)
				require.Equal(t, SteerEnqueued, outcome)
			} else {
				sa.DeliverTaskCompletion(ctx, sess.ID, TaskCompletion{DelegationID: "task-1", ChildSessionID: "child", Status: "completed", ResultText: "unique delegated report", TerminalAt: time.Now(), Acknowledge: func(context.Context) error { acknowledged.Store(true); return nil }})
				close(manager.gate)
			}
			require.NoError(t, <-done)
			body := <-model.next
			if steer {
				require.Contains(t, body, "new person instruction")
				require.Contains(t, body, "person sent a message")
				require.NotContains(t, body, "unique delegated report")
			} else {
				require.Contains(t, body, "unique delegated report")
				require.Contains(t, body, "Delegation task-1 completed")
				require.Equal(t, 2, strings.Count(body, "unique delegated report"))
				require.True(t, acknowledged.Load())
				require.Empty(t, sa.drainCompletionsForStep(sess.ID))
			}
			select {
			case <-manager.exited:
			default:
				t.Fatal("wait worker not joined")
			}
			require.Equal(t, int32(2), model.steps.Load())
		})
	}
}

func TestDelegationTerminalFailureResults(t *testing.T) {
	for _, status := range []string{"failed", "cancelled", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			manager := &fakeTaskManager{info: tools.TaskInfo{ID: "task", Status: status, Error: "terminal reason"}}
			coord := newAgentToolTestCoordinator(t, manager)
			tool, err := coord.delegation.agentTool(t.Context(), newAgentConfig(coord.cfg.Config()), true)
			require.NoError(t, err)
			response, err := tool.Run(context.WithValue(t.Context(), tools.SessionIDContextKey, "parent"), fantasy.ToolCall{ID: "call", Input: `{"prompt":"work"}`})
			require.NoError(t, err)
			require.False(t, response.IsError)
			require.Contains(t, response.Content, "status="+status)
			require.Contains(t, response.Content, "terminal reason")
		})
	}
}
