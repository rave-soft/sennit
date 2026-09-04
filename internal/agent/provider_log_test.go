package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/stretchr/testify/require"
)

// findProviderLogLine returns the first captured JSON log line with the given
// message and session_id, or nil. The provider request lines normalize on
// session_id (not the historical "session"), so they need their own matcher
// rather than reusing the latency test's.
func findProviderLogLine(t *testing.T, buf *syncLogBuffer, msg, sessionID string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var decoded map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &decoded))
		if decoded["msg"] == msg && decoded["session_id"] == sessionID {
			return decoded
		}
	}
	return nil
}

// allProviderLogLines returns every captured JSON log line with the given
// message and session_id, in capture order. The provider request tests need
// the *count* of lines (the acceptance criterion is "exact number of provider
// attempts per run_id"), not just the first, so a single-line finder is not
// enough here.
func allProviderLogLines(t *testing.T, buf *syncLogBuffer, msg, sessionID string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var decoded map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &decoded))
		if decoded["msg"] == msg && decoded["session_id"] == sessionID {
			out = append(out, decoded)
		}
	}
	return out
}

// slogLevel returns the level a JSON log line was logged at. The JSON handler
// records it under "level" as a string ("INFO", "DEBUG", "WARN", "ERROR");
// slog has no string parser, so this maps the four levels by hand and fails
// loudly on anything else. Comparing against slog's own constants is what makes
// a refactor that quietly bumps a line from Debug to Info fail.
func slogLevel(t *testing.T, line map[string]any) slog.Level {
	t.Helper()
	raw, ok := line["level"].(string)
	require.True(t, ok, "log line missing a level field: %v", line)
	switch strings.ToUpper(raw) {
	case "DEBUG":
		return slog.LevelDebug
	case "INFO":
		return slog.LevelInfo
	case "WARN":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		t.Fatalf("unknown log level %q", raw)
		return slog.LevelInfo
	}
}

// fieldKeys returns the key names from a slog key/value field slice, so a test
// can assert on which fields are present without depending on their order or
// values.
func fieldKeys(fields []any) []string {
	keys := make([]string, 0, len(fields)/2)
	for i := 0; i+1 < len(fields); i += 2 {
		if key, ok := fields[i].(string); ok {
			keys = append(keys, key)
		}
	}
	return keys
}

// TestProviderRetryReason pins the closed mapping a retry_reason log field
// comes from: it is the filterable label the plan asks to be distinguishable
// (auth / rate_limited / server_error / ...), and it must be stable for the
// same status code so a filter on it is meaningful.
func TestProviderRetryReason(t *testing.T) {
	cases := []struct {
		name string
		err  *fantasy.ProviderError
		want string
	}{
		{"nil error yields no reason", nil, ""},
		{"401 is an auth failure", &fantasy.ProviderError{StatusCode: 401}, "auth"},
		{"403 is an auth failure", &fantasy.ProviderError{StatusCode: 403}, "auth"},
		{"429 is rate limiting", &fantasy.ProviderError{StatusCode: 429}, "rate_limited"},
		{"500 is a server error", &fantasy.ProviderError{StatusCode: 500}, "server_error"},
		{"503 is a server error", &fantasy.ProviderError{StatusCode: 503}, "server_error"},
		{"400 is a client error", &fantasy.ProviderError{StatusCode: 400}, "client_400"},
		{"404 is a client error", &fantasy.ProviderError{StatusCode: 404}, "client_404"},
		{"a non-HTTP failure is a generic provider error", &fantasy.ProviderError{StatusCode: 0}, "provider_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, providerRetryReason(tc.err))
		})
	}
}

// TestErrorCategory pins the closed, safe error_category mapping: it must name
// the *class* of failure from the error's type alone (never its message) and
// map the categories the provider can actually produce. A nil error yields ""
// (no field), and a canceled context is its own category.
func TestErrorCategory(t *testing.T) {
	t.Run("nil error yields no category", func(t *testing.T) {
		require.Equal(t, "", errorCategory(nil))
	})
	t.Run("canceled context is canceled", func(t *testing.T) {
		require.Equal(t, "canceled", errorCategory(context.Canceled))
	})
	t.Run("deadline exceeded is timeout", func(t *testing.T) {
		require.Equal(t, "timeout", errorCategory(context.DeadlineExceeded))
	})
	t.Run("5xx provider error is http_5xx", func(t *testing.T) {
		require.Equal(t, "http_5xx", errorCategory(&fantasy.ProviderError{StatusCode: 503}))
	})
	t.Run("4xx provider error is http_4xx", func(t *testing.T) {
		require.Equal(t, "http_4xx", errorCategory(&fantasy.ProviderError{StatusCode: 429}))
	})
	t.Run("a provider error with no status is http_other when status>0 is false -> provider", func(t *testing.T) {
		// A ProviderError with StatusCode 0 is not an HTTP status at all; it
		// is a provider-reported error, so it classifies as provider, not
		// http_other.
		require.Equal(t, "provider", errorCategory(&fantasy.ProviderError{StatusCode: 0}))
	})
	t.Run("a fantasy.Error is provider", func(t *testing.T) {
		require.Equal(t, "provider", errorCategory(&fantasy.Error{Message: "boom"}))
	})
	t.Run("an unknown error is unknown", func(t *testing.T) {
		require.Equal(t, "unknown", errorCategory(errors.New("something else")))
	})
}

// TestProviderUsageLogFields pins that usage is only logged when the provider
// actually reported it (a zero is omitted, never logged as a misleading 0),
// and that cache read vs creation stay separate so a cache hit (reads) is
// tellable from a cache miss (writes).
func TestProviderUsageLogFields(t *testing.T) {
	t.Run("an all-zero usage logs nothing", func(t *testing.T) {
		require.Empty(t, providerUsageLogFields(fantasy.Usage{}))
	})

	t.Run("a cache hit logs cache_read but not cache_creation", func(t *testing.T) {
		fields := providerUsageLogFields(fantasy.Usage{
			InputTokens:     100,
			OutputTokens:    10,
			CacheReadTokens: 5000,
		})
		joined := strings.Join(fieldKeys(fields), " ")
		require.Contains(t, joined, "cache_read_tokens")
		require.NotContains(t, joined, "cache_creation_tokens",
			"a cache hit must not also report cache creation")
	})

	t.Run("a cache miss logs cache_creation but not cache_read", func(t *testing.T) {
		fields := providerUsageLogFields(fantasy.Usage{
			InputTokens:         100,
			CacheCreationTokens: 5000,
		})
		joined := strings.Join(fieldKeys(fields), " ")
		require.Contains(t, joined, "cache_creation_tokens")
		require.NotContains(t, joined, "cache_read_tokens",
			"a cache miss must not also report a cache read")
	})
}

// TestProviderRequestLogFields pins the correlation block every provider
// request line shares: the exact field names the plan's filters depend on
// (session_id, run_id, turn_id, step, attempt, request_reason) and their
// order, so a rename in one site cannot silently break the others.
func TestProviderRequestLogFields(t *testing.T) {
	fields := providerRequestLogFields("sess-1", "run-1", "turn-1", 2, 3, reasonRetry)
	require.Equal(t, []any{
		"session_id", "sess-1",
		"run_id", "run-1",
		"turn_id", "turn-1",
		"step", 2,
		"attempt", 3,
		"request_reason", reasonRetry,
	}, fields)
}

// streamScript describes the parts a fake model's Stream will yield, or the
// error its Stream call itself returns (before any part is yielded). A script
// entry of type finish carries the finish reason + usage; an entry of type
// error carries the error that makes the consumer stop (and, in the real
// flow, triggers a retry).
type streamScript struct {
	// createErr, when non-nil, is returned by Stream() itself (the request
	// never became a stream).
	createErr error
	// parts, when createErr is nil, are the StreamParts to yield in order.
	parts []fantasy.StreamPart
	// block, when set, makes Stream block on the context until it is done
	// (used to test cancellation of an in-flight stream).
	block bool
}

// fakeStreamModel is a fantasy.LanguageModel whose Stream behavior is fully
// scripted. It stands in for the concrete model the instrumentedModel wraps,
// so the tests exercise the instrumentedModel's started/finished pairing
// without a real provider.
type fakeStreamModel struct {
	script streamScript
}

