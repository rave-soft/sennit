package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// fakeParentMessenger records SendToParent calls, or returns a configured
// error to simulate a session with no registered parent.
type fakeParentMessenger struct {
	err error

	called    bool
	sessionID string
	message   string
}

func (f *fakeParentMessenger) SendToParent(_ context.Context, sessionID, message string) error {
	f.called = true
	f.sessionID = sessionID
	f.message = message
	return f.err
}

func runAskParentTool(t *testing.T, ctx context.Context, messenger *fakeParentMessenger, message string) fantasy.ToolResponse {
	t.Helper()

	tool := NewAskParentTool(messenger)

	input, err := json.Marshal(AskParentParams{Message: message})
	require.NoError(t, err)

	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Input: string(input)})
	require.NoError(t, err)
	return resp
}

func TestAskParentTool_IsSequential(t *testing.T) {
	t.Parallel()

	require.False(t, NewAskParentTool(&fakeParentMessenger{}).Info().Parallel)
}

func TestAskParentTool_SendsMessageWithCallersSessionID(t *testing.T) {
	t.Parallel()

	messenger := &fakeParentMessenger{}
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "session-1")

	resp := runAskParentTool(t, ctx, messenger, "which API did you mean?")

	require.False(t, resp.IsError)
	require.True(t, messenger.called)
	require.Equal(t, "session-1", messenger.sessionID)
	require.Equal(t, "which API did you mean?", messenger.message)
}

func TestAskParentTool_MissingMessageIsRejected(t *testing.T) {
	t.Parallel()

	messenger := &fakeParentMessenger{}
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "session-1")

	resp := runAskParentTool(t, ctx, messenger, "")

	require.True(t, resp.IsError)
	require.False(t, messenger.called)
}

func TestAskParentTool_MissingSessionIDIsRejected(t *testing.T) {
	t.Parallel()

	messenger := &fakeParentMessenger{}
	tool := NewAskParentTool(messenger)

	input, err := json.Marshal(AskParentParams{Message: "anything"})
	require.NoError(t, err)

	// A missing session ID is not something the model's call can fix — it
	// is wired in by the caller — so this is a Go error, not a text
	// response the model would see as a retryable tool result.
	_, err = tool.Run(t.Context(), fantasy.ToolCall{ID: "call-1", Input: string(input)})
	require.Error(t, err)
	require.Contains(t, err.Error(), "session ID is required for asking the parent")
	require.False(t, messenger.called)
}

func TestAskParentTool_MessengerErrorSurfacesAsTextErrorResponse(t *testing.T) {
	t.Parallel()

	wantErr := errors.New(`session "session-1" has no registered parent to message`)
	messenger := &fakeParentMessenger{err: wantErr}
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "session-1")

	resp := runAskParentTool(t, ctx, messenger, "anything")

	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, wantErr.Error())
	require.True(t, messenger.called)
}
