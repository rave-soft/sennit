package agent

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/openrouter"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/session"
)

const (
	largeContextWindowThreshold = 200_000
	largeContextWindowBuffer    = 20_000
	smallContextWindowRatio     = 0.2
	// summarizeOutputHeadroom is the extra slack kept on top of a model's
	// own maximum reply, for the summarize request's system prompt and
	// for the usual drift between what a provider counts and what we
	// counted a step ago.
	summarizeOutputHeadroom = 0.25
)

// summarizePolicy is what a turn was told about summarizing itself: the
// switch that turns it off entirely, and the ceiling a session may work up
// to when the model's own window is more room than anyone wants to pay
// for. Both come from config (see config.Options), captured with the rest
// of the runtime so a reload cannot change the answer mid-turn.
type summarizePolicy struct {
	disabled bool
	at       int64
}

// window returns the context window this policy wants a session judged
// against: the model's own, or the configured ceiling when that is lower.
// A ceiling at or above the model's window is not a ceiling, and one on a
// model with no declared window (0) has nothing to cap — the caller skips
// auto-summarize there anyway, and inventing a window would make it
// summarize a local model on its first step.
func (p summarizePolicy) window(modelWindow int64) int64 {
	if modelWindow <= 0 || p.at <= 0 || p.at >= modelWindow {
		return modelWindow
	}
	return p.at
}

// summarizeBuffer is how much of the context window must stay free before
// a turn stops to summarize.
//
// The base figure keeps the original intent: a flat 20k for large windows,
// so a million-token window is not squandered on headroom, and a fifth of
// a small one, where a flat number would be either useless or ruinous.
//
// What the base alone gets wrong is that the turn which summarizes has to
// fit its own reply in what is left. A model configured to write up to
// 32k tokens cannot produce a summary into 20k of remaining window, so
// summarization would trip only to fail — and the flat buffer applies to
// every window above 200k, which is exactly where generous max-reply
// settings live. Observed on a 262k local model with a 32k reply limit:
// the buffer (20k) was smaller than a single reply, so the pass had
// nowhere to write. Hence the floor at the model's own reply size plus
// slack.
//
// Capped at half the window so a model configured with an outsized reply
// limit relative to its context does not summarize on its first step.
// maxOut of 0 (unknown) leaves the base untouched.
func summarizeBuffer(contextWindow, maxOut int64) int64 {
	buffer := int64(largeContextWindowBuffer)
	if contextWindow <= largeContextWindowThreshold {
		buffer = int64(float64(contextWindow) * smallContextWindowRatio)
	}
	if need := maxOut + int64(float64(maxOut)*summarizeOutputHeadroom); need > buffer {
		buffer = need
	}
	if half := contextWindow / 2; buffer > half {
		buffer = half
	}
	return buffer
}

//go:embed templates/summary.md
var summaryPrompt []byte

func (a *sessionAgent) Summarize(ctx context.Context, sessionID string, opts fantasy.ProviderOptions, onAuthRefresh func(context.Context, *fantasy.ProviderError) error) error {
	return a.summarize(ctx, sessionID, opts, onAuthRefresh, nil, a.model.Get(), a.systemPromptPrefix.Get(), nil, nil)
}

// claim, when non-nil, is the caller's own still-installed active-run slot
// (e.g. finishTurn's shouldSummarize path): summarize takes it over with a
// single atomic swap instead of releasing it and re-claiming from scratch,
// closing the window in which a queued continuation could claim the
// session first and turn a successful turn's summarize into ErrSessionBusy.
// A nil claim falls back to the normal claim-if-idle check, used by callers
// (Summarize, and coordinator's explicit trigger) that never held the slot
// themselves.
func (a *sessionAgent) summarize(ctx context.Context, sessionID string, opts fantasy.ProviderOptions, onAuthRefresh func(context.Context, *fantasy.ProviderError) error, onRateLimit func(context.Context, *fantasy.ProviderError) error, model Model, systemPromptPrefix string, active *activeRuntime, claim *activeCancel) (retErr error) {
	s, release := a.session(sessionID)
	defer release()
	genCtx, cancel := context.WithCancel(ctx)
	ac := &activeCancel{cancel: cancel}
	if err := a.claimSummarizeSlot(s, sessionID, ac, claim); err != nil {
		cancel()
		return err
	}
	defer a.finishSummarizeSlot(ctx, sessionID, ac, claim, &retErr)

	currentSession, aiMsgs, ok, err := a.summarizeMessages(ctx, sessionID, model)
	if err != nil {
		return err
	}
	if !ok {
		// Nothing to summarize.
		return nil
	}

	defer func() {
		if flushErr := a.messages.FlushAll(ctx); flushErr != nil {
			slog.Error("Failed to flush pending message updates after summarize", "error", flushErr)
		}
	}()

	summaryMessage, resp, err := a.streamSummary(genCtx, ctx, sessionID, model, systemPromptPrefix, active, onAuthRefresh, onRateLimit, aiMsgs, opts, currentSession.Todos)
	if err != nil {
		return err
	}

	return a.persistSummaryResult(genCtx, model, &currentSession, &summaryMessage, resp)
}