func (f *fakeStreamModel) Stream(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	if f.script.createErr != nil {
		return nil, f.script.createErr
	}
	return func(yield func(fantasy.StreamPart) bool) {
		if f.script.block {
			// Block until the context is done so a cancellation is observable
			// as a stopped stream rather than an immediate exhaustion.
			<-ctx.Done()
			return
		}
		for _, part := range f.script.parts {
			if !yield(part) {
				return
			}
		}
	}, nil
}

func (f *fakeStreamModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not used")
}

func (f *fakeStreamModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not used")
}

func (f *fakeStreamModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not used")
}
func (f *fakeStreamModel) Provider() string { return "fake" }
func (f *fakeStreamModel) Model() string    { return "fake-model" }

// consumeStream drains a StreamResponse fully, the way fantasy's
// processStepStream ranges over it.
func consumeStream(stream fantasy.StreamResponse) {
	for range stream {
	}
}

// TestInstrumentedModel_Success drives a real Stream through the instrumented
// model and proves the happy path: exactly one "Provider request started" and
// one "Provider request finished" line, both at Info, sharing the full
// correlation block, the finished line carrying outcome=success plus the
// finish reason and usage the stream reported, and a latency.
func TestInstrumentedModel_Success(t *testing.T) {
	t.Parallel()
	logs := captureJSONLogs(t)

	corr := providerCorrelation{sessionID: "sess-ok", runID: "run-ok", turnID: "turn-ok", step: 0, attempt: 1, reason: reasonTurn}
	inner := &fakeStreamModel{script: streamScript{parts: []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeTextDelta, Delta: "hello"},
		{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop, Usage: fantasy.Usage{InputTokens: 42, OutputTokens: 7, CacheReadTokens: 15}},
	}}}
	model := newInstrumentedModel(inner, corr, "openai")

	stream, err := model.Stream(t.Context(), fantasy.Call{})
	require.NoError(t, err)
	consumeStream(stream)

	started := allProviderLogLines(t, logs, "Provider request started", "sess-ok")
	finished := allProviderLogLines(t, logs, "Provider request finished", "sess-ok")
	require.Len(t, started, 1, "a single Stream call must log exactly one started line")
	require.Len(t, finished, 1, "a single Stream call must log exactly one finished line")

	s, f := started[0], finished[0]
	// Same correlation block on both halves of the pair.
	for _, key := range []string{"session_id", "run_id", "turn_id", "request_reason"} {
		require.Equal(t, s[key], f[key], "started and finished must agree on %s", key)
	}
	require.Equal(t, "run-ok", s["run_id"])
	require.Equal(t, "turn-ok", s["turn_id"])
	require.Equal(t, reasonTurn, s["request_reason"])
	require.EqualValues(t, 1, s["attempt"])
	// Levels: the request pair is Info.
	require.Equal(t, slog.LevelInfo, slogLevel(t, s))
	require.Equal(t, slog.LevelInfo, slogLevel(t, f))
	// Finished line carries the success outcome, the finish reason, usage and
	// a latency.
	require.Equal(t, outcomeSuccess, f["outcome"])
	require.Equal(t, string(fantasy.FinishReasonStop), f["finish_reason"])
	require.EqualValues(t, 42, f["input_tokens"])
	require.EqualValues(t, 7, f["output_tokens"])
	require.EqualValues(t, 15, f["cache_read_tokens"])
	_, hasLatency := f["latency"]
	require.True(t, hasLatency, "the finished line must carry a latency")
}

// TestInstrumentedModel_StreamCreationError proves the failure path where the
// Stream call itself errors (the request never became a stream): still exactly
// one started and one finished pair, the finished line carrying
// outcome=error and a safe error_category derived from the error's type.
func TestInstrumentedModel_StreamCreationError(t *testing.T) {
	t.Parallel()
	logs := captureJSONLogs(t)

	corr := providerCorrelation{sessionID: "sess-err", runID: "run-err", turnID: "turn-err", step: 0, attempt: 1, reason: reasonTurn}
	inner := &fakeStreamModel{script: streamScript{createErr: &fantasy.ProviderError{StatusCode: 503, Message: "unavailable"}}}
	model := newInstrumentedModel(inner, corr, "openai")

	_, err := model.Stream(t.Context(), fantasy.Call{})
	require.Error(t, err, "the creation error must propagate to the caller (fantasy retry)")

	started := allProviderLogLines(t, logs, "Provider request started", "sess-err")
	finished := allProviderLogLines(t, logs, "Provider request finished", "sess-err")
	require.Len(t, started, 1)
	require.Len(t, finished, 1)
	require.Equal(t, outcomeError, finished[0]["outcome"])
	require.Equal(t, "http_5xx", finished[0]["error_category"])
	// The error message must not leak into the log: only the category is
	// present, not the text.
	for _, line := range append(started, finished[0]) {
		require.NotContains(t, mustMarshal(t, line), "unavailable",
			"the error message must not be logged, only its category")
	}
}

// TestInstrumentedModel_StreamErrorPart proves the failure path where the
// stream is created but yields an error part (a mid-stream provider failure):
// one pair, finished with outcome=error and a category, and the error part is
// still passed through to the consumer (the tee is transparent).
func TestInstrumentedModel_StreamErrorPart(t *testing.T) {
	t.Parallel()
	logs := captureJSONLogs(t)

	corr := providerCorrelation{sessionID: "sess-mid", runID: "run-mid", turnID: "turn-mid", step: 1, attempt: 2, reason: reasonRetry}
	sentinel := &fantasy.ProviderError{StatusCode: 429, Message: "slow down"}
	inner := &fakeStreamModel{script: streamScript{parts: []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeTextDelta, Delta: "partial"},
		{Type: fantasy.StreamPartTypeError, Error: sentinel},
	}}}
	model := newInstrumentedModel(inner, corr, "openai")

	stream, err := model.Stream(t.Context(), fantasy.Call{})
	require.NoError(t, err, "an error part does not fail the Stream call itself")

	// The consumer must still see every part, including the error part, so
	// fantasy's processStepStream behaves as if it were the raw stream.
	var sawError bool
	for part := range stream {
		if part.Type == fantasy.StreamPartTypeError {
			sawError = errors.Is(part.Error, sentinel)
		}
	}
	require.True(t, sawError, "the error part must be teed through to the consumer unchanged")

	finished := allProviderLogLines(t, logs, "Provider request finished", "sess-mid")
	require.Len(t, finished, 1)
	require.Equal(t, outcomeError, finished[0]["outcome"])
	require.Equal(t, "http_4xx", finished[0]["error_category"])
	// The retry-labeled correlation is preserved on the pair.
	require.Equal(t, reasonRetry, finished[0]["request_reason"])
	require.EqualValues(t, 2, finished[0]["attempt"])
}

// TestInstrumentedModel_Cancel proves an in-flight stream that is abandoned by
// a canceled context logs a canceled outcome (not success, not error), with no
// error_category (cancellation is the outcome itself).
// TestInstrumentedModel_StreamCanceledErrorPart verifies both direct and
// wrapped cancellations yielded as error parts. The caller context stays active
// to ensure classification is based on the error part itself.
func TestInstrumentedModel_StreamCanceledErrorPart(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "direct", err: context.Canceled},
		{name: "wrapped", err: fmt.Errorf("stream stopped: %w", context.Canceled)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			logs := captureJSONLogs(t)
			sessionID := "sess-mid-canceled-" + tc.name
			corr := providerCorrelation{sessionID: sessionID, runID: "run-mid-canceled", turnID: "turn-mid-canceled", step: 0, attempt: 1, reason: reasonTurn}
			inner := &fakeStreamModel{script: streamScript{parts: []fantasy.StreamPart{{
				Type:  fantasy.StreamPartTypeError,
				Error: tc.err,
			}}}}

			stream, err := newInstrumentedModel(inner, corr, "openai").Stream(t.Context(), fantasy.Call{})
			require.NoError(t, err)
			consumeStream(stream)

			finished := allProviderLogLines(t, logs, "Provider request finished", sessionID)
			require.Len(t, finished, 1)
			require.Equal(t, outcomeCanceled, finished[0]["outcome"])
			_, hasCategory := finished[0]["error_category"]
			require.False(t, hasCategory, "a canceled error part has no error_category")
		})
	}
}

