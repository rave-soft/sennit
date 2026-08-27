package agent

import (
	"context"
	"errors"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/message"
	messagestore "github.com/rave-soft/sennit/internal/message/store"
	"github.com/stretchr/testify/require"
)

// createFailingToolResultStore wraps a real message store and fails Create
// for tool-role messages only, simulating a DB error (busy store, shutdown
// race) while handleStreamError is inserting the synthetic tool result for
// an interrupted tool call. Every other call — including the Update that
// persists AddFinish — passes through untouched.
type createFailingToolResultStore struct {
	messagestore.Service
}

func (s *createFailingToolResultStore) Create(ctx context.Context, sessionID string, params message.CreateMessageParams) (message.Message, error) {
	if params.Role == message.Tool {
		return message.Message{}, errors.New("simulated store failure inserting synthetic tool result")
	}
	return s.Service.Create(ctx, sessionID, params)
}

// interruptedToolCallModel starts a tool call and then errors mid-stream
// without ever finishing it, the interrupted-stream shape handleStreamError
// exists to clean up.
type interruptedToolCallModel struct{}

func (m *interruptedToolCallModel) Provider() string { return "fake" }
func (m *interruptedToolCallModel) Model() string    { return "fake-model" }

func (m *interruptedToolCallModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not implemented")
}

func (m *interruptedToolCallModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if isTitleCall(call) {
		return titleStream()
	}
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputStart, ID: "call_1", ToolCallName: "read"}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: errors.New("boom mid-stream")})
	}, nil
}

func (m *interruptedToolCallModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *interruptedToolCallModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

// TestHandleStreamError_SyntheticResultCreateFailureStillPersistsFinish is
// the regression test for the createErr path in handleStreamError
// (turn.go): a DB failure creating the synthetic tool result for an
// interrupted tool call used to return early, skipping both AddFinish and
// the final messages.Update — leaving the assistant message with no finish
// reason, which the chat UI reads as "still running" forever. The failure
// must be logged and tolerated instead, exactly like its listErr and
// updateErr siblings in the same loop.
func TestHandleStreamError_SyntheticResultCreateFailureStillPersistsFinish(t *testing.T) {
	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	store := &createFailingToolResultStore{Service: env.messages}

	sa := NewSessionAgent(SessionAgentOptions{
		Model: Model{
			Model:      &interruptedToolCallModel{},
			CatalogCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000, Name: "fake-model"},
		},
		SystemPrompt: "system",
		Sessions:     env.sessions,
		Messages:     store,
	}).(*sessionAgent)

	_, runErr := sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "hi"})
	require.Error(t, runErr, "the mid-stream error must still surface to the caller")

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)

	var found bool
	for i := range msgs {
		if msgs[i].Role != message.Assistant {
			continue
		}
		found = true
		finish := msgs[i].FinishPart()
		require.NotNil(t, finish, "the assistant message must still get a finish reason even when the synthetic tool result fails to persist")
	}
	require.True(t, found, "an assistant message must have been persisted")
}
