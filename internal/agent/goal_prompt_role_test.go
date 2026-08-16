package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/stretchr/testify/require"
)

// promptCapturingModel is a fantasy.LanguageModel shaped like
// recordingModel (title calls are told apart via isTitleCall/titleStream),
// but it additionally records every non-title call's Prompt so a test can
// inspect exactly what reached the model on each turn.
type promptCapturingModel struct {
	provider string
	name     string

	mu      sync.Mutex
	prompts []fantasy.Prompt
	titles  int

	// titleGot is signalled when the title stream arrives, so a test can
	// wait on the detached title goroutine (see agent.go's
	// "go a.generateTitle(...)") without sleeping.
	titleGot chan struct{}
}

func newPromptCapturingModel(provider, name string) *promptCapturingModel {
	return &promptCapturingModel{provider: provider, name: name, titleGot: make(chan struct{}, 1)}
}

// calls returns the Prompt of every non-title call served so far, in order.
func (m *promptCapturingModel) calls() []fantasy.Prompt {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]fantasy.Prompt, len(m.prompts))
	copy(out, m.prompts)
	return out
}

// titleCalls returns how many detached title streams this model served.
func (m *promptCapturingModel) titleCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.titles
}

func (m *promptCapturingModel) Provider() string { return m.provider }
func (m *promptCapturingModel) Model() string    { return m.name }

func (m *promptCapturingModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not implemented")
}

func (m *promptCapturingModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if isTitleCall(call) {
		m.mu.Lock()
		m.titles++
		m.mu.Unlock()
		select {
		case m.titleGot <- struct{}{}:
		default:
		}
		return titleStream()
	}

	m.mu.Lock()
	m.prompts = append(m.prompts, call.Prompt)
	m.mu.Unlock()

	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "1"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "1", Delta: "done"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "1"}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func (m *promptCapturingModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *promptCapturingModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

// promptHasUserText reports whether prompt contains a user-role message
// whose text contains want.
func promptHasUserText(prompt fantasy.Prompt, want string) bool {
	return promptHasRoleText(prompt, fantasy.MessageRoleUser, want)
}

func promptHasRoleText(prompt fantasy.Prompt, role fantasy.MessageRole, want string) bool {
	for _, msg := range prompt {
		if msg.Role != role {
			continue
		}
		for _, part := range msg.Content {
			if tp, ok := part.(fantasy.TextPart); ok && strings.Contains(tp.Text, want) {
				return true
			}
		}
	}
	return false
}

// TestGoalDispatchedWithAgentOriginPersistsAsUserRoleAndReachesModelOnEveryTurn
// is the regression test for the fix that replaced the earlier (wrong)
// role-swapping approach: a thread/task's goal is dispatched with
// PromptOrigin: message.OriginAgent (see agent.WithPromptOrigin and
// thread.Manager.Create/TaskManager.Create), which persists it as an
// ordinary message.User message tagged Origin: message.OriginAgent - never
// message.System, and never any role other than message.User - while still
// reaching the model on every turn, including follow-ups, exactly the way
// any other user turn does.
func TestGoalDispatchedWithAgentOriginPersistsAsUserRoleAndReachesModelOnEveryTurn(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	model := newPromptCapturingModel("fake", "fake-model")
	sa := testSessionAgent(env, model, "system")

	sess, err := env.sessions.Create(t.Context(), "goal-derived title")
	require.NoError(t, err)

	// Dispatch the goal exactly as thread.Manager.Create/TaskManager.Create
	// would once coordinator.run has copied PromptOriginFromContext(ctx)
	// onto the call.
	result, err := sa.Run(t.Context(), SessionAgentCall{
		SessionID:    sess.ID,
		Prompt:       "implement the widget",
		PromptOrigin: message.OriginAgent,
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	select {
	case <-model.titleGot:
	case <-time.After(10 * time.Second):
		t.Fatal("the detached title generation never reached the agent's model")
	}

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)

	require.Equal(t, 1, countRoleText(msgs, message.User, "implement the widget"),
		"the goal must be persisted as exactly one message.User message")
	require.Equal(t, 1, countOrigin(msgs, message.OriginAgent, "implement the widget"),
		"the goal must be persisted with Origin: message.OriginAgent")

	calls := model.calls()
	require.Len(t, calls, 1)
	require.True(t, promptHasUserText(calls[0], "implement the widget"),
		"the goal must reach the model on turn one as a user-role message")

	// A real, undecorated follow-up: no PromptOrigin, so it persists as
	// the ordinary message.User/OriginPerson message the user actually
	// typed.
	result, err = sa.Run(t.Context(), SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "also add tests",
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	msgs, err = env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)

	require.Equal(t, 1, countRoleText(msgs, message.User, "implement the widget"),
		"the goal's message.User message must stay singular across turns")
	require.Equal(t, 1, countOrigin(msgs, message.OriginAgent, "implement the widget"),
		"the goal must still be Origin: message.OriginAgent after a follow-up turn")
	require.True(t, containsRoleText(msgs, message.User, "also add tests"),
		"the genuine follow-up must be persisted as a message.User message")
	require.Equal(t, 0, countOrigin(msgs, message.OriginAgent, "also add tests"),
		"the genuine follow-up must be persisted as Origin: message.OriginPerson (the default)")

	calls = model.calls()
	require.Len(t, calls, 2)
	require.True(t, promptHasUserText(calls[1], "implement the widget"),
		"the goal must still reach the model on a follow-up turn, rebuilt from history, as a user-role message")
	require.True(t, promptHasUserText(calls[1], "also add tests"),
		"the follow-up must also reach the model on the same turn")

	// Regression check for hasUserTextMessage: title generation must run
	// exactly once. Role is uniformly message.User now, so this holds
	// trivially, but it is still worth asserting explicitly since a
	// second run would clobber the goal-derived title.
	require.Equal(t, 1, model.titleCalls(),
		"title generation must run exactly once; a goal dispatched with OriginAgent must still suppress a second run on follow-up")
}

// containsRoleText reports whether msgs contains a message with the given
// role whose text contains want.
func containsRoleText(msgs []message.Message, role message.MessageRole, want string) bool {
	return countRoleText(msgs, role, want) > 0
}

// countRoleText counts messages with the given role whose text contains
// want.
func countRoleText(msgs []message.Message, role message.MessageRole, want string) int {
	n := 0
	for _, msg := range msgs {
		if msg.Role != role {
			continue
		}
		for _, part := range msg.Parts {
			if tc, ok := part.(message.TextContent); ok && strings.Contains(tc.Text, want) {
				n++
				break
			}
		}
	}
	return n
}

// countOrigin counts messages with the given origin whose text contains
// want.
func countOrigin(msgs []message.Message, origin message.Origin, want string) int {
	n := 0
	for _, msg := range msgs {
		if msg.Origin != origin {
			continue
		}
		for _, part := range msg.Parts {
			if tc, ok := part.(message.TextContent); ok && strings.Contains(tc.Text, want) {
				n++
				break
			}
		}
	}
	return n
}