func TestInstrumentedModel_Cancel(t *testing.T) {
	t.Parallel()
	logs := captureJSONLogs(t)

	ctx, cancel := context.WithCancel(t.Context())
	corr := providerCorrelation{sessionID: "sess-cxl", runID: "run-cxl", turnID: "turn-cxl", step: 0, attempt: 1, reason: reasonTurn}
	inner := &fakeStreamModel{script: streamScript{block: true}}
	model := newInstrumentedModel(inner, corr, "openai")

	stream, err := model.Stream(ctx, fantasy.Call{})
	require.NoError(t, err)

	// Consume in a goroutine (the blocking stream never yields), then cancel
	// so the consumer's ranging stops and the deferred finished line fires.
	done := make(chan struct{})
	go func() {
		defer close(done)
		consumeStream(stream)
	}()
	// Give the goroutine a moment to reach the block, then cancel.
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the canceled stream to drain")
	}

	finished := allProviderLogLines(t, logs, "Provider request finished", "sess-cxl")
	require.Len(t, finished, 1)
	require.Equal(t, outcomeCanceled, finished[0]["outcome"])
	_, hasCategory := finished[0]["error_category"]
	require.False(t, hasCategory, "a canceled attempt has no error_category, only the outcome")
}

// TestInstrumentedModel_StreamCreationCanceled proves the first half of the
// "canceled is an outcome, not an error" rule: when the Stream call itself
// fails and the context is already done (the transport returns the context
// error on abort), the finished line carries outcome=canceled and no
// error_category - not outcome=error with a category that would imply a
// provider fault. The returned error is still the context error so the caller
// (fantasy's retry loop) sees it.
func TestInstrumentedModel_StreamCreationCanceled(t *testing.T) {
	t.Parallel()
	logs := captureJSONLogs(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // already done before Stream is called

	corr := providerCorrelation{sessionID: "sess-cxl2", runID: "run-cxl2", turnID: "turn-cxl2", step: 0, attempt: 1, reason: reasonTurn}
	// A model whose Stream returns the context's error (the way a transport
	// aborts a request that was canceled before it left the process).
	inner := &fakeStreamModel{script: streamScript{createErr: ctx.Err()}}
	model := newInstrumentedModel(inner, corr, "openai")

	_, err := model.Stream(ctx, fantasy.Call{})
	require.Error(t, err, "the creation error must propagate to the caller")
	require.ErrorIs(t, err, context.Canceled, "the returned error is the context error")

	finished := allProviderLogLines(t, logs, "Provider request finished", "sess-cxl2")
	require.Len(t, finished, 1)
	require.Equal(t, outcomeCanceled, finished[0]["outcome"],
		"a stream that failed to be created because the context is done is canceled, not an error")
	_, hasCategory := finished[0]["error_category"]
	require.False(t, hasCategory, "a canceled stream-creation has no error_category")
}

// TestInstrumentedModel_StreamCreationWrappedCanceled verifies that a transport
// can return a wrapped context.Canceled while the caller context remains
// active. The error itself is still cancellation, so it must not be classified
// as a provider error.
func TestInstrumentedModel_StreamCreationWrappedCanceled(t *testing.T) {
	t.Parallel()
	logs := captureJSONLogs(t)

	corr := providerCorrelation{sessionID: "sess-cxl3", runID: "run-cxl3", turnID: "turn-cxl3", step: 0, attempt: 1, reason: reasonTurn}
	inner := &fakeStreamModel{script: streamScript{createErr: fmt.Errorf("transport stopped: %w", context.Canceled)}}
	model := newInstrumentedModel(inner, corr, "openai")

	_, err := model.Stream(t.Context(), fantasy.Call{})
	require.ErrorIs(t, err, context.Canceled)

	finished := allProviderLogLines(t, logs, "Provider request finished", "sess-cxl3")
	require.Len(t, finished, 1)
	require.Equal(t, outcomeCanceled, finished[0]["outcome"])
	_, hasCategory := finished[0]["error_category"]
	require.False(t, hasCategory, "wrapped cancellation has no error_category")
}

// TestInstrumentedModel_AbortedWithoutFinish proves the "no terminal finish is
// not a success" rule: a stream that ends (is exhausted) without ever yielding
// a terminal finish part is logged as outcome=aborted, not success. This is
// the fix for a class of false successes where an interrupted or short stream
// was counted as a completed response.
func TestInstrumentedModel_AbortedWithoutFinish(t *testing.T) {
	t.Parallel()
	logs := captureJSONLogs(t)

	corr := providerCorrelation{sessionID: "sess-abort", runID: "run-abort", turnID: "turn-abort", step: 0, attempt: 1, reason: reasonTurn}
	// The stream yields text deltas and then ends - no terminal finish part.
	inner := &fakeStreamModel{script: streamScript{parts: []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeTextDelta, Delta: "partial"},
	}}}
	model := newInstrumentedModel(inner, corr, "openai")

	stream, err := model.Stream(t.Context(), fantasy.Call{})
	require.NoError(t, err)
	consumeStream(stream)

	finished := allProviderLogLines(t, logs, "Provider request finished", "sess-abort")
	require.Len(t, finished, 1)
	require.Equal(t, outcomeAborted, finished[0]["outcome"],
		"a stream that ended without a terminal finish part is aborted, not a success")
	_, hasCategory := finished[0]["error_category"]
	require.False(t, hasCategory, "an aborted (not errored) attempt has no error_category")
	_, hasFinishReason := finished[0]["finish_reason"]
	require.False(t, hasFinishReason, "an aborted attempt never reached a finish part, so no finish_reason")
}

// TestInstrumentedModel_ConsumerStopsBeforeFinish proves the other half of the
// "no terminal finish is not a success" rule: the consumer (fantasy's
// processStepStream) stops ranging before the terminal finish part is reached
// (it returns early). The instrumented wrapper must log outcome=aborted, not
// success, because it never observed a terminal finish.
func TestInstrumentedModel_ConsumerStopsBeforeFinish(t *testing.T) {
	t.Parallel()
	logs := captureJSONLogs(t)

	corr := providerCorrelation{sessionID: "sess-stop", runID: "run-stop", turnID: "turn-stop", step: 0, attempt: 1, reason: reasonTurn}
	// The stream would yield a text delta and then a terminal finish, but the
	// consumer stops after the delta (yield returns false) so the finish is
	// never observed.
	inner := &fakeStreamModel{script: streamScript{parts: []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeTextDelta, Delta: "partial"},
		{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop},
	}}}
	model := newInstrumentedModel(inner, corr, "openai")

	stream, err := model.Stream(t.Context(), fantasy.Call{})
	require.NoError(t, err)

	// Consume exactly one part, then stop ranging (the consumer abandons the
	// stream before the terminal finish).
	for range stream {
		break
	}

	finished := allProviderLogLines(t, logs, "Provider request finished", "sess-stop")
	require.Len(t, finished, 1)
	require.Equal(t, outcomeAborted, finished[0]["outcome"],
		"stopping before the terminal finish is an aborted attempt, not a success")
}