// claimSummarizeSlot installs ac as sessionID's active-run slot. When claim
// is non-nil (finishTurn's shouldSummarize path), it takes over that
// already-installed slot with a single atomic swap instead of releasing it
// and re-claiming from scratch, closing the window in which a queued
// continuation could claim the session first and turn a successful turn's
// summarize into ErrSessionBusy. A nil claim falls back to the normal
// claim-if-idle check, used by callers (Summarize, and coordinator's
// explicit trigger) that never held the slot themselves.
func (a *sessionAgent) claimSummarizeSlot(s *sessionState, sessionID string, ac, claim *activeCancel) error {
	if claim != nil {
		if !a.swapActive(sessionID, claim, ac) {
			return ErrSessionBusy
		}
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil {
		return ErrSessionBusy
	}
	s.active = ac
	return nil
}

// finishSummarizeSlot is summarize's deferred cleanup: it releases the
// active-run slot this call claimed, wakes any completion parked in the
// inbox while summarize held it, and hands off to whatever ran got queued
// behind this summarize - the same responsibilities runTurn's own defer
// carries for an ordinary turn.
func (a *sessionAgent) finishSummarizeSlot(ctx context.Context, sessionID string, ac, claim *activeCancel, retErr *error) {
	a.clearActiveIfMatch(sessionID, ac)
	ac.cancel()

	// A completion can land in the inbox while this summarize holds
	// the active slot - wakeEligibleLocked requires s.active == nil,
	// so DeliverTaskCompletion sees wakeEligible=false and the
	// caller's own continuation attempt is dropped (SteerDropped).
	// runTurn's own defer is the ordinary place this gets retried
	// from, but that defer never runs here: this call *is* the
	// active slot's owner for as long as the summary takes, and
	// finishTurn's shouldSummarize path (claim != nil) calls this
	// instead of clearing the slot itself. Without this call, a
	// parked delegation session - most idle-sweep summarize
	// candidates are exactly that, see markActivity below - would
	// sit at StatusRunning until the watchdog notices, rather than
	// picking the completion up the moment this summary finishes.
	// Detached from ctx: this defer's own cancel() above ends
	// genCtx, and callers on the finishTurn path hand in a ctx tied
	// to the very turn that is winding down here.
	a.wakeFromInboxIfIdle(context.WithoutCancel(ctx), sessionID)

	// The queue handoff belongs to a summarize that owns the whole
	// dispatch — the caller asked for a summary and nothing else.
	// With claim != nil this runs *inside* finishTurn, before its
	// turn has published AgentFinished: the nested Run would execute
	// a queued follow-up too early, and its error would replace the
	// result of the outer turn that had in fact succeeded. finishTurn
	// does its own handoff, at the point it is finished.
	if claim != nil {
		return
	}

	_, next, canceledRunIDDrops := a.drainNext(sessionID)
	a.publishCanceledQueueDrops(canceledRunIDDrops)
	if next == nil {
		return
	}
	_, handoffErr := a.Run(context.WithoutCancel(ctx), *next)
	if *retErr == nil {
		*retErr = handoffErr
	}
}

// summarizeMessages loads sessionID's session and converts its history into
// the fantasy messages the summarization request sends to the model. ok is
// false when there is nothing to summarize (an empty history), which is not
// an error.
func (a *sessionAgent) summarizeMessages(ctx context.Context, sessionID string, model Model) (currentSession session.Session, aiMsgs []fantasy.Message, ok bool, err error) {
	currentSession, err = a.sessions.Get(ctx, sessionID)
	if err != nil {
		return session.Session{}, nil, false, fmt.Errorf("failed to get session: %w", err)
	}
	msgs, err := a.getSessionMessages(ctx, currentSession)
	if err != nil {
		return session.Session{}, nil, false, err
	}
	if len(msgs) == 0 {
		return session.Session{}, nil, false, nil
	}

	aiMsgs, _ = a.preparePrompt(msgs, model.CatalogCfg.SupportsImages, currentSession.Todos, nil,
		withRepairSessionID(sessionID, RunIDFromContext(ctx)),
		withRepairOrigins(originSlice(msgs, originSummary)),
	)
	return currentSession, aiMsgs, true, nil
}

// streamSummary issues the summarization provider request: it builds the
// summary agent, creates the summary message that streaming deltas write
// into, and runs fantasy.Agent.Stream against it. On a stream error the
// summary message is already finished with the error (or deleted, if the
// error was a cancel) by the time this returns, so the caller has nothing
// left to clean up on that path - it can just propagate err.
func (a *sessionAgent) streamSummary(genCtx, ctx context.Context, sessionID string, model Model, systemPromptPrefix string, active *activeRuntime, onAuthRefresh func(context.Context, *fantasy.ProviderError) error, onRateLimit func(context.Context, *fantasy.ProviderError) error, aiMsgs []fantasy.Message, opts fantasy.ProviderOptions, todos []session.Todo) (message.Message, *fantasy.AgentResult, error) {
	agent := fantasy.NewAgent(
		model.Model,
		fantasy.WithSystemPrompt(string(summaryPrompt)),
		fantasy.WithUserAgent(userAgent),
	)
	summaryMessage, err := a.messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:             message.Assistant,
		Model:            model.ModelCfg.Model,
		Provider:         model.ModelCfg.Provider,
		IsSummaryMessage: true,
	})
	if err != nil {
		return message.Message{}, nil, err
	}

	summaryPromptText := buildSummaryPrompt(todos)

	// The summarize pass is a real provider request of its own, so it gets
	// its own started/finished pair - not folded into the turn's request
	// count, which is the whole point of the request_reason distinction (a
	// summarize is an extra request a run_id's log must show as a cause, not
	// as an ordinary tool-continuation step). The turn id is minted per pass
	// so a session that summarizes several times stays separable, and the run
	// id is read from the context the pass was started under (finishTurn's
	// shouldSummarize path carries the turn's). The pair is logged by the
	// instrumented model returned from the ModelProvider below - the same
	// mechanism the turn uses - so a summarize retry produces its own 1:1
	// pair rather than an orphaned started.
	//
	// The request_reason for the summarize's attempts is tracked the same way
	// the turn tracks it: the first attempt is a summary, a retry is a retry
	// (a transient failure fantasy chose to re-attempt), and a re-attempt made
	// after a successful OnAuthRefresh is an auth_refresh. All of this state
	// is read/written on the stream's own goroutine, in the retry loop's order
	// (OnRetry / OnAuthRefresh fire before the next attempt's ModelProvider),
	// so no lock is needed - the same guarantee the turn's runTurn relies on.
	summaryTurnID := newTurnID()
	summaryRunID := RunIDFromContext(ctx)
	summaryAttempt := 0
	summaryPendingReason := "" // "" | reasonRetry | reasonAuthRefresh, for the next attempt
	// Preserve nil: fantasy treats a configured callback as permission to
	// restart a failed auth request. A synthetic wrapper around nil would turn
	// a 401 into a false successful refresh and issue an unintended retry.
	var summaryOnAuthRefresh fantasy.OnAuthRefreshFunc
	if onAuthRefresh != nil {
		summaryOnAuthRefresh = func(ctx context.Context, err *fantasy.ProviderError) error {
			if refreshErr := onAuthRefresh(ctx, err); refreshErr != nil {
				return refreshErr
			}
			summaryPendingReason = reasonAuthRefresh
			return nil
		}
	}

	// Same nil discipline as OnAuthRefresh above, and the same reason:
	// fantasy reads a configured callback as permission to retry the
	// rate-limited attempt immediately, skipping its backoff. A synthetic
	// wrapper around nil would claim a rotation happened when none did.
	//
	// A summary is the single most expensive request a session makes, and
	// it only happens when the context is already full - the worst moment
	// to lose the whole pass to a 429 that a spare account would have
	// absorbed. The parent turn has rotated on this since rotation
	// existed; this pass, and a delegation's, did not.
	var summaryOnRateLimit fantasy.OnRateLimitFunc
	if onRateLimit != nil {
		summaryOnRateLimit = func(ctx context.Context, err *fantasy.ProviderError) error {
			if rotateErr := onRateLimit(ctx, err); rotateErr != nil {
				return rotateErr
			}
			summaryPendingReason = reasonAccountRotated
			return nil
		}
	}

	resp, err := agent.Stream(genCtx, fantasy.AgentStreamCall{
		Prompt:          summaryPromptText,
		Messages:        aiMsgs,
		Headers:         sessionHeaders(sessionID),
		ProviderOptions: opts,
		OnAuthRefresh:   summaryOnAuthRefresh,
		OnRateLimit:     summaryOnRateLimit,
		OnRetry: func(err *fantasy.ProviderError, _ time.Duration) {
			// A transient failure the retry loop is about to re-attempt: the
			// next attempt is a retry, not a fresh summary.
			summaryPendingReason = reasonRetry
		},
		ModelProvider: func() fantasy.LanguageModel {
			// Count the attempt and label the summarize's request. Each
			// ModelProvider call is one summarize attempt, so a retry gets its
			// own pair (attempt 2, 3, ...). The reason is a pending re-attempt
			// cause (retry / auth_refresh) when set, otherwise the baseline
			// summary cause for the pass's first attempt.
			summaryAttempt++
			reason := reasonSummary
			if summaryPendingReason != "" {
				reason = summaryPendingReason
				summaryPendingReason = ""
			}
			corr := providerCorrelation{
				sessionID: sessionID,
				runID:     summaryRunID,
				turnID:    summaryTurnID,
				step:      0,
				attempt:   summaryAttempt,
				reason:    reason,
			}
			if active != nil {
				if activeRuntime := active.load(); activeRuntime != nil && activeRuntime.model.ModelCfg.Provider == model.ModelCfg.Provider && activeRuntime.model.ModelCfg.Model == model.ModelCfg.Model {
					return newInstrumentedModel(activeRuntime.model.Model, corr, activeRuntime.model.ModelCfg.Provider)
				}
			}
			return newInstrumentedModel(model.Model, corr, model.ModelCfg.Provider)
		},
		PrepareStep: func(callContext context.Context, options fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
			prepared.Messages = options.Messages
			if systemPromptPrefix != "" {
				prepared.Messages = append([]fantasy.Message{fantasy.NewSystemMessage(systemPromptPrefix)}, prepared.Messages...)
			}
			return callContext, prepared, nil
		},
		OnReasoningDelta: func(id string, text string) error {
			summaryMessage.AppendReasoningContent(text, time.Now().Unix())
			return a.messages.Update(genCtx, summaryMessage)
		},
		OnReasoningEnd: func(id string, reasoning fantasy.ReasoningContent) error {
			// Handle anthropic signature.
			if anthropicData, ok := reasoning.ProviderMetadata["anthropic"]; ok {
				if signature, ok := anthropicData.(*anthropic.ReasoningOptionMetadata); ok && signature.Signature != "" {
					summaryMessage.AppendReasoningSignature(signature.Signature)
				}
			}
			summaryMessage.FinishThinking(time.Now().Unix())
			return a.messages.Update(genCtx, summaryMessage)
		},
		OnTextDelta: func(id, text string) error {
			summaryMessage.AppendContent(text)
			return a.messages.Update(genCtx, summaryMessage)
		},
	})
	if err != nil {
		return a.handleSummarizeStreamError(ctx, sessionID, summaryMessage, err)
	}
	return summaryMessage, resp, nil
}

