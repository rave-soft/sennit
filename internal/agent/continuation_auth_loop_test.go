package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/message"
)

// unauthorizedModel fails every non-title request the way a provider does
// once its OAuth credential is dead.
type unauthorizedModel struct {
	calls atomic.Int32
}

func (m *unauthorizedModel) Provider() string { return "fake" }
func (m *unauthorizedModel) Model() string    { return "fake-model" }

func (m *unauthorizedModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not implemented")
}

func (m *unauthorizedModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if isTitleCall(call) {
		return titleStream()
	}
	m.calls.Add(1)
	return nil, &fantasy.ProviderError{
		Message:    "Provided authentication token is expired.",
		Title:      "unauthorized",
		StatusCode: 401,
	}
}

func (m *unauthorizedModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *unauthorizedModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

// TestContinuationStopsRetryingADeadCredential pins the loop breaker on the
// real wake path. A delegation that finished while the provider's token was
// dead left its parent waking a continuation every second and a half for as
// long as the process lived: each attempt drained the report, failed on 401,
// requeued it and woke the next one.
func TestContinuationStopsRetryingADeadCredential(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &unauthorizedModel{}
	sa := testSessionAgent(env, model, "system").(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "earlier answer"}},
	})
	require.NoError(t, err)
	sa.SetLiveSession(sess.ID)

	sa.DeliverTaskCompletion(t.Context(), sess.ID, testCompletion("report"))

	require.Eventually(t, func() bool { return int(model.calls.Load()) >= maxContinuationAttempts },
		continuationRetryBackoff(1)+continuationRetryBackoff(2)+5*time.Second, 10*time.Millisecond,
		"each attempt within the budget must reach the model")
	// Past the cap nothing wakes again. A fourth attempt would wait out
	// the backoff first, so wait that long before counting.
	require.Eventually(t, func() bool { return sa.continuationFailureCount(sess.ID) >= maxContinuationAttempts },
		2*time.Second, 10*time.Millisecond)
	time.Sleep(continuationRetryBackoff(maxContinuationAttempts) + 500*time.Millisecond)
	require.Equal(t, maxContinuationAttempts, int(model.calls.Load()),
		"a continuation failing on a dead credential must stop after the attempt budget")
	require.Equal(t, 1, completionInboxLen(sa, sess.ID), "the report stays queued for the next real turn")
}