// TestAttemptOutcome pins the finished-line outcome mapping the wrapper uses
// from what it actually observed on a stream attempt: an explicit error part
// is an error (with its category), a terminal finish is a success, a canceled
// context without a finish is canceled (no category), and anything else (the
// stream ended short or the consumer stopped early, no error, no finish) is
// aborted. The mapping is pure so it is unit-testable without a log capture.
func TestAttemptOutcome(t *testing.T) {
	// A real instrumented model just to reach the method; inner is never used
	// by the pure mapping.
	m := &instrumentedModel{inner: &fakeStreamModel{}}

	// A context that is already done (canceled) for the canceled cases.
	canceledCtx, cancel := context.WithCancel(t.Context())
	cancel()
	defer func() { _ = canceledCtx.Err() }()

	cases := []struct {
		name         string
		ctx          context.Context
		sawFinish    bool
		streamErr    error
		wantOutcome  string
		wantCategory string
	}{
		{"a direct canceled error part is canceled", t.Context(), false, context.Canceled, outcomeCanceled, ""},
		{"a wrapped canceled error part is canceled", t.Context(), false, fmt.Errorf("stream stopped: %w", context.Canceled), outcomeCanceled, ""},
		{"a provider error part wins over everything", t.Context(), false, &fantasy.ProviderError{StatusCode: 500}, outcomeError, "http_5xx"},
		{"a non-cancellation error part is an error even after a finish (defensive)", t.Context(), true, errors.New("late"), outcomeError, "unknown"},
		{"a terminal finish is a success", t.Context(), true, nil, outcomeSuccess, ""},
		{"a canceled context without a finish is canceled", canceledCtx, false, nil, outcomeCanceled, ""},
		{"a canceled context is canceled even with a consumer stop", canceledCtx, false, nil, outcomeCanceled, ""},
		{"a stream that ended short (no error, no finish) is aborted", t.Context(), false, nil, outcomeAborted, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outcome, category := m.attemptOutcome(tc.ctx, tc.sawFinish, tc.streamErr)
			require.Equal(t, tc.wantOutcome, outcome)
			require.Equal(t, tc.wantCategory, category)
		})
	}
}

// TestInstrumentedModel_ExactPairCount proves the 1:1 invariant across several
// attempts: N Stream calls produce exactly N started and N finished lines, and
// each started/finished pair shares the same attempt number and turn id (so a
// run_id's log can be counted and correlated without hunting). This is the
// core fix for the earlier orphaned-started / duplicate-finished problem.
func TestInstrumentedModel_ExactPairCount(t *testing.T) {
	t.Parallel()
	logs := captureJSONLogs(t)

	const attempts = 3
	const sessionID = "sess-count"
	corr := providerCorrelation{sessionID: sessionID, runID: "run-count", turnID: "turn-count", step: 0}
	// First two attempts fail, third succeeds - mirroring a retry-then-success.
	scripts := []streamScript{
		{createErr: &fantasy.ProviderError{StatusCode: 500}},
		{parts: []fantasy.StreamPart{{Type: fantasy.StreamPartTypeError, Error: &fantasy.ProviderError{StatusCode: 500}}}},
		{parts: []fantasy.StreamPart{{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop}}},
	}
	outcomes := []string{outcomeError, outcomeError, outcomeSuccess}

	for i, script := range scripts {
		corr.attempt = i + 1
		if i == 0 || i == 1 {
			corr.reason = reasonRetry
		} else {
			corr.reason = reasonTurn
		}
		model := newInstrumentedModel(&fakeStreamModel{script: script}, corr, "openai")
		stream, err := model.Stream(t.Context(), fantasy.Call{})
		if err != nil {
			require.Equal(t, outcomeError, outcomes[i])
		} else {
			consumeStream(stream)
		}
	}

	started := allProviderLogLines(t, logs, "Provider request started", sessionID)
	finished := allProviderLogLines(t, logs, "Provider request finished", sessionID)
	require.Len(t, started, attempts, "one started line per Stream attempt")
	require.Len(t, finished, attempts, "one finished line per Stream attempt, including failures")
	require.Equal(t, len(started), len(finished), "started and finished must be a 1:1 count")

	// Each pair shares its attempt number and turn id.
	for i := range attempts {
		require.EqualValues(t, i+1, started[i]["attempt"])
		require.EqualValues(t, i+1, finished[i]["attempt"])
		require.Equal(t, "turn-count", started[i]["turn_id"])
		require.Equal(t, started[i]["turn_id"], finished[i]["turn_id"], "the pair must share its turn id")
		require.Equal(t, outcomes[i], finished[i]["outcome"])
	}
}

// newTurnForLogTest builds a runTurn wired just enough to call modelProvider
// and onRetry (the two points that drive the attempt counter and the retry
// label): a real message service (so onRetry's reset update lands) and a real
// assistant message (so ResetStreamedContent has a target). It does not run a
// full turn - the retry tests below orchestrate the attempts directly.
func newTurnForLogTest(t *testing.T, runID, turnID string) (*runTurn, string) {
	t.Helper()
	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "log-turn")
	require.NoError(t, err)
	agent := &sessionAgent{messages: env.messages}
	assistantMsg, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:     message.Assistant,
		Parts:    []message.ContentPart{},
		Model:    "fake-model",
		Provider: "fake",
	})
	require.NoError(t, err)
	return &runTurn{
		agent:            agent,
		turnID:           turnID,
		call:             SessionAgentCall{SessionID: sess.ID, RunID: runID},
		ctx:              t.Context(),
		genCtx:           t.Context(),
		currentAssistant: &assistantMsg,
		stepNumber:       0,
		attempt:          0,
	}, sess.ID
}

// simulateRetryAttempt runs one attempt the way fantasy's retry loop does:
// ask modelProvider for the (instrumented) model, then call its Stream. It
// returns the model so the caller can decide (based on the attempt's error)
// whether to retry. This reproduces the exact sequence fantasy performs -
// ModelProvider then Stream inside the retry fn - without fantasy's 5s backoff
// sleep, so the retry flow is testable deterministically.
func simulateRetryAttempt(t *testing.T, rt *runTurn) (fantasy.StreamResponse, error) {
	t.Helper()
	langModel := rt.modelProvider()
	stream, err := langModel.Stream(t.Context(), fantasy.Call{})
	return stream, err
}

// TestProviderRequest_RetrySucceedsOnSecond drives the turn's real modelProvider
// and onRetry through a failed-then-successful pair of attempts (exactly the
// sequence fantasy's retry loop runs) and proves the log stream is a clean,
// countable sequence: started(turn, attempt 1) -> finished(error) -> retry
// warning -> started(retry, attempt 2) -> finished(success), with the retry
// warning tied to the attempt it interrupts and the second attempt labeled
// retry.
func TestProviderRequest_RetrySucceedsOnSecond(t *testing.T) {
	t.Parallel()
	logs := captureJSONLogs(t)

	rt, sessionID := newTurnForLogTest(t, "run-r", "turn-r")
	// The turn's model is scripted to fail on the first Stream and succeed on
	// the second; modelProvider wraps whichever the turn's model holds, so we
	// swap it between attempts the way an auth refresh would.
	failModel := &fakeStreamModel{script: streamScript{createErr: &fantasy.ProviderError{StatusCode: 503, Title: "unavailable"}}}
	okModel := &fakeStreamModel{script: streamScript{parts: []fantasy.StreamPart{{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop, Usage: fantasy.Usage{OutputTokens: 3}}}}}
	rt.model.Model = failModel

	// Attempt 1: turn reason, fails.
	rt.attempt = 0
	stream, err := simulateRetryAttempt(t, rt)
	require.Error(t, err, "attempt 1 must fail")
	_ = stream
	// fantasy would now call OnRetry: record the failure reason and warn.
	rt.onRetry(&fantasy.ProviderError{StatusCode: 503, Title: "unavailable", Message: "try later"}, time.Millisecond)
	// Swap in the recovered model, then attempt 2: retry reason, succeeds.
	rt.model.Model = okModel
	stream, err = simulateRetryAttempt(t, rt)
	require.NoError(t, err)
	consumeStream(stream)

	started := allProviderLogLines(t, logs, "Provider request started", sessionID)
	finished := allProviderLogLines(t, logs, "Provider request finished", sessionID)
	require.Len(t, started, 2, "two attempts = two started lines")
	require.Len(t, finished, 2, "two attempts = two finished lines, including the failed one")

	// The attempts are labeled and ordered: first is the turn's baseline, the
	// second is the retry.
	require.Equal(t, reasonTurn, started[0]["request_reason"])
	require.Equal(t, reasonRetry, started[1]["request_reason"])
	require.EqualValues(t, 1, started[0]["attempt"])
	require.EqualValues(t, 2, started[1]["attempt"])
	require.Equal(t, outcomeError, finished[0]["outcome"])
	require.Equal(t, outcomeSuccess, finished[1]["outcome"])
	require.Equal(t, "http_5xx", finished[0]["error_category"])

	// The retry warning fires once, tied to the attempt it interrupts, and
	// carries the filterable retry_reason.
	warnings := allProviderLogLines(t, logs, "Provider request failed, retrying", sessionID)
	require.Len(t, warnings, 1)
	require.Equal(t, "server_error", warnings[0]["retry_reason"])
	require.EqualValues(t, 1, warnings[0]["attempt"], "the warning must name the attempt that failed")
	require.Equal(t, rt.turnID, warnings[0]["turn_id"])
}

