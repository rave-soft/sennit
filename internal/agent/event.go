package agent

import (
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/event"
)

func (a *sessionAgent) eventPromptSent(call SessionAgentCall) {
	event.PromptSent(
		a.eventCommon(call.SessionID, a.callModel(call))...,
	)
}

func (a *sessionAgent) eventPromptResponded(call SessionAgentCall, duration time.Duration) {
	event.PromptResponded(
		append(
			a.eventCommon(call.SessionID, a.callModel(call)),
			"prompt duration pretty", duration.String(),
			"prompt duration in seconds", int64(duration.Seconds()),
		)...,
	)
}

func (a *sessionAgent) eventTokensUsed(sessionID string, model Model, usage fantasy.Usage, cost float64) {
	event.TokensUsed(
		append(
			a.eventCommon(sessionID, model),
			"input tokens", usage.InputTokens,
			"output tokens", usage.OutputTokens,
			"cache read tokens", usage.CacheReadTokens,
			"cache creation tokens", usage.CacheCreationTokens,
			"total tokens", usage.InputTokens+usage.OutputTokens+usage.CacheReadTokens+usage.CacheCreationTokens,
			"cost", cost,
		)...,
	)
}

func (a *sessionAgent) eventCommon(sessionID string, model Model) []any {
	m := model.ModelCfg

	return []any{
		"session id", sessionID,
		"provider", m.Provider,
		"model", m.Model,
		"reasoning effort", m.ReasoningEffort,
		"thinking mode", m.Think,
	}
}
