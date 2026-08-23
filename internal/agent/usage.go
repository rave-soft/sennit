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
	return a.summarize(ctx, sessionID, opts, onAuthRefresh, a.model.Get(), a.systemPromptPrefix.Get(), nil, nil)
}

// claim, when non-nil, is the caller's own still-installed active-run slot
// (e.g. finishTurn's shouldSummarize path): summarize takes it over with a
// single atomic swap instead of releasing it and re-claiming from scratch,
// closing the window in which a queued continuation could claim the
// session first and turn a successful turn's summarize into ErrSessionBusy.
// A nil claim falls back to the normal claim-if-idle check, used by callers
// (Summarize, and coordinator's explicit trigger) that never held the slot
// themselves.
func (a *sessionAgent) summarize(ctx context.Context, sessionID string, opts fantasy.ProviderOptions, onAuthRefresh func(context.Context, *fantasy.ProviderError) error, model Model, systemPromptPrefix string, active *activeRuntime, claim *activeCancel) (retErr error) {
	s, release := a.session(sessionID)
	defer release()
	genCtx, cancel := context.WithCancel(ctx)
	ac := &activeCancel{cancel: cancel}
	if claim != nil {
		if !a.swapActive(sessionID, claim, ac) {
			cancel()
			return ErrSessionBusy
		}
	} else {
		s.mu.Lock()
		if s.active != nil {
			s.mu.Unlock()
			cancel()
			return ErrSessionBusy
		}
		s.active = ac
		s.mu.Unlock()
	}

	defer func() {
		a.clearActiveIfMatch(sessionID, ac)
		cancel()

		_, next, canceledRunIDDrops := a.drainNext(sessionID)
		a.publishCanceledQueueDrops(canceledRunIDDrops)
		if next == nil {
			return
		}
		_, handoffErr := a.Run(context.WithoutCancel(ctx), *next)
		if retErr == nil {
			retErr = handoffErr
		}
	}()

	currentSession, err := a.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}
	msgs, err := a.getSessionMessages(ctx, currentSession)
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		// Nothing to summarize.
		return nil
	}

	aiMsgs, _ := a.preparePrompt(msgs, model.CatalogCfg.SupportsImages, currentSession.Todos)

	defer func() {
		if flushErr := a.messages.FlushAll(ctx); flushErr != nil {
			slog.Error("Failed to flush pending message updates after summarize", "error", flushErr)
		}
	}()

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
		return err
	}

	summaryPromptText := buildSummaryPrompt(currentSession.Todos)

	resp, err := agent.Stream(genCtx, fantasy.AgentStreamCall{
		Prompt:          summaryPromptText,
		Messages:        aiMsgs,
		Headers:         sessionHeaders(sessionID),
		ProviderOptions: opts,
		OnAuthRefresh:   onAuthRefresh,
		ModelProvider: func() fantasy.LanguageModel {
			if active != nil {
				if activeRuntime := active.load(); activeRuntime != nil && activeRuntime.model.ModelCfg.Provider == model.ModelCfg.Provider && activeRuntime.model.ModelCfg.Model == model.ModelCfg.Model {
					return activeRuntime.model.Model
				}
			}
			return model.Model
		},
		PrepareStep: func(callContext context.Context, options fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
			prepared.Messages = options.Messages
			if systemPromptPrefix != "" {
				prepared.Messages = append([]fantasy.Message{fantasy.NewSystemMessage(systemPromptPrefix)}, prepared.Messages...)
			}
			return callContext, prepared, nil
		},
		OnReasoningDelta: func(id string, text string) error {
			summaryMessage.AppendReasoningContent(text)
			return a.messages.Update(genCtx, summaryMessage)
		},
		OnReasoningEnd: func(id string, reasoning fantasy.ReasoningContent) error {
			// Handle anthropic signature.
			if anthropicData, ok := reasoning.ProviderMetadata["anthropic"]; ok {
				if signature, ok := anthropicData.(*anthropic.ReasoningOptionMetadata); ok && signature.Signature != "" {
					summaryMessage.AppendReasoningSignature(signature.Signature)
				}
			}
			summaryMessage.FinishThinking()
			return a.messages.Update(genCtx, summaryMessage)
		},
		OnTextDelta: func(id, text string) error {
			summaryMessage.AppendContent(text)
			return a.messages.Update(genCtx, summaryMessage)
		},
	})
	if err != nil {
		isCancelErr := errors.Is(err, context.Canceled)
		// ctx is canceled on this path (that's exactly what got us here),
		// so cleanup writes need a context detached from it the same way
		// handleStreamError detaches cleanupCtx from t.ctx: otherwise the
		// delete/update below fails immediately, leaving an empty summary
		// message behind forever.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cleanupCancel()
		if isCancelErr {
			// User cancelled summarize we need to remove the summary message.
			//
			// The cancellation itself is what this returns, not the
			// delete's own error: returning the delete result (nil on the
			// ordinary path) reported a cancelled summarize as a
			// successful one, and finishTurn went on to requeue a
			// continuation — on the un-summarized context that made
			// summarizing necessary in the first place, so the next turn
			// walked straight back into it. A delete failure is worth
			// logging but is not the outcome the caller has to act on.
			if deleteErr := a.messages.Delete(cleanupCtx, summaryMessage.ID); deleteErr != nil {
				slog.Error("Failed to remove the summary message of a cancelled summarize",
					"session_id", sessionID, "message_id", summaryMessage.ID, "error", deleteErr)
			}
			return err
		}
		// Mark the summary message as finished with an error so the UI
		// stops spinning.
		summaryMessage.AddFinish(message.FinishReasonError, "Summarization Error", err.Error())
		if updateErr := a.messages.Update(cleanupCtx, summaryMessage); updateErr != nil {
			return updateErr
		}
		return err
	}

	summaryMessage.AddFinish(message.FinishReasonEndTurn, "", "")
	err = a.messages.Update(genCtx, summaryMessage)
	if err != nil {
		return err
	}

	var openrouterCost *float64
	for _, step := range resp.Steps {
		stepCost := a.openrouterCost(step.ProviderMetadata)
		if stepCost != nil {
			newCost := *stepCost
			if openrouterCost != nil {
				newCost += *openrouterCost
			}
			openrouterCost = &newCost
		}
	}

	a.updateSessionUsage(model, &currentSession, resp.TotalUsage, openrouterCost, false)

	// Just in case, get just the last usage info.
	usage := resp.Response.Usage
	currentSession.SummaryMessageID = summaryMessage.ID
	currentSession.CompletionTokens = summaryCompletionTokens(usage, summaryMessage)
	currentSession.PromptTokens = 0
	currentSession.EstimatedUsage = usageIsZero(usage)
	_, err = a.sessions.Save(genCtx, currentSession)
	if err != nil {
		return err
	}

	return nil
}

