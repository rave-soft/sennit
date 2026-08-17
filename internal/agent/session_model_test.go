package agent

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/stretchr/testify/require"
)

// modelPinTestAgent builds a DB-backed agent that runs on modelCfg, which
// is the part testSessionAgent leaves zero: the fake language model is
// what serves the turn, but the SelectedModel is what gets recorded.
func modelPinTestAgent(env fakeEnv, modelCfg config.SelectedModel) SessionAgent {
	return NewSessionAgent(SessionAgentOptions{
		Model: Model{
			Model: newRecordingModel("fake", "fake-model"),
			CatalogCfg: catwalk.Model{
				ContextWindow:    200000,
				DefaultMaxTokens: 10000,
			},
			ModelCfg: modelCfg,
		},
		SystemPrompt: "system",
		Sessions:     env.sessions,
		Messages:     env.messages,
	})
}

// A turn records the model it ran on onto its session, which is what
// makes restoring the session able to put that model back rather than
// resuming it on whatever the instance happens to have selected later.
func TestRun_RecordsTheModelTheSessionRanOn(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	pinned := config.SelectedModel{Provider: "anthropic", Model: "claude-opus-5"}
	sa := modelPinTestAgent(env, pinned)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	require.True(t, sess.Model.IsZero(), "a session that has not run yet pins nothing")

	_, err = sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "hello"})
	require.NoError(t, err)

	got, err := env.sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, session.ModelRef{Provider: "anthropic", Model: "claude-opus-5"}, got.Model)
}

// An agent with no SelectedModel of its own must leave an existing pin
// alone rather than clear it. Several agents in this package run without
// one (a sub-agent built from a bare Model, the title agent), and writing
// their empty config through would erase the record of what the session
// actually runs on — the exact thing the pin exists to keep.
func TestRun_AgentWithoutSelectedModelLeavesThePinAlone(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	pinned := session.ModelRef{Provider: "anthropic", Model: "claude-opus-5"}
	require.NoError(t, env.sessions.SetModel(t.Context(), sess.ID, pinned))

	sa := modelPinTestAgent(env, config.SelectedModel{})
	_, err = sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "hello"})
	require.NoError(t, err)

	got, err := env.sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, pinned, got.Model, "an agent with no model of its own must not clear the pin")
}