// TestProviderRequest_RetryExhausted drives three consecutive failed attempts
// and proves an exhausted retry sequence is still a clean, countable log: every
// attempt gets a started/finished pair (no orphaned started), each retry is
// labeled by its cause, and the finished lines all carry the error outcome.
func TestProviderRequest_RetryExhausted(t *testing.T) {
	t.Parallel()
	logs := captureJSONLogs(t)

	rt, sessionID := newTurnForLogTest(t, "run-e", "turn-e")
	rt.model.Model = &fakeStreamModel{script: streamScript{createErr: &fantasy.ProviderError{StatusCode: 500, Title: "down"}}}

	const attempts = 3
	// PrepareStep stamps the step and resets the attempt counter once; retries
	// within the step then read 2, 3, ... so reset it once here, not per
	// attempt.
	rt.attempt = 0
	for i := range attempts {
		if i > 0 {
			// Each subsequent attempt is a retry of the failed previous one.
			rt.onRetry(&fantasy.ProviderError{StatusCode: 500, Title: "down"}, time.Millisecond)
		}
		stream, err := simulateRetryAttempt(t, rt)
		require.Error(t, err, "every attempt must fail in the exhausted case")
		_ = stream
	}

	started := allProviderLogLines(t, logs, "Provider request started", sessionID)
	finished := allProviderLogLines(t, logs, "Provider request finished", sessionID)
	require.Len(t, started, attempts)
	require.Len(t, finished, attempts, "every failed attempt must still get a finished line (no orphaned started)")
	require.Equal(t, len(started), len(finished))

	for i := range attempts {
		require.Equal(t, outcomeError, finished[i]["outcome"])
		require.Equal(t, "http_5xx", finished[i]["error_category"])
		require.EqualValues(t, i+1, started[i]["attempt"])
	}
	// The first attempt is the turn baseline; the rest are retries.
	require.Equal(t, reasonTurn, started[0]["request_reason"])
	for i := 1; i < attempts; i++ {
		require.Equal(t, reasonRetry, started[i]["request_reason"])
	}
}

// TestProviderRequest_AuthRefreshThenSuccess drives the turn's real
// modelProvider and onAuthRefresh through an auth-failure-then-refreshed
// sequence and proves the closed auth_refresh reason: the first attempt is the
// turn baseline, it fails with an auth error (401), a successful OnAuthRefresh
// marks the next attempt auth_refresh (not a plain retry), and that re-attempt
// succeeds. The attempt numbers and the 1:1 started/finished pairs stay
// countable throughout.
func TestProviderRequest_AuthRefreshThenSuccess(t *testing.T) {
	t.Parallel()
	logs := captureJSONLogs(t)

	rt, sessionID := newTurnForLogTest(t, "run-auth", "turn-auth")
	// The first Stream fails with an auth error; the refreshed Stream succeeds.
	failModel := &fakeStreamModel{script: streamScript{createErr: &fantasy.ProviderError{StatusCode: 401, Title: "unauthorized"}}}
	okModel := &fakeStreamModel{script: streamScript{parts: []fantasy.StreamPart{{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop, Usage: fantasy.Usage{OutputTokens: 3}}}}}
	rt.model.Model = failModel
	// A successful auth refresh: the coordinator's callback succeeds, so the
	// next attempt is an auth_refresh.
	rt.call.OnAuthRefresh = func(ctx context.Context, err *fantasy.ProviderError) error { return nil }

	rt.attempt = 0
	// Attempt 1: turn baseline, fails with an auth error.
	stream, err := simulateRetryAttempt(t, rt)
	require.Error(t, err, "attempt 1 must fail with the auth error")
	_ = stream
	// fantasy would now call OnAuthRefresh (the auth error is not a plain
	// retry); it succeeds, so the next attempt is an auth_refresh.
	require.NoError(t, rt.onAuthRefresh(t.Context(), &fantasy.ProviderError{StatusCode: 401, Title: "unauthorized"}))
	// Swap in the refreshed model, then attempt 2: auth_refresh reason, succeeds.
	rt.model.Model = okModel
	stream, err = simulateRetryAttempt(t, rt)
	require.NoError(t, err)
	consumeStream(stream)

	started := allProviderLogLines(t, logs, "Provider request started", sessionID)
	finished := allProviderLogLines(t, logs, "Provider request finished", sessionID)
	require.Len(t, started, 2, "two attempts = two started lines")
	require.Len(t, finished, 2, "two attempts = two finished lines")

	// The attempts are labeled: baseline turn, then auth_refresh (not retry).
	require.Equal(t, reasonTurn, started[0]["request_reason"])
	require.Equal(t, reasonAuthRefresh, started[1]["request_reason"],
		"a re-attempt after a successful auth refresh is an auth_refresh, not a plain retry")
	require.EqualValues(t, 1, started[0]["attempt"])
	require.EqualValues(t, 2, started[1]["attempt"])
	require.Equal(t, outcomeError, finished[0]["outcome"])
	require.Equal(t, outcomeSuccess, finished[1]["outcome"])
	require.Equal(t, "http_4xx", finished[0]["error_category"])
	// Each pair shares its attempt and turn id.
	require.Equal(t, rt.turnID, started[1]["turn_id"])
	require.Equal(t, started[1]["turn_id"], finished[1]["turn_id"])
}

// TestProviderRequest_AuthRefreshFailedKeepsNoPendingReason proves a *failed*
// auth refresh does not mark the next attempt as an auth_refresh: fantasy
// returns the original auth error and runs no further attempt, so no stray
// auth_refresh label is left pending.
func TestProviderRequest_AuthRefreshFailedKeepsNoPendingReason(t *testing.T) {
	t.Parallel()
	logs := captureJSONLogs(t)

	rt, sessionID := newTurnForLogTest(t, "run-auth-fail", "turn-auth-fail")
	rt.model.Model = &fakeStreamModel{script: streamScript{createErr: &fantasy.ProviderError{StatusCode: 401, Title: "unauthorized"}}}
	rt.call.OnAuthRefresh = func(ctx context.Context, err *fantasy.ProviderError) error { return errors.New("refresh failed") }

	rt.attempt = 0
	_, err := simulateRetryAttempt(t, rt)
	require.Error(t, err)
	// The refresh fails: onAuthRefresh returns the refresh error and must NOT
	// set the pending reason.
	require.Error(t, rt.onAuthRefresh(t.Context(), &fantasy.ProviderError{StatusCode: 401}))
	require.Empty(t, rt.pendingReason, "a failed refresh must not mark the next attempt as an auth_refresh")
	_ = sessionID
	_ = logs
}

