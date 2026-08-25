package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"charm.land/fantasy"

	"github.com/google/uuid"
)

// Provider request log vocabulary.
//
// Every line that represents a real provider request carries the same
// correlation block (session_id, run_id, turn_id, step, attempt,
// request_reason) plus, on the finished line, an outcome and - when the
// outcome is not success - a safe error_category. The started and finished
// lines are emitted only from the instrumented model below, so they are
// always a 1:1 pair for one model.Stream attempt, whether that attempt
// succeeds, errors, or is canceled.
//
// request_reason is the *cause* of the request, the distinction the run-log
// audit asked for (a summarize is an extra request, a retry is a re-attempt,
// an auto-woken continuation is not a user prompt). It is one of the constants
// below and is safe to filter on.
const (
	// reasonTurn is the baseline: an ordinary model call that advances the
	// turn - the first step, or a tool-continuation step after tool results
	// came back. This is the "normal" request the others are extra on top of.
	reasonTurn = "turn"
	// reasonContinuation is a turn that auto-woke from the completion inbox
	// (a background task or thread finished); call.Continuation is set.
	reasonContinuation = "continuation"
	// reasonSummary is a summarize pass that compresses the session's own
	// history before continuing.
	reasonSummary = "summary"
	// reasonRetry is a re-attempt of a provider request that the previous
	// attempt failed and fantasy chose to retry.
	reasonRetry = "retry"
	// reasonAuthRefresh is a re-attempt made after a failed authentication
	// (401/403) was resolved by a successful OnAuthRefresh credential refresh.
	// fantasy restarts the whole retry pass with a fresh budget after the
	// refresh, so the first attempt of that pass is this cause (distinct from
	// a plain retry, which is a re-attempt of a transiently-failed request).
	reasonAuthRefresh = "auth_refresh"
)

// Request outcomes for the "Provider request finished" line. outcome is a
// closed, safe set: it says *what happened* to the attempt, never *why* in
// words. error_category is a closed set too, present only when the outcome is
// an error, and is derived from the error's type - never its message - so no
// prompt, response body or credential detail can leak into the log.
//
// The outcomes are deliberately distinct so a log reader cannot mistake a
// half-finished stream for a completed one:
//
//   - success  the stream reached its terminal finish part (the provider
//     completed the response); finish_reason and usage are present.
//   - error    the attempt failed with a provider/transport error (the Stream
//     call returned an error, or the stream yielded an error part);
//     error_category is present.
//   - canceled the context was canceled before the attempt completed (the
//     Stream call returned the context error, or the stream was abandoned
//     mid-flight); the cancellation *is* the outcome, so no error_category.
//   - aborted  the stream ended without a terminal finish part and without an
//     error - the consumer stopped ranging early, or the provider closed the
//     stream short. This is NOT a success: no terminal finish means the
//     provider never completed the response.
const (
	outcomeSuccess  = "success"
	outcomeError    = "error"
	outcomeCanceled = "canceled"
	outcomeAborted  = "aborted"
)

// providerCorrelation is the stable identity of one provider attempt, captured
// at the moment the attempt starts (in modelProvider, or summarize's
// ModelProvider) and handed to the instrumented model that performs the
// Stream. It is the single source of the started/finished correlation block,
// so the two lines of one attempt cannot drift apart.
type providerCorrelation struct {
	sessionID string
	runID     string
	turnID    string
	step      int
	attempt   int
	reason    string
}

func (c providerCorrelation) fields() []any {
	return providerRequestLogFields(c.sessionID, c.runID, c.turnID, c.step, c.attempt, c.reason)
}

