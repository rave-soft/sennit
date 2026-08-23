package agent

import (
	"context"
	"errors"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/stretchr/testify/require"
)

// modelNotEnabledModel streams a single error part whose message trips
// classifyStreamError's classModelNotEnabled classification (see
// TestClassifyStreamError), regardless of which provider is configured.
type modelNotEnabledModel struct{}

func (m *modelNotEnabledModel) Provider() string { return "fake" }
func (m *modelNotEnabledModel) Model() string    { return "fake-model" }

func (m *modelNotEnabledModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not implemented")
}

func (m *modelNotEnabledModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if isTitleCall(call) {
		return titleStream()
	}
	return func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{
			Type:  fantasy.StreamPartTypeError,
			Error: &fantasy.ProviderError{Message: "The requested model is not supported."},
		})
	}, nil
}

func (m *modelNotEnabledModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *modelNotEnabledModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

// TestRun_ModelNotEnabledClassifiedAsCopilotQuotaOnlyForCopilot is the
// regression test for turn.go's handleStreamError: a "model not enabled"
// provider error used to be unconditionally turned into a *ProviderQuotaError
// pointing at Copilot's settings page, regardless of which provider actually
// produced it. It must only happen for the Copilot provider; every other
// provider gets the generic provider-error handling instead.
func TestRun_ModelNotEnabledClassifiedAsCopilotQuotaOnlyForCopilot(t *testing.T) {
	t.Parallel()

	t.Run("copilot provider gets the typed quota error", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		sess, err := env.sessions.Create(t.Context(), "session")
		require.NoError(t, err)

		sa := NewSessionAgent(SessionAgentOptions{
			Model: Model{
				Model:      &modelNotEnabledModel{},
				CatalogCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000, Name: "gpt-5"},
				ModelCfg:   config.SelectedModel{Provider: string(catwalk.InferenceProviderCopilot)},
			},
			SystemPrompt: "system",
			Sessions:     env.sessions,
			Messages:     env.messages,
		}).(*sessionAgent)

		_, runErr := sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "hi"})
		var quotaErr *ProviderQuotaError
		require.ErrorAs(t, runErr, &quotaErr, "copilot's model-not-enabled error must classify as a quota error")
		require.Equal(t, "copilot", quotaErr.Provider)
	})

	t.Run("a non-copilot provider does not get the copilot quota treatment", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		sess, err := env.sessions.Create(t.Context(), "session")
		require.NoError(t, err)

		sa := NewSessionAgent(SessionAgentOptions{
			Model: Model{
				Model:      &modelNotEnabledModel{},
				CatalogCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000, Name: "some-model"},
				ModelCfg:   config.SelectedModel{Provider: "openai"},
			},
			SystemPrompt: "system",
			Sessions:     env.sessions,
			Messages:     env.messages,
		}).(*sessionAgent)

		_, runErr := sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "hi"})
		var quotaErr *ProviderQuotaError
		require.False(t, errors.As(runErr, &quotaErr), "a non-copilot provider must not be classified as a Copilot quota error")

		msgs, err := env.messages.List(t.Context(), sess.ID)
		require.NoError(t, err)
		var found bool
		for i := range msgs {
			if msgs[i].Role != message.Assistant {
				continue
			}
			found = true
			finish := msgs[i].FinishPart()
			require.NotNil(t, finish)
			require.NotEqual(t, "Copilot model not enabled", finish.Message)
		}
		require.True(t, found, "an assistant message must have been persisted")
	})
}