// TestProviderRequest_AuthRefreshAfterRetryProvesPrecedence drives a turn that
// first retries a transient failure (500) and then, on the retried attempt,
// hits an auth error that a successful refresh resolves. It proves the two
// pending reasons are distinct and ordered correctly: the first re-attempt is
// a retry, the second (post-refresh) is an auth_refresh. This is the
// orchestration-level ordering the audit asked to pin (OnRetry then
// OnAuthRefresh then the next ModelProvider), reproduced without fantasy's
// backoff sleep.
func TestProviderRequest_AuthRefreshAfterRetryProvesPrecedence(t *testing.T) {
	t.Parallel()
	logs := captureJSONLogs(t)

	rt, sessionID := newTurnForLogTest(t, "run-auth-prec", "turn-auth-prec")
	rt.model.Model = &fakeStreamModel{script: streamScript{createErr: &fantasy.ProviderError{StatusCode: 500, Title: "down"}}}
	rt.call.OnAuthRefresh = func(ctx context.Context, err *fantasy.ProviderError) error { return nil }

	rt.attempt = 0
	// Attempt 1: turn baseline, transient 500 failure.
	stream, err := simulateRetryAttempt(t, rt)
	require.Error(t, err)
	_ = stream
	// fantasy retries the transient failure: onRetry marks the next attempt a
	// retry.
	rt.onRetry(&fantasy.ProviderError{StatusCode: 500, Title: "down"}, time.Millisecond)
	// Attempt 2: retry reason, but this time it fails with an auth error.
	_, err = simulateRetryAttempt(t, rt)
	require.Error(t, err)
	// The auth failure is not a plain retry; fantasy calls OnAuthRefresh,
	// which succeeds and marks the next attempt an auth_refresh.
	require.NoError(t, rt.onAuthRefresh(t.Context(), &fantasy.ProviderError{StatusCode: 401, Title: "unauthorized"}))
	// Attempt 3: auth_refresh reason, succeeds.
	rt.model.Model = &fakeStreamModel{script: streamScript{parts: []fantasy.StreamPart{{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop}}}}
	stream, err = simulateRetryAttempt(t, rt)
	require.NoError(t, err)
	consumeStream(stream)

	started := allProviderLogLines(t, logs, "Provider request started", sessionID)
	finished := allProviderLogLines(t, logs, "Provider request finished", sessionID)
	require.Len(t, started, 3)
	require.Len(t, finished, 3)
	// The three reasons in order: baseline turn, transient retry, then
	// auth_refresh after the successful refresh.
	require.Equal(t, reasonTurn, started[0]["request_reason"])
	require.Equal(t, reasonRetry, started[1]["request_reason"])
	require.Equal(t, reasonAuthRefresh, started[2]["request_reason"],
		"the post-refresh re-attempt is an auth_refresh, distinct from the transient retry before it")
	// Attempt numbers are 1, 2, 3 and each pair shares its attempt + turn id.
	for i := range started {
		require.EqualValues(t, i+1, started[i]["attempt"])
		require.EqualValues(t, i+1, finished[i]["attempt"])
		require.Equal(t, started[i]["turn_id"], finished[i]["turn_id"])
	}
	require.Equal(t, outcomeError, finished[0]["outcome"])
	require.Equal(t, outcomeError, finished[1]["outcome"])
	require.Equal(t, outcomeSuccess, finished[2]["outcome"])
}

// TestProviderRequest_SummarizeLabeled drives a real summarize pass and proves
// it is logged as its own request pair labeled summary - the plan's "summarize"
// cause, distinct from the turn's ordinary steps.
// summaryAfterTurnModel triggers auto-summarization after a successful first
// turn and records the RunIDs supplied to each provider request.
type summaryAfterTurnModel struct {
	streams atomic.Int32
}

func (*summaryAfterTurnModel) Provider() string { return "fake" }
func (*summaryAfterTurnModel) Model() string    { return "fake-model" }
func (*summaryAfterTurnModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not used")
}

func (m *summaryAfterTurnModel) Stream(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	stream := m.streams.Add(1)
	return func(yield func(fantasy.StreamPart) bool) {
		if stream == 1 {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "text"})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: "reply"})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "text"})
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func (*summaryAfterTurnModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not used")
}

func (*summaryAfterTurnModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not used")
}

// TestProviderRequest_AutoSummarizeUsesCallRunID ensures auto-summarize uses
// the triggering call's RunID even if the direct caller did not use WithRunID.
func TestProviderRequest_AutoSummarizeUsesCallRunID(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	logs := captureJSONLogs(t)
	model := &summaryAfterTurnModel{}
	sa := NewSessionAgent(SessionAgentOptions{
		Model:    Model{Model: model, CatalogCfg: catwalk.Model{ContextWindow: 1, DefaultMaxTokens: 100}},
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)
	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	_, err = sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, RunID: "call-run", Prompt: "prompt"})
	require.NoError(t, err)

	started := allProviderLogLines(t, logs, "Provider request started", sess.ID)
	require.Len(t, started, 2)
	require.Equal(t, reasonTurn, started[0]["request_reason"])
	require.Equal(t, "call-run", started[0]["run_id"])
	require.Equal(t, reasonSummary, started[1]["request_reason"])
	require.Equal(t, "call-run", started[1]["run_id"])
}

// TestProviderRequest_QueuedAutoSummarizeUsesOwnRunID ensures a queued call's
// summary does not inherit the parent context's RunID when the queue drains.
func TestProviderRequest_QueuedAutoSummarizeUsesOwnRunID(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	logs := captureJSONLogs(t)
	model := &summaryAfterTurnModel{}
	sa := NewSessionAgent(SessionAgentOptions{
		Model:    Model{Model: model, CatalogCfg: catwalk.Model{ContextWindow: 1, DefaultMaxTokens: 100}},
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)
	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	// Make the first call wait in its stream so the second is necessarily queued.
	// The direct queue setup keeps this regression focused on the handoff context.
	sa.enqueueCall(SessionAgentCall{SessionID: sess.ID, RunID: "queued-run", Prompt: "queued"})
	_, err = sa.Run(WithRunID(t.Context(), "parent-run"), SessionAgentCall{SessionID: sess.ID, RunID: "parent-run", Prompt: "parent"})
	require.NoError(t, err)

	started := allProviderLogLines(t, logs, "Provider request started", sess.ID)
	var summaryRunIDs []string
	for _, line := range started {
		if line["request_reason"] == reasonSummary {
			summaryRunIDs = append(summaryRunIDs, line["run_id"].(string))
		}
	}
	require.Equal(t, []string{"parent-run", "queued-run"}, summaryRunIDs)
}

func TestProviderRequest_SummarizeLabeled(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	logs := captureJSONLogs(t)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	// A summarize bails when the session has no history, so seed one user and
	// one assistant message for it to compress.
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "seed prompt"}},
	})
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "seed reply"}},
	})
	require.NoError(t, err)

	model := &usageReportingModel{usage: fantasy.Usage{InputTokens: 40, OutputTokens: 20, CacheReadTokens: 15}}
	sa := NewSessionAgent(SessionAgentOptions{
		Model:        Model{Model: model, CatalogCfg: catwalkModel()},
		SystemPrompt: "system",
		Sessions:     env.sessions,
		Messages:     env.messages,
	}).(*sessionAgent)

	// Standalone summarize (the coordinator path) has no SessionAgentCall; its
	// correlation must therefore preserve the RunID supplied on the context.
	require.NoError(t, sa.summarize(WithRunID(t.Context(), "standalone-run"), sess.ID, fantasy.ProviderOptions{}, nil, nil, sa.model.Get(), "", nil, nil))

	started := allProviderLogLines(t, logs, "Provider request started", sess.ID)
	finished := allProviderLogLines(t, logs, "Provider request finished", sess.ID)
	require.Len(t, started, 1, "a summarize makes exactly one provider request")
	require.Len(t, finished, 1, "a summarize finishes exactly one provider request")

	s, f := started[0], finished[0]
	require.Equal(t, reasonSummary, s["request_reason"], "the summarize request must be labeled summary")
	require.Equal(t, reasonSummary, f["request_reason"])
	require.Equal(t, "standalone-run", s["run_id"])
	require.Equal(t, "standalone-run", f["run_id"])
	require.Equal(t, s["turn_id"], f["turn_id"], "the started and finished lines must share the summarize turn id")
	require.Equal(t, outcomeSuccess, f["outcome"])
	// The summarize's own usage (its cache read marks whether the prompt
	// prefix hit the provider cache).
	require.EqualValues(t, 15, f["cache_read_tokens"])
}

// TestProviderRequest_SummarizeCancel drives a summarize whose stream is
// abandoned by a canceled context and proves the attempt logs a canceled
// outcome rather than a success or an error.
func TestProviderRequest_SummarizeCancel(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	logs := captureJSONLogs(t)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "seed prompt"}},
	})
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "seed reply"}},
	})
	require.NoError(t, err)

	// A model whose Stream blocks until the context is done: the summarize's
	// stream is then abandoned by the cancellation.
	model := &blockingStreamModel{}
	sa := NewSessionAgent(SessionAgentOptions{
		Model:        Model{Model: model, CatalogCfg: catwalkModel()},
		SystemPrompt: "system",
		Sessions:     env.sessions,
		Messages:     env.messages,
	}).(*sessionAgent)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- sa.summarize(ctx, sess.ID, fantasy.ProviderOptions{}, nil, nil, sa.model.Get(), "", nil, nil)
	}()
	// Let the summarize reach its blocking Stream, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the canceled summarize to return")
	}

	finished := allProviderLogLines(t, logs, "Provider request finished", sess.ID)
	require.NotEmpty(t, finished, "the canceled summarize attempt must still log a finished line")
	require.Equal(t, outcomeCanceled, finished[len(finished)-1]["outcome"])
}