// handleSummarizeStreamError finishes summaryMessage after a failed stream.
// A cancel deletes it outright: the cancellation is what this returns, not
// the delete's own error, because returning the delete result (nil on the
// ordinary path) would report a cancelled summarize as a successful one,
// and finishTurn would go on to requeue a continuation on the very
// un-summarized context that made summarizing necessary in the first
// place. Any other error marks the message finished with it, so the UI
// stops spinning.
//
// ctx is canceled on this path (that's exactly what got us here), so
// cleanup writes need a context detached from it the same way
// handleStreamError detaches cleanupCtx from t.ctx: otherwise the
// delete/update below fails immediately, leaving an empty summary message
// behind forever.
func (a *sessionAgent) handleSummarizeStreamError(ctx context.Context, sessionID string, summaryMessage message.Message, err error) (message.Message, *fantasy.AgentResult, error) {
	isCancelErr := errors.Is(err, context.Canceled)
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cleanupCancel()
	if isCancelErr {
		// User cancelled summarize we need to remove the summary message.
		if deleteErr := a.messages.Delete(cleanupCtx, summaryMessage.ID); deleteErr != nil {
			slog.Error("Failed to remove the summary message of a cancelled summarize",
				"session_id", sessionID, "message_id", summaryMessage.ID, "error", deleteErr)
		}
		return summaryMessage, nil, err
	}
	summaryMessage.AddFinish(message.FinishReasonError, time.Now().Unix(), "Summarization Error", err.Error())
	if updateErr := a.messages.Update(cleanupCtx, summaryMessage); updateErr != nil {
		return summaryMessage, nil, updateErr
	}
	return summaryMessage, nil, err
}

