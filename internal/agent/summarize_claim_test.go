package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/stretchr/testify/require"
)

// TestSummarize_ClaimHandsOffTheActiveSlotAtomically is the regression test
// for the idle window between finishTurn's clearActiveIfMatch and
// summarize's own busy check: finishTurn used to release its active-run
// slot and let summarize re-claim it from scratch, as two separately
// locked steps. A queued continuation could claim the session in that
// window, so a perfectly successful turn's summarize call would fail with
// ErrSessionBusy even though nothing was actually wrong. finishTurn now
// hands its still-installed *activeCancel to summarize via the claim
// parameter, which swaps the slot atomically instead of releasing and
// re-claiming.
//
//nolint:tparallel // the subtests share one sessionAgent and session; they must run in sequence.
func TestSummarize_ClaimHandsOffTheActiveSlotAtomically(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	sa := NewSessionAgent(SessionAgentOptions{
		Model:        Model{Model: &raceInjectModel{text: "summary"}, CatalogCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		SystemPrompt: "system",
		Sessions:     env.sessions,
		Messages:     env.messages,
	}).(*sessionAgent)

	t.Run("passing claim takes over the caller's own still-installed slot", func(t *testing.T) {
		// This is what finishTurn does now: the slot is never released
		// before summarize gets a chance to take it over, so there is no
		// window for anything else to claim the session in between.
		ac := &activeCancel{cancel: func() {}}
		sa.setActiveForTest(sess.ID, ac)
		err := sa.summarize(t.Context(), sess.ID, fantasy.ProviderOptions{}, nil, nil, sa.model.Get(), "", nil, ac)
		require.NoError(t, err, "summarize must succeed by swapping in its own slot, not by finding the session idle")
		require.False(t, sa.IsSessionBusy(sess.ID), "summarize must release the slot once it's done")
	})

	t.Run("the old release-then-reclaim sequence loses the slot to a racer", func(t *testing.T) {
		// Reproduces the bug directly: release the slot the way finishTurn
		// used to (clearActiveIfMatch), let a racer (standing in for a
		// queued continuation) claim the now-idle session, then call
		// summarize the old way (claim=nil, re-claiming from scratch). It
		// observes the racer and bails with ErrSessionBusy - the exact
		// symptom a successful turn's finishTurn used to hit.
		ac := &activeCancel{cancel: func() {}}
		sa.setActiveForTest(sess.ID, ac)
		sa.clearActiveIfMatch(sess.ID, ac)
		racer := &activeCancel{cancel: func() {}}
		sa.setActiveForTest(sess.ID, racer)

		err := sa.summarize(t.Context(), sess.ID, fantasy.ProviderOptions{}, nil, nil, sa.model.Get(), "", nil, nil)
		require.ErrorIs(t, err, ErrSessionBusy)

		sa.clearActiveIfMatch(sess.ID, racer)
	})

	t.Run("claim fails cleanly, as ErrSessionBusy, if the slot no longer matches", func(t *testing.T) {
		ac := &activeCancel{cancel: func() {}}
		sa.setActiveForTest(sess.ID, ac)
		other := &activeCancel{cancel: func() {}}
		sa.setActiveForTest(sess.ID, other) // something else already took over

		err := sa.summarize(t.Context(), sess.ID, fantasy.ProviderOptions{}, nil, nil, sa.model.Get(), "", nil, ac)
		require.ErrorIs(t, err, ErrSessionBusy)
		got, ok := sa.getActiveForTest(sess.ID)
		require.True(t, ok)
		require.Same(t, other, got, "a failed claim must not disturb whatever is actually installed")

		sa.clearActiveIfMatch(sess.ID, other)
	})
}

// TestSummarize_PassesRateLimitCallbackToTheProvider pins B4's other
// half. A summary is the single most expensive request a session makes
// and is only asked for once the context is full, so losing the whole
// pass to a 429 that a spare account would have absorbed is the worst
// place to skip rotation. The parent turn has rotated on this since
// rotation existed; the summary it triggers did not, and neither did a
// delegation's.
func TestSummarize_PassesRateLimitCallbackToTheProvider(t *testing.T) {
	env := testEnv(t)

	rotated := make(chan struct{}, 1)
	model := &rateLimitOnceModel{}
	sa := NewSessionAgent(SessionAgentOptions{
		Model:    Model{Model: model, CatalogCfg: catwalk.Model{ContextWindow: 100000, DefaultMaxTokens: 1000}},
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "hello"}},
	})
	require.NoError(t, err)

	onRateLimit := func(context.Context, *fantasy.ProviderError) error {
		select {
		case rotated <- struct{}{}:
		default:
		}
		return nil
	}
	_ = sa.summarize(t.Context(), sess.ID, fantasy.ProviderOptions{}, nil, onRateLimit,
		sa.model.Get(), "", nil, nil)

	select {
	case <-rotated:
	default:
		t.Fatal("a rate-limited summarize must reach the rotation callback")
	}
}

// rateLimitOnceModel fails its first stream with a 429 and succeeds on the
// retry, which is the shape fantasy's OnRateLimit contract is written for.
type rateLimitOnceModel struct {
	calls atomic.Int32
}

func (m *rateLimitOnceModel) Provider() string { return "fake" }
func (m *rateLimitOnceModel) Model() string    { return "fake-model" }

func (m *rateLimitOnceModel) Stream(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	if m.calls.Add(1) == 1 {
		return func(yield func(fantasy.StreamPart) bool) {
			yield(fantasy.StreamPart{
				Type:  fantasy.StreamPartTypeError,
				Error: &fantasy.ProviderError{StatusCode: 429, Message: "rate limited"},
			})
		}, nil
	}
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "1"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "1", Delta: "summary"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "1"}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func (m *rateLimitOnceModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not used")
}

func (m *rateLimitOnceModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not used")
}

func (m *rateLimitOnceModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not used")
}