// seedSummarizableSession creates a session with one user and one assistant
// message, the minimum history a summarize pass compresses (an empty session
// would bail before making a provider request).
func seedSummarizableSession(t *testing.T, env fakeEnv, name string) string {
	t.Helper()
	sess, err := env.sessions.Create(t.Context(), name)
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "seed prompt"}},
	})
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "seed reply"}},
	})
	require.NoError(t, err)
	return sess.ID
}

// callCountedModel is a fantasy model whose Stream behavior is chosen by a
// counter: the first N-1 calls return the failure script, the Nth (and any
// later) call returns the success script. It stands in for the provider across
// a summarize's retry/auth-refresh attempts so a real summarize pass can be
// driven through a failure-then-success sequence and the per-attempt reasons
// read back from the finished/started lines.
type callCountedModel struct {
	failures   int
	failScript streamScript
	okScript   streamScript
	calls      int
}

func (c *callCountedModel) Stream(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	c.calls++
	var script streamScript
	if c.calls <= c.failures {
		script = c.failScript
	} else {
		script = c.okScript
	}
	inner := &fakeStreamModel{script: script}
	return inner.Stream(ctx, fantasy.Call{})
}

func (c *callCountedModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not used")
}

func (c *callCountedModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not used")
}

func (c *callCountedModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not used")
}
func (c *callCountedModel) Provider() string { return "fake" }
func (c *callCountedModel) Model() string    { return "fake-model" }

// TestProviderRequest_SummarizeRetryLabeled drives a real summarize pass whose
// first attempt fails transiently (500) and the retry succeeds, and proves the
// summarize's own reason tracking: the first attempt is labeled summary, the
// retry is labeled retry (not a second summary), and the two attempts are a
// countable 1:1 pair sequence with attempt numbers 1 and 2 sharing the
// summarize's turn id. This exercises the summarize's real fantasy retry loop
// (one backoff sleep) rather than faking the orchestration.
func TestProviderRequest_SummarizeRetryLabeled(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	logs := captureJSONLogs(t)

	sessID := seedSummarizableSession(t, env, "sum-retry")
	model := &callCountedModel{
		failures:   1,
		failScript: streamScript{createErr: &fantasy.ProviderError{StatusCode: 500, Title: "down"}},
		okScript:   streamScript{parts: []fantasy.StreamPart{{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop, Usage: fantasy.Usage{OutputTokens: 3}}}},
	}
	sa := NewSessionAgent(SessionAgentOptions{
		Model:        Model{Model: model, CatalogCfg: catwalkModel()},
		SystemPrompt: "system",
		Sessions:     env.sessions,
		Messages:     env.messages,
	}).(*sessionAgent)

	require.NoError(t, sa.summarize(t.Context(), sessID, fantasy.ProviderOptions{}, nil, nil, sa.model.Get(), "", nil, nil))

	started := allProviderLogLines(t, logs, "Provider request started", sessID)
	finished := allProviderLogLines(t, logs, "Provider request finished", sessID)
	require.Len(t, started, 2, "a failed-then-retried summarize makes two provider attempts")
	require.Len(t, finished, 2, "both attempts get a finished line (no orphaned started)")

	// The first attempt is the summarize's baseline, the retry is labeled
	// retry - not a second summary.
	require.Equal(t, reasonSummary, started[0]["request_reason"])
	require.Equal(t, reasonRetry, started[1]["request_reason"], "the summarize's retry is a retry, not another summary")
	require.EqualValues(t, 1, started[0]["attempt"])
	require.EqualValues(t, 2, started[1]["attempt"])
	// Both share the summarize's own turn id (the pass minted one), so the two
	// pairs are correlated to the same summarize.
	require.Equal(t, started[0]["turn_id"], started[1]["turn_id"], "both summarize attempts share the pass's turn id")
	require.Equal(t, started[1]["turn_id"], finished[1]["turn_id"])
	require.Equal(t, outcomeError, finished[0]["outcome"])
	require.Equal(t, outcomeSuccess, finished[1]["outcome"])
}

// TestProviderRequest_SummarizeAuthRefreshLabeled drives a real summarize pass
// whose first attempt fails with an auth error (401), a successful OnAuthRefresh
// refreshes the credentials, and the re-attempt (a fresh fantasy retry pass)
// succeeds. It proves the summarize's auth_refresh reason: the first attempt
// is a summary, the post-refresh re-attempt is an auth_refresh (distinct from a
// plain retry), and the sequence is a countable 1:1 pair with the shared turn
// id. A 401 is not a transient retry, so fantasy's outer auth-refresh path (not
// the inner retry loop) runs - no backoff sleep.
// TestProviderRequest_SummarizeUnauthorizedWithoutRefresh does not install an
// auth-refresh callback. A 401 must remain terminal: exactly one model call
// and one summary request pair, rather than a synthetic wrapper retrying it.
func TestProviderRequest_SummarizeUnauthorizedWithoutRefresh(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	logs := captureJSONLogs(t)

	sessID := seedSummarizableSession(t, env, "sum-auth-no-refresh")
	model := &callCountedModel{
		failures:   1,
		failScript: streamScript{createErr: &fantasy.ProviderError{StatusCode: 401, Title: "unauthorized"}},
		okScript:   streamScript{parts: []fantasy.StreamPart{{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop}}},
	}
	sa := NewSessionAgent(SessionAgentOptions{
		Model:        Model{Model: model, CatalogCfg: catwalkModel()},
		SystemPrompt: "system",
		Sessions:     env.sessions,
		Messages:     env.messages,
	}).(*sessionAgent)

	err := sa.summarize(t.Context(), sessID, fantasy.ProviderOptions{}, nil, nil, sa.model.Get(), "", nil, nil)
	require.Error(t, err)

	started := allProviderLogLines(t, logs, "Provider request started", sessID)
	finished := allProviderLogLines(t, logs, "Provider request finished", sessID)
	require.Equal(t, 1, model.calls, "a nil auth refresh must not restart a 401 summarize")
	require.Len(t, started, 1)
	require.Len(t, finished, 1)
	require.Equal(t, reasonSummary, started[0]["request_reason"])
	require.Equal(t, outcomeError, finished[0]["outcome"])
	require.Equal(t, "http_4xx", finished[0]["error_category"])
}

func TestProviderRequest_SummarizeAuthRefreshLabeled(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	logs := captureJSONLogs(t)

	sessID := seedSummarizableSession(t, env, "sum-auth")
	model := &callCountedModel{
		failures:   1,
		failScript: streamScript{createErr: &fantasy.ProviderError{StatusCode: 401, Title: "unauthorized"}},
		okScript:   streamScript{parts: []fantasy.StreamPart{{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop, Usage: fantasy.Usage{OutputTokens: 3}}}},
	}
	sa := NewSessionAgent(SessionAgentOptions{
		Model:        Model{Model: model, CatalogCfg: catwalkModel()},
		SystemPrompt: "system",
		Sessions:     env.sessions,
		Messages:     env.messages,
	}).(*sessionAgent)

	// A successful auth refresh: the credentials are rebuilt, so fantasy
	// restarts the pass and the next attempt is an auth_refresh.
	require.NoError(t, sa.summarize(t.Context(), sessID, fantasy.ProviderOptions{},
		func(ctx context.Context, err *fantasy.ProviderError) error { return nil },
		nil,
		sa.model.Get(), "", nil, nil))

	started := allProviderLogLines(t, logs, "Provider request started", sessID)
	finished := allProviderLogLines(t, logs, "Provider request finished", sessID)
	require.Len(t, started, 2, "an auth-failed-then-refreshed summarize makes two provider attempts")
	require.Len(t, finished, 2, "both attempts get a finished line (no orphaned started)")

	// The first attempt is the summarize's baseline; the post-refresh
	// re-attempt is an auth_refresh, not a plain retry.
	require.Equal(t, reasonSummary, started[0]["request_reason"])
	require.Equal(t, reasonAuthRefresh, started[1]["request_reason"],
		"a summarize re-attempt after a successful auth refresh is an auth_refresh, not a retry")
	require.EqualValues(t, 1, started[0]["attempt"])
	require.EqualValues(t, 2, started[1]["attempt"])
	require.Equal(t, started[0]["turn_id"], started[1]["turn_id"], "both summarize attempts share the pass's turn id")
	require.Equal(t, outcomeError, finished[0]["outcome"])
	require.Equal(t, "http_4xx", finished[0]["error_category"])
	require.Equal(t, outcomeSuccess, finished[1]["outcome"])
}