// persistSummaryResult records what the summarize call produced: the
// summary message is finished and persisted before its token counts are
// read back off it, so one write persists the numbers and one pubsub event
// (published by the Update above) carries them to a chat already rendering
// the row. "Before" is the summarize request's own prompt — the whole
// conversation, as the provider counted it — rather than the session's
// counters, which the next turn overwrites; "after" is the summary that
// replaces it, reusing the same fallback the session's own completion
// count uses when a provider reports no usage.
func (a *sessionAgent) persistSummaryResult(ctx context.Context, model Model, currentSession *session.Session, summaryMessage *message.Message, resp *fantasy.AgentResult) error {
	summaryMessage.SummaryBeforeTokens = promptTokensOf(resp.Response.Usage)
	summaryMessage.SummaryAfterTokens = summaryCompletionTokens(resp.Response.Usage, *summaryMessage)
	summaryMessage.AddFinish(message.FinishReasonEndTurn, time.Now().Unix(), "", "")
	if err := a.messages.Update(ctx, *summaryMessage); err != nil {
		return err
	}

	// The summarize request's "finished" line is logged by the instrumented
	// model that performed the Stream (the 1:1 counterpart of the started
	// line, carrying finish_reason + usage + the summarize's cache
	// read/creation). We do not log a second finished line here - that was
	// the duplicate the audit caught.

	costDelta := a.updateSessionUsage(model, currentSession, resp.TotalUsage, a.openrouterTotal(resp.Steps), false)

	// Just in case, get just the last usage info.
	usage := resp.Response.Usage
	currentSession.SummaryMessageID = summaryMessage.ID
	currentSession.CompletionTokens = summaryCompletionTokens(usage, *summaryMessage)
	currentSession.PromptTokens = 0
	currentSession.EstimatedUsage = usageIsZero(usage)
	// SaveUsage, not Save: this pass's Get happened before an entire
	// provider stream ran, long enough for another writer (an async
	// title-generation save against this same session, say) to have
	// landed its own cost write in between. Save would write back
	// currentSession.Cost - a total computed from the stale read - and
	// silently erase that write; SaveUsage instead folds costDelta onto
	// whatever cost is there now, in one atomic UPDATE.
	_, err := a.sessions.SaveUsage(ctx, *currentSession, costDelta)
	return err
}

