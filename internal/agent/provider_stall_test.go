package agent

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/stretchr/testify/require"
)

// stallBudgets shrinks an instrumented model's silence budgets to
// something a test can wait for. The production values are minutes; the
// behaviour under test is identical at milliseconds.
func stallBudgets(m *instrumentedModel, firstPart, gap time.Duration) *instrumentedModel {
	m.firstPartTimeout, m.stallTimeout = firstPart, gap
	return m
}

// silentModel is a fantasy.LanguageModel that behaves the way a wedged
// provider does: the stream is created, and then nothing is ever sent on
// it. Without a watchdog, ranging over it never returns.
type silentModel struct {
	fakeStreamModel
	// beforeSilence is yielded first, so a test can distinguish a stream
	// that never started from one that started and then stopped.
	beforeSilence []fantasy.StreamPart
}

func (s *silentModel) Stream(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	return func(yield func(fantasy.StreamPart) bool) {
		for _, part := range s.beforeSilence {
			if !yield(part) {
				return
			}
		}
		<-ctx.Done()
	}, nil
}

// TestProviderStall_FirstPart proves the case that produced this watchdog:
// a provider that accepts the request and then says nothing at all. The
// stream must end, and it must end as a stall rather than as a silent
// truncation.
func TestProviderStall_FirstPart(t *testing.T) {
	t.Parallel()

	corr := providerCorrelation{sessionID: "sess-stall", turnID: "turn-stall", attempt: 1, reason: reasonTurn}
	model := stallBudgets(newInstrumentedModel(&silentModel{}, corr, "openai"), 20*time.Millisecond, time.Hour)

	stream, err := model.Stream(t.Context(), fantasy.Call{})
	require.NoError(t, err)

	var streamErr error
	for part := range stream {
		if part.Type == fantasy.StreamPartTypeError {
			streamErr = part.Error
		}
	}
	require.Error(t, streamErr, "a stalled stream must surface an error, not end quietly")
	require.True(t, isProviderStall(streamErr), "the error must be the stall, got %v", streamErr)
}

// TestProviderStall_MidStream covers the other silence: a provider that
// starts producing and then stops. The parts already delivered must reach
// the consumer, and the hole after them must still end the attempt.
func TestProviderStall_MidStream(t *testing.T) {
	t.Parallel()

	corr := providerCorrelation{sessionID: "sess-mid", turnID: "turn-mid", attempt: 1, reason: reasonTurn}
	inner := &silentModel{beforeSilence: []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeTextDelta, Delta: "partial"},
	}}
	model := stallBudgets(newInstrumentedModel(inner, corr, "openai"), time.Hour, 20*time.Millisecond)

	stream, err := model.Stream(t.Context(), fantasy.Call{})
	require.NoError(t, err)

	var deltas int
	var streamErr error
	for part := range stream {
		switch part.Type {
		case fantasy.StreamPartTypeTextDelta:
			deltas++
		case fantasy.StreamPartTypeError:
			streamErr = part.Error
		}
	}
	require.Equal(t, 1, deltas, "parts delivered before the stall must still reach the consumer")
	require.True(t, isProviderStall(streamErr), "the hole after the last part must end the attempt, got %v", streamErr)
}

// TestProviderStall_Retryable is the property the whole design rests on:
// fantasy retries net.Errors and refuses to retry cancellations, so a
// stall must present as the former even though a cancellation is how it
// is broken. If this ever regresses, a stalled attempt fails its turn
// outright instead of being re-attempted.
func TestProviderStall_Retryable(t *testing.T) {
	t.Parallel()

	err := error(&providerStallError{phase: stallPhaseStream, limit: time.Minute})

	var netErr net.Error
	require.True(t, errors.As(err, &netErr), "a stall must satisfy net.Error to be retryable")
	require.True(t, netErr.Timeout(), "a stall must report itself as a timeout")
	require.False(t, errors.Is(err, context.Canceled), "a stall must not read as a cancellation")
}

// TestProviderStall_HealthyStreamNotTripped guards the expensive failure
// mode in the other direction: a watchdog that fires on a working stream
// would kill real work. A stream that keeps producing inside the gap
// budget must finish normally, however long it runs in total.
func TestProviderStall_HealthyStreamNotTripped(t *testing.T) {
	t.Parallel()

	corr := providerCorrelation{sessionID: "sess-ok", turnID: "turn-ok", attempt: 1, reason: reasonTurn}
	inner := &steadyModel{parts: 12, gap: 5 * time.Millisecond}
	// A gap budget many times the inter-part delay, but far below the
	// stream's total duration: this fails if the budget is ever treated
	// as a total rather than as a gap.
	model := stallBudgets(newInstrumentedModel(inner, corr, "openai"), time.Second, 200*time.Millisecond)

	stream, err := model.Stream(t.Context(), fantasy.Call{})
	require.NoError(t, err)

	var sawFinish bool
	var streamErr error
	for part := range stream {
		switch part.Type {
		case fantasy.StreamPartTypeFinish:
			sawFinish = true
		case fantasy.StreamPartTypeError:
			streamErr = part.Error
		}
	}
	require.NoError(t, streamErr, "a steadily producing stream must not be tripped")
	require.True(t, sawFinish, "a steadily producing stream must reach its terminal finish")
}

// steadyModel emits parts spaced by gap, the shape of a slow but healthy
// provider.
type steadyModel struct {
	fakeStreamModel
	parts int
	gap   time.Duration
}

func (s *steadyModel) Stream(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	return func(yield func(fantasy.StreamPart) bool) {
		for i := 0; i < s.parts; i++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(s.gap):
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "."}) {
				return
			}
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

// TestProviderStall_LogsStalledOutcome proves the finished line names the
// stall rather than the cancellation that broke it. The log is how this
// is diagnosed after the fact, and "canceled" there would point at the
// user.
func TestProviderStall_LogsStalledOutcome(t *testing.T) {
	t.Parallel()
	logs := captureJSONLogs(t)

	corr := providerCorrelation{sessionID: "sess-log", turnID: "turn-log", attempt: 1, reason: reasonTurn}
	model := stallBudgets(newInstrumentedModel(&silentModel{}, corr, "openai"), 20*time.Millisecond, time.Hour)

	stream, err := model.Stream(t.Context(), fantasy.Call{})
	require.NoError(t, err)
	consumeStream(stream)

	finished := allProviderLogLines(t, logs, "Provider request finished", "sess-log")
	require.Len(t, finished, 1)
	require.Equal(t, outcomeStalled, finished[0]["outcome"])
}

// TestStreamStallWatchdog_StopDisarms proves stop is what it claims: once
// the attempt is over, a budget that would otherwise have expired must
// not record a stall against a stream that already finished.
func TestStreamStallWatchdog_StopDisarms(t *testing.T) {
	t.Parallel()

	_, w := newStreamStallWatchdog(t.Context(), 20*time.Millisecond, 20*time.Millisecond)
	w.stop()
	time.Sleep(60 * time.Millisecond)
	require.Nil(t, w.stall(), "a stopped watchdog must not trip afterwards")
	w.stop() // idempotent
}