// providerRequestLogFields is the shared correlation block every provider
// request log line carries. session_id and run_id are the run's identity
// (run_id may be empty for an in-memory session, in which case turn_id is the
// only stable per-turn handle); turn_id is a per-runTurn uuid; step is the
// fantasy step number; attempt is the 1-based network attempt within the step
// (first attempt is 1, the first retry is 2, ...); reason is the
// request_reason token above. Field names are fixed here and reused by every
// site, so they cannot drift the way "session" vs "session_id" used to.
func providerRequestLogFields(sessionID, runID, turnID string, step, attempt int, reason string) []any {
	return []any{
		"session_id", sessionID,
		"run_id", runID,
		"turn_id", turnID,
		"step", step,
		"attempt", attempt,
		"request_reason", reason,
	}
}

// instrumentedModel wraps a fantasy.LanguageModel and logs one
// "Provider request started" / "Provider request finished" pair for every
// model.Stream call.
//
// fantasy's per-step retry loop (agent.go) calls the ModelProvider - which
// returns a fresh instrumentedModel - and then model.Stream once, inside the
// same retry attempt function. So one instrumented Stream call = one attempt
// = one pair, regardless of whether that attempt succeeds, errors, or is
// canceled. This is the fix for the earlier callback-level instrumentation,
// which logged started from ModelProvider and finished from onStepFinish: a
// failed attempt that got retried produced a started with no finished, and a
// step with two attempts produced two starteds but one finished.
//
// The wrapper is the *lowest available Stream boundary*: it sits directly
// around the concrete model's Stream, inside fantasy's retry loop, which is
// the closest point we can instrument without forking fantasy. The honest
// limit, documented per the audit: a model.Stream can still fail *before* the
// HTTP request is sent (the transport layer rejecting the request locally),
// and it can also complete the HTTP handshake and then fail while *reading*
// the stream. So a pair counts a *provider attempt*, not strictly a completed
// HTTP round-trip; the outcome field makes that explicit (success | error |
// canceled) instead of implying every pair is a full exchange.
type instrumentedModel struct {
	inner     fantasy.LanguageModel
	corr      providerCorrelation
	startedAt time.Time
}

// newInstrumentedModel wraps inner so its Stream is logged. startedAt is taken
// at wrap time, which is the instant the attempt begins (right before Stream
// is called by fantasy's retry loop); latency on the finished line is
// measured from it.
func newInstrumentedModel(inner fantasy.LanguageModel, corr providerCorrelation) *instrumentedModel {
	return &instrumentedModel{inner: inner, corr: corr, startedAt: time.Now()}
}