// buildSummaryPrompt constructs the prompt text for session summarization.
func buildSummaryPrompt(todos []session.Todo) string {
	var sb strings.Builder
	// "Detailed" used to sit here and fight the system prompt's own
	// opening line ("Compression is the point. A summary that restates
	// the conversation has failed") - the model follows the later,
	// more specific instruction, so a request phrased as this one used
	// to be produced exactly the bloated summary the template warns
	// against.
	sb.WriteString("Summarize our conversation above, following the instructions in the system prompt above.")
	if len(todos) > 0 {
		sb.WriteString("\n\n## Current Todo List\n\n")
		for _, t := range todos {
			fmt.Fprintf(&sb, "- [%s] %s\n", t.Status, t.Content)
		}
		sb.WriteString("\nInclude these tasks and their statuses in your summary. ")
		sb.WriteString("Instruct the resuming assistant to use the `todos` tool to continue tracking progress on these tasks.")
	}
	return sb.String()
}

func (a *sessionAgent) getSessionMessages(ctx context.Context, session session.Session) ([]message.Message, error) {
	msgs, err := a.messages.List(ctx, session.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to list messages: %w", err)
	}
	return trimToSummary(session, msgs), nil
}

// trimToSummary drops everything a session has already summarized away,
// leaving the summary itself as the leading user message. Shared by the
// live session's own history and by carried-over history from a named
// sub-agent's earlier sessions, so a summarized past turn is replayed
// the same way whichever side reads it.
func trimToSummary(session session.Session, msgs []message.Message) []message.Message {
	if session.SummaryMessageID == "" {
		return msgs
	}
	summaryMsgIndex := -1
	for i, msg := range msgs {
		if msg.ID == session.SummaryMessageID {
			summaryMsgIndex = i
			break
		}
	}
	if summaryMsgIndex == -1 {
		return msgs
	}
	msgs = msgs[summaryMsgIndex:]
	msgs[0].Role = message.User
	return msgs
}

