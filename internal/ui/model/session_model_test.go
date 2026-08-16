package model

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// TestLastAssistantModel: a session can span a model switch, so the model
// that answered most recently is the one describing it.
func TestLastAssistantModel(t *testing.T) {
	t.Parallel()

	ref := lastAssistantModel([]message.Message{
		{Role: message.Assistant, Provider: "openai", Model: "gpt-5.6"},
		{Role: message.User},
		{Role: message.Assistant, Provider: "anthropic", Model: "claude-sonnet-5"},
		{Role: message.User},
	})
	require.Equal(t, "anthropic/claude-sonnet-5", ref.String())

	// Nothing has answered yet, and nothing is claimed.
	require.Empty(t, lastAssistantModel([]message.Message{{Role: message.User}}).String())
	require.Empty(t, lastAssistantModel(nil).String())
}

// TestViewedModelPrefersTheChildSessionsOwnModel: drilling into a sub-agent
// shows that session's context, so it must show that session's model too —
// a delegation often runs on a model the picker isn't pointing at.
func TestViewedModelPrefersTheChildSessionsOwnModel(t *testing.T) {
	u := newCursorTestUI(t)
	u.wsCache.agentReady = true
	u.wsCache.agentModel = workspace.AgentModel{
		CatalogCfg: catwalk.Model{ID: "gpt-5.6-sol", Name: "GPT-5.6-Sol"},
		ModelCfg:   config.SelectedModel{Provider: "codex", Model: "gpt-5.6-sol"},
	}
	u.sess.modelUsed = sessionModelRef{provider: "anthropic", model: "claude-haiku-4-5"}

	// Top-level session: the sidebar answers "what will the next prompt
	// run on", which is the selection.
	require.Equal(t, "gpt-5.6-sol", u.viewedModel().ModelCfg.Model)

	u.sess.navStack = []sessionNavFrame{{parentSessionID: "main", effort: "low"}}
	viewed := u.viewedModel()
	require.Equal(t, "anthropic", viewed.ModelCfg.Provider)
	require.Equal(t, "claude-haiku-4-5", viewed.ModelCfg.Model)
	require.Equal(t, "low", viewed.ModelCfg.ReasoningEffort, "the delegation's effort is the one those messages ran at")
	require.Equal(t, "claude-haiku-4-5", viewed.CatalogCfg.Name, "an unknown model is still named by its id")
}

// TestSidebarShowsTheChildSessionsModel is the user-visible half of the
// above: the sidebar line itself, not just the resolver behind it.
func TestSidebarShowsTheChildSessionsModel(t *testing.T) {
	u := newCursorTestUI(t)
	u.wsCache.agentReady = true
	u.wsCache.agentModel = workspace.AgentModel{
		CatalogCfg: catwalk.Model{ID: "gpt-5.6-sol", Name: "GPT-5.6-Sol"},
		ModelCfg:   config.SelectedModel{Provider: "codex", Model: "gpt-5.6-sol"},
	}
	u.sess.navStack = []sessionNavFrame{{parentSessionID: "main"}}
	u.sess.modelUsed = sessionModelRef{provider: "anthropic", model: "claude-haiku-4-5"}

	rendered := ansi.Strip(u.modelInfo(60))
	require.Contains(t, rendered, "claude-haiku-4-5")
	require.NotContains(t, rendered, "GPT-5.6-Sol")
}

// TestRecordAssistantModelTracksALiveDelegation: a delegation that starts
// answering while it is on screen must stop being described by whatever
// the previously loaded session used.
func TestRecordAssistantModelTracksALiveDelegation(t *testing.T) {
	u := newCursorTestUI(t)
	u.sess.modelUsed = sessionModelRef{provider: "openai", model: "gpt-5.6"}

	u.recordAssistantModel(message.Message{Role: message.User, Provider: "x", Model: "y"})
	require.Equal(t, "openai/gpt-5.6", u.sess.modelUsed.String(), "a user message says nothing about the model")

	u.recordAssistantModel(message.Message{Role: message.Assistant, Provider: "anthropic", Model: "claude-sonnet-5"})
	require.Equal(t, "anthropic/claude-sonnet-5", u.sess.modelUsed.String())
}

// TestChildPanelNamesTheInheritedModel: a delegation with no override of
// its own used to render "default model", which answers a different
// question than the one being asked — reading back a delegation from
// history, "which model was this?" is the whole point.
func TestChildPanelNamesTheInheritedModel(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	u.sess.navStack[len(u.sess.navStack)-1].model = ""
	u.sess.navStack[len(u.sess.navStack)-1].effort = ""
	u.sess.modelUsed = sessionModelRef{provider: "anthropic", model: "claude-haiku-4-5"}

	scr := uv.NewScreenBuffer(u.lay.width, u.lay.height)
	u.drawChildSessionPanel(scr, u.lay.layout.editor)
	out := ansi.Strip(scr.Render())

	require.Contains(t, out, "anthropic/claude-haiku-4-5")
	require.NotContains(t, out, "default model")
}