// Stream implements fantasy.LanguageModel.Stream. It logs the attempt started
// immediately before calling inner.Stream, and the attempt finished when the
// attempt ends: either inner.Stream itself returned an error (the request
// never became a stream), or the returned stream terminates. The finished line
// carries outcome, latency, finish_reason and the provider's usage.
//
// The returned iterator tees every part through unchanged, so the consumer
// (fantasy processStepStream) behaves exactly as if it were ranging over the
// raw stream; the wrapper only observes the terminal finish part (which
// carries FinishReason + Usage) and any error part. The outcome on the
// finished line is decided from what the wrapper actually observed:
//
//   - inner.Stream returned an error -> error (or canceled, if the context is
//     already done - the cancellation is the outcome, not a category),
//   - the stream yielded an error part -> error + a safe error_category,
//   - the stream reached its terminal finish part -> success + finish_reason
//   - usage,
//   - the context was canceled before a terminal finish -> canceled (no
//     category),
//   - otherwise (the stream ended, or the consumer stopped ranging early,
//     without a terminal finish and without an error) -> aborted: the attempt
//     did not complete, so it must not be counted as a success.
//
// The terminal-finish tracking is the fix for a class of false successes: a
// stream that is interrupted short (the provider closes early, or the consumer
// abandons it) must not be logged as success just because the ranging loop
// ended.
func (m *instrumentedModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	slog.Info("Provider request started", append(m.corr.fields(),
		"provider", m.inner.Provider(),
		"model", m.inner.Model(),
	)...)

	stream, err := m.inner.Stream(ctx, call)
	if err != nil {
		// The stream was never created: the attempt failed at the boundary.
		// If the context is already done, the cancellation is what failed the
		// creation (the transport returns the context error on abort), so the
		// outcome is canceled with no error_category - not an error with a
		// category that would imply a provider fault.
		outcome, category := outcomeError, errorCategory(err)
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			outcome, category = outcomeCanceled, ""
		}
		m.logFinished(outcome, category, "", fantasy.Usage{})
		return nil, err
	}

	return func(yield func(fantasy.StreamPart) bool) {
		usage := fantasy.Usage{}
		finishReason := fantasy.FinishReason("")
		// sawFinish is true once the terminal finish part has been observed;
		// it is the authoritative "the provider completed the response"
		// signal that separates success from an interrupted stream.
		sawFinish := false
		var streamErr error

		// The deferred closure is the "finished" half of the pair. It runs
		// exactly once when the consumer's ranging over this wrapper stops:
		// on normal exhaustion, on an error part, when the context is
		// canceled and the consumer abandons the stream, or when the stream
		// simply ends short of a terminal finish.
		defer func() {
			outcome, category := m.attemptOutcome(ctx, sawFinish, streamErr)
			m.logFinished(outcome, category, finishReason, usage)
		}()

		for part := range stream {
			switch part.Type {
			case fantasy.StreamPartTypeFinish:
				usage = part.Usage
				finishReason = part.FinishReason
				sawFinish = true
			case fantasy.StreamPartTypeError:
				streamErr = part.Error
			}
			if !yield(part) {
				// The consumer stopped ranging before the stream was
				// exhausted (it hit an error part, or was interrupted). We
				// record nothing further; the deferred finished line decides
				// the outcome from sawFinish/streamErr/ctx.
				return
			}
		}
	}, nil
}

// attemptOutcome maps what the wrapper observed on a stream attempt to the
// finished line's (outcome, error_category). It is pure so the mapping is
// unit-testable without a log capture. The precedence is deliberate:
//
//  1. an explicit cancellation error part -> canceled (no category),
//  2. another explicit error part -> error + its category (a provider failure
//     is the most specific outcome),
//  3. a terminal finish part was seen -> success (the provider completed the
//     response; a later context cancel is irrelevant to that),
//  4. the context is done and no terminal finish was reached -> canceled (the
//     attempt was interrupted by cancellation; the cancellation is the
//     outcome, so no category),
//  5. otherwise -> aborted (the stream ended short, or the consumer stopped
//     early, without a terminal finish and without an error).
func (m *instrumentedModel) attemptOutcome(ctx context.Context, sawFinish bool, streamErr error) (string, string) {
	switch {
	case errors.Is(streamErr, context.Canceled):
		return outcomeCanceled, ""
	case streamErr != nil:
		return outcomeError, errorCategory(streamErr)
	case sawFinish:
		return outcomeSuccess, ""
	case ctx.Err() != nil:
		return outcomeCanceled, ""
	default:
		return outcomeAborted, ""
	}
}

// Generate implements fantasy.LanguageModel.Generate. The turn and summarize
// paths use Stream (and therefore the instrumented pair); Generate is not
// used on those paths, so it delegates without logging to avoid a second,
// unpaired line. If a future path starts using Generate, instrument it the
// same way Stream is.
func (m *instrumentedModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	return m.inner.Generate(ctx, call)
}

func (m *instrumentedModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return m.inner.GenerateObject(ctx, call)
}

func (m *instrumentedModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return m.inner.StreamObject(ctx, call)
}

func (m *instrumentedModel) Provider() string { return m.inner.Provider() }
func (m *instrumentedModel) Model() string    { return m.inner.Model() }