func (a *sessionAgent) openrouterCost(metadata fantasy.ProviderMetadata) *float64 {
	openrouterMetadata, ok := metadata[openrouter.Name]
	if !ok {
		return nil
	}

	opts, ok := openrouterMetadata.(*openrouter.ProviderMetadata)
	if !ok {
		return nil
	}
	return &opts.Usage.Cost
}

// updateSessionUsage folds usage into session in place (cost accumulated,
// token counters set to the request's own values - see
// updateSessionTokenCounters) and returns the cost delta this call added,
// for a caller whose write-back cannot simply persist session.Cost as a
// whole total (see SaveUsage).
func (a *sessionAgent) updateSessionUsage(model Model, session *session.Session, usage fantasy.Usage, overrideCost *float64, estimated bool) float64 {
	if !usageIsZero(usage) {
		session.EstimatedUsage = estimated
	}

	cost := catalogCost(model, usage)

	if estimated {
		cost = 0
	} else {
		// Use override cost if available (e.g., from OpenRouter).
		if overrideCost != nil {
			cost = *overrideCost
		}

		// Skip cost accumulation
		if model.FlatRate {
			cost = 0
		}
	}

	session.Cost += cost
	updateSessionTokenCounters(session, usage)
	return cost
}

func updateSessionTokenCounters(session *session.Session, usage fantasy.Usage) {
	if usage.OutputTokens != 0 {
		session.CompletionTokens = usage.OutputTokens
	}
	if promptTokens := promptTokensOf(usage); promptTokens != 0 {
		session.PromptTokens = promptTokens
	}
}

// promptTokensOf is what a turn counts as its prompt: the tokens the model
// read, whether they came fresh or from the cache.
//
// It exists because two places counted this and disagreed — the per-turn
// path added CacheReadTokens, and title generation added
// CacheCreationTokens instead, so a session's token count depended on
// which of the two wrote last.
func promptTokensOf(usage fantasy.Usage) int64 {
	return usage.InputTokens + usage.CacheReadTokens
}

// catalogCost is the model's own price for usage, before any provider
// override or flat-rate rule is applied.
func catalogCost(model Model, usage fantasy.Usage) float64 {
	cfg := model.CatalogCfg
	return cfg.CostPer1MInCached/1e6*float64(usage.CacheCreationTokens) +
		cfg.CostPer1MOutCached/1e6*float64(usage.CacheReadTokens) +
		cfg.CostPer1MIn/1e6*float64(usage.InputTokens) +
		cfg.CostPer1MOut/1e6*float64(usage.OutputTokens)
}

// openrouterTotal sums the per-step costs OpenRouter reports, or returns
// nil when no step carried one — in which case the caller keeps the
// catalog price.
func (a *sessionAgent) openrouterTotal(steps []fantasy.StepResult) *float64 {
	var total *float64
	for _, step := range steps {
		stepCost := a.openrouterCost(step.ProviderMetadata)
		if stepCost == nil {
			continue
		}
		sum := *stepCost
		if total != nil {
			sum += *total
		}
		total = &sum
	}
	return total
}

func summaryCompletionTokens(usage fantasy.Usage, summaryMessage message.Message) int64 {
	if usage.OutputTokens != 0 {
		return usage.OutputTokens
	}
	return approxTokenCount(summaryMessage.Content().Text) + approxTokenCount(summaryMessage.ReasoningContent().String())
}
