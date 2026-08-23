package agent

import (
	"context"
	"errors"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/stretchr/testify/require"
)

// cancelDuringSummaryModel blocks its stream until the request context is
// canceled, then reports the cancellation as a stream error - standing in
// for a real provider request that gets interrupted mid-flight.
type cancelDuringSummaryModel struct {
	streamStarted chan struct{}
}

func (m *cancelDuringSummaryModel) Provider() string { return "fake" }
func (m *cancelDuringSummaryModel) Model() string    { return "fake-model" }

func (m *cancelDuringSummaryModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not implemented")
}

func (m *cancelDuringSummaryModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if isTitleCall(call) {
		return titleStream()
	}
	return func(yield func(fantasy.StreamPart) bool) {
		close(m.streamStarted)
		<-ctx.Done()
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ctx.Err()})
	}, nil
}

func (m *cancelDuringSummaryModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *cancelDuringSummaryModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

// TestSummarize_CanceledCleanupStillDeletesTheSummaryMessage is the
// regression test for summarize's cancel cleanup: on a canceled Stream, it
// deleted the placeholder summary message using the same ctx that just got
// canceled. A canceled context fails the delete outright (database/sql
// rejects a query on a done context before it ever reaches the driver), so
// the empty summary message was left behind forever, even though the
// returned error still satisfied errors.Is(err, context.Canceled) - the
// delete's own failure happened to be the same sentinel, which is exactly
// why asserting on the error alone would not catch this. The fix runs the
// cleanup delete on a context detached from ctx (context.WithoutCancel plus
// a bounded timeout), so it succeeds regardless of the cancellation that
// triggered it.
func TestSummarize_CanceledCleanupStillDeletesTheSummaryMessage(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	// summarize bails out early with nothing to do unless there is at
	// least one message to summarize.
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "hi"}},
	})
	require.NoError(t, err)

	streamStarted := make(chan struct{})
	model := &cancelDuringSummaryModel{streamStarted: streamStarted}
	sa := NewSessionAgent(SessionAgentOptions{
		Model:        Model{Model: model, CatalogCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		SystemPrompt: "system",
		Sessions:     env.sessions,
		Messages:     env.messages,
	}).(*sessionAgent)

	summarizeCtx, cancelSummarize := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- sa.Summarize(summarizeCtx, sess.ID, fantasy.ProviderOptions{}, nil) }()

	<-streamStarted
	cancelSummarize()
	// summarize reports the cancellation itself (see the isCancelErr
	// branch): returning the cleanup delete's error instead - nil
	// whenever cleanup worked - told finishTurn a canceled summarize had
	// succeeded, and it went on to continue the turn on the very context
	// that needed summarizing. The cleanup's own success is asserted
	// below, on whether the message actually got deleted.
	require.ErrorIs(t, <-errCh, context.Canceled)

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	for _, m := range msgs {
		require.False(t, m.IsSummaryMessage, "a canceled summarize must not leave an orphaned summary message behind")
	}
}