// logFinished emits the "Provider request finished" line, the counterpart of
// the started line logged in Stream. latency is measured from the attempt's
// start (wrap time). finishReason and usage are empty/zero when the attempt
// never reached a terminal finish part. It logs no content - the correlation
// ids, the outcome, a safe error_category (only when outcome is an error),
// finish_reason and usage counts only.
func (m *instrumentedModel) logFinished(outcome, category string, finishReason fantasy.FinishReason, usage fantasy.Usage) {
	fields := m.corr.fields()
	fields = append(fields,
		"provider", m.inner.Provider(),
		"model", m.inner.Model(),
		"latency", time.Since(m.startedAt).Round(time.Millisecond).String(),
		"outcome", outcome,
	)
	if category != "" {
		fields = append(fields, "error_category", category)
	}
	if finishReason != "" {
		fields = append(fields, "finish_reason", string(finishReason))
	}
	fields = append(fields, providerUsageLogFields(usage)...)
	slog.Info("Provider request finished", fields...)
}

// errorCategory maps an error to a closed, safe set of tokens without ever
// reading its message. It is only ever non-empty on a non-success outcome;
// the returned token names the class of failure, not its text.
func errorCategory(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if providerErr, ok := errors.AsType[*fantasy.ProviderError](err); ok {
		// A provider-reported error: if it carries an HTTP status, name the
		// status band; otherwise it is a provider-level failure (no status).
		switch {
		case providerErr.StatusCode >= 500:
			return "http_5xx"
		case providerErr.StatusCode >= 400:
			return "http_4xx"
		case providerErr.StatusCode > 0:
			return "http_other"
		default:
			return "provider"
		}
	}
	if _, ok := errors.AsType[*fantasy.Error](err); ok {
		return "provider"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if fantasy.IsTransportError(err) {
		return "transport"
	}
	return "unknown"
}

// providerUsageLogFields renders a fantasy.Usage for a finished request.
// Tokens are only present when the provider actually reported them; a
// provider that omits usage leaves the fields off rather than logging a
// misleading zero (the difference between "no usage reported" and "zero
// tokens" is exactly what a cost/capacity diagnosis needs). Cache read vs
// creation are split so a cache hit (reads) is tellable from a cache miss
// (writes).
func providerUsageLogFields(usage fantasy.Usage) []any {
	fields := make([]any, 0, 8)
	if usage.InputTokens != 0 {
		fields = append(fields, "input_tokens", usage.InputTokens)
	}
	if usage.OutputTokens != 0 {
		fields = append(fields, "output_tokens", usage.OutputTokens)
	}
	if usage.ReasoningTokens != 0 {
		fields = append(fields, "reasoning_tokens", usage.ReasoningTokens)
	}
	if usage.CacheReadTokens != 0 {
		fields = append(fields, "cache_read_tokens", usage.CacheReadTokens)
	}
	if usage.CacheCreationTokens != 0 {
		fields = append(fields, "cache_creation_tokens", usage.CacheCreationTokens)
	}
	return fields
}

// providerRetryReason reduces a failed provider error to a short, closed
// reason token suitable for the filterable "retry_reason" on the retry
// warning: it says *why* the attempt is being retried without exposing the
// error message. The full error (status code, title, message) is still logged
// by the retry warning itself, so retry_reason is the label and nothing more.
// A nil error yields "" so a log line simply omits the field.
func providerRetryReason(err *fantasy.ProviderError) string {
	if err == nil {
		return ""
	}
	switch {
	case err.StatusCode == 401 || err.StatusCode == 403:
		return "auth"
	case err.StatusCode == 429:
		return "rate_limited"
	case err.StatusCode >= 500:
		return "server_error"
	case err.StatusCode >= 400:
		return fmt.Sprintf("client_%d", err.StatusCode)
	default:
		return "provider_error"
	}
}

// newTurnID mints the turn's correlation id. A turn is exactly one runTurn
// (one accepted prompt -> N streaming steps), so the id is generated once at
// construction and shared by every provider request the turn makes. It is a
// fresh UUID per turn so consecutive turns under the same run_id (the
// queued-prompt case) stay separable, and so a turn is addressable even when
// its run_id is empty.
func newTurnID() string {
	return uuid.NewString()
}