// TestProviderRequestLogsCarryNoPrompt drives a real turn through a scripted
// model and asserts that no provider request line carries the prompt text:
// the request lines log only ids, reasons, outcomes and counts.
func TestProviderRequestLogsCarryNoPrompt(t *testing.T) {
	t.Parallel()
	logs := captureJSONLogs(t)

	const prompt = "the-needle-prompt-that-must-not-be-logged"
	rt, sessionID := newTurnForLogTest(t, "run-np", "turn-np")
	rt.model.Model = &fakeStreamModel{script: streamScript{parts: []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeTextDelta, Delta: "a reply"},
		{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop},
	}}}
	rt.attempt = 0
	stream, err := simulateRetryAttempt(t, rt)
	require.NoError(t, err)
	consumeStream(stream)

	for _, line := range []map[string]any{
		findProviderLogLine(t, logs, "Provider request started", sessionID),
		findProviderLogLine(t, logs, "Provider request finished", sessionID),
	} {
		require.NotNil(t, line)
		require.NotContains(t, mustMarshal(t, line), prompt,
			"provider request lines must not carry the prompt")
	}
}

// TestModelProviderLogsAtDebugNotInfo proves the level split the audit asked
// for: the per-attempt "ModelProvider called" runtime callback is Debug (it was
// the line mistaken for the request count), while the "provider request
// started" line that actually counts requests is Info.
func TestModelProviderLogsAtDebugNotInfo(t *testing.T) {
	t.Parallel()
	logs := captureJSONLogs(t)

	rt, sessionID := newTurnForLogTest(t, "run-lvl", "turn-lvl")
	rt.model.Model = &fakeStreamModel{script: streamScript{parts: []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop},
	}}}
	rt.attempt = 0
	stream, err := simulateRetryAttempt(t, rt)
	require.NoError(t, err)
	consumeStream(stream)

	mp := findProviderLogLine(t, logs, "ModelProvider called", sessionID)
	require.NotNil(t, mp, "the ModelProvider callback must still be logged (now at Debug)")
	require.Equal(t, slog.LevelDebug, slogLevel(t, mp), "ModelProvider called must be Debug, not Info")

	started := findProviderLogLine(t, logs, "Provider request started", sessionID)
	require.NotNil(t, started)
	require.Equal(t, slog.LevelInfo, slogLevel(t, started), "the request-started line is the count and stays Info")
}

// TestProviderRequestLogs_CorrelationMatchesModelProvider proves the request
// pair and the ModelProvider callback share the turn's correlation ids
// (run_id, session_id, turn_id), so the request can be correlated with the
// runtime callback that produced it.
func TestProviderRequestLogs_CorrelationMatchesModelProvider(t *testing.T) {
	t.Parallel()
	logs := captureJSONLogs(t)

	const runID, turnID = "run-corr", "turn-corr"
	rt, sessionID := newTurnForLogTest(t, runID, turnID)
	rt.model.Model = &fakeStreamModel{script: streamScript{parts: []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop},
	}}}
	rt.attempt = 0
	stream, err := simulateRetryAttempt(t, rt)
	require.NoError(t, err)
	consumeStream(stream)

	mp := findProviderLogLine(t, logs, "ModelProvider called", sessionID)
	started := findProviderLogLine(t, logs, "Provider request started", sessionID)
	require.NotNil(t, mp, "ModelProvider callback must carry the session id")
	require.NotNil(t, started, "the request started line must carry the session id")
	require.Equal(t, mp["turn_id"], started["turn_id"], "both must share the turn id")
	require.Equal(t, mp["session_id"], started["session_id"], "both must share the session id")
	require.Equal(t, runID, started["run_id"], "the request line must carry the run id")
	require.Equal(t, turnID, started["turn_id"], "the request line must carry the turn id")
}

// TestToolLifecycleLogs_ActualCallbacksEmitSafeCorrelation exercises the real
// callbacks wired into fantasy.AgentStreamCall. Their records join on the tool
// call ID but never serialize user-supplied arguments or tool output.
func TestToolLifecycleLogs_ActualCallbacksEmitSafeCorrelation(t *testing.T) {
	t.Parallel()
	logs := captureJSONLogs(t)

	rt, sessionID := newTurnForLogTest(t, "run-tool", "turn-tool")
	const secretInput = `{"token":"do-not-log"}`
	const secretOutput = "do-not-log-output"
	require.NoError(t, rt.onToolCall(fantasy.ToolCallContent{
		ToolCallID: "tool-1", ToolName: "read", Input: secretInput,
	}))
	require.NoError(t, rt.onToolResult(fantasy.ToolResultContent{
		ToolCallID: "tool-1", ToolName: "read",
		Result: fantasy.ToolResultOutputContentText{Text: secretOutput},
	}))

	call := findProviderLogLine(t, logs, "Tool lifecycle", sessionID)
	require.NotNil(t, call)
	lines := allProviderLogLines(t, logs, "Tool lifecycle", sessionID)
	require.Len(t, lines, 2)
	for _, line := range lines {
		require.Equal(t, "run-tool", line["run_id"])
		require.Equal(t, "turn-tool", line["turn_id"])
		require.Equal(t, "tool-1", line["tool_call_id"])
		require.Equal(t, "read", line["tool_name"])
		require.NotContains(t, mustMarshal(t, line), "do-not-log")
	}
	require.Equal(t, "tool_call", lines[0]["event"])
	require.Equal(t, "started", lines[0]["tool_outcome"])
	require.Equal(t, "tool_result", lines[1]["event"])
	require.Equal(t, "success", lines[1]["tool_outcome"])
}

// mustMarshal renders a decoded log line back to a string so a test can assert
// on the absence of a needle anywhere in it (keys and values).
func mustMarshal(t *testing.T, line map[string]any) string {
	t.Helper()
	b, err := json.Marshal(line)
	require.NoError(t, err)
	return string(b)
}

// usageReportingModel is a fantasy model that streams a fixed text reply and a
// finish part carrying the configured usage, then stops. It is the scripted
// provider used to drive a real summarize pass and read its usage from the
// finished line.
type usageReportingModel struct {
	usage fantasy.Usage
}

func (f *usageReportingModel) Stream(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "summary"}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop, Usage: f.usage})
	}, nil
}

func (f *usageReportingModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not used")
}

func (f *usageReportingModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not used")
}

func (f *usageReportingModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not used")
}
func (f *usageReportingModel) Provider() string { return "fake" }
func (f *usageReportingModel) Model() string    { return "fake-model" }

// blockingStreamModel is a fantasy model whose Stream never yields and blocks
// until the context is done. It is used to test cancellation of an in-flight
// summarize: the stream is abandoned, and the instrumented model must log a
// canceled outcome.
type blockingStreamModel struct{}

func (b *blockingStreamModel) Stream(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	return func(yield func(fantasy.StreamPart) bool) {
		<-ctx.Done()
	}, nil
}

func (b *blockingStreamModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not used")
}

func (b *blockingStreamModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not used")
}

func (b *blockingStreamModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not used")
}
func (b *blockingStreamModel) Provider() string { return "fake" }
func (b *blockingStreamModel) Model() string    { return "fake-model" }

// catwalkModel is a tiny helper to build a catwalk.Model with the summarize
// buffer's required context window and default max tokens.
func catwalkModel() catwalk.Model {
	return catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}
}