// buildSummaryPrompt constructs the prompt text for session summarization.
func buildSummaryPrompt(todos []session.Todo) string {
	var sb strings.Builder
	sb.WriteString("Provide a detailed summary of our conversation above.")
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

func (a *sessionAgent) updateSessionUsage(model Model, session *session.Session, usage fantasy.Usage, overrideCost *float64, estimated bool) {
	if !usageIsZero(usage) {
		session.EstimatedUsage = estimated
	}

	modelConfig := model.CatalogCfg
	cost := modelConfig.CostPer1MInCached/1e6*float64(usage.CacheCreationTokens) +
		modelConfig.CostPer1MOutCached/1e6*float64(usage.CacheReadTokens) +
		modelConfig.CostPer1MIn/1e6*float64(usage.InputTokens) +
		modelConfig.CostPer1MOut/1e6*float64(usage.OutputTokens)

	if !estimated {
		a.eventTokensUsed(session.ID, model, usage, cost)
	}

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
}

func updateSessionTokenCounters(session *session.Session, usage fantasy.Usage) {
	if usage.OutputTokens != 0 {
		session.CompletionTokens = usage.OutputTokens
	}
	if promptTokens := usage.InputTokens + usage.CacheReadTokens; promptTokens != 0 {
		session.PromptTokens = promptTokens
	}
}

func summaryCompletionTokens(usage fantasy.Usage, summaryMessage message.Message) int64 {
	if usage.OutputTokens != 0 {
		return usage.OutputTokens
	}
	return approxTokenCount(summaryMessage.Content().Text) + approxTokenCount(summaryMessage.ReasoningContent().String())
}
