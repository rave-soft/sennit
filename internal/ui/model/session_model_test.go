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

	ref := lastAssistantModel("sess-1", []message.Message{
		{Role: message.Assistant, Provider: "openai", Model: "gpt-5.6"},
		{Role: message.User},
		{Role: message.Assistant, Provider: "anthropic", Model: "claude-sonnet-5"},
		{Role: message.User},
	})
	require.Equal(t, "anthropic/claude-sonnet-5", ref.String())
	require.Equal(t, "sess-1", ref.sessionID, "a reading is about the session it was read from")

	// Nothing has answered yet, and nothing is claimed.
	require.Empty(t, lastAssistantModel("sess-1", []message.Message{{Role: message.User}}).String())
	require.Empty(t, lastAssistantModel("sess-1", nil).String())
}

// TestViewedModelPrefersTheChildSessionsOwnModel: drilling into a sub-agent
// shows that session's context, so it must show that session's model too —
// a delegation often runs on a model the picker isn't pointing at.
func TestViewedModelPrefersTheChildSessionsOwnModel(t *testing.T) {
	u := newCursorTestUI(t)
	u.wsCache.agentCache.value.ready = true
	u.wsCache.agentCache.value.model = workspace.AgentModel{
		CatalogCfg: catwalk.Model{ID: "gpt-5.6-sol", Name: "GPT-5.6-Sol"},
		ModelCfg:   config.SelectedModel{Provider: "codex", Model: "gpt-5.6-sol"},
	}
	u.sess.modelUsed = sessionModelRef{sessionID: "child-1", provider: "anthropic", model: "claude-haiku-4-5"}

	// Top-level session: the sidebar answers "what will the next prompt
	// run on", which is the selection.
	require.Equal(t, "gpt-5.6-sol", u.viewedModel().ModelCfg.Model)

	u.sess.navStack = []sessionNavFrame{{parentSessionID: "main", childSessionID: "child-1", effort: "low"}}
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
	u.wsCache.agentCache.value.ready = true
	u.wsCache.agentCache.value.model = workspace.AgentModel{
		CatalogCfg: catwalk.Model{ID: "gpt-5.6-sol", Name: "GPT-5.6-Sol"},
		ModelCfg:   config.SelectedModel{Provider: "codex", Model: "gpt-5.6-sol"},
	}
	u.sess.navStack = []sessionNavFrame{{parentSessionID: "main", childSessionID: "child-1"}}
	u.sess.modelUsed = sessionModelRef{sessionID: "child-1", provider: "anthropic", model: "claude-haiku-4-5"}

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

	u.recordAssistantModel(message.Message{SessionID: "child-1", Role: message.Assistant, Provider: "anthropic", Model: "claude-sonnet-5"})
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
	u.sess.modelUsed = sessionModelRef{
		sessionID: u.sess.navStack[len(u.sess.navStack)-1].childSessionID,
		provider:  "anthropic",
		model:     "claude-haiku-4-5",
	}

	scr := uv.NewScreenBuffer(u.lay.width, u.lay.height)
	u.drawChildSessionPanel(scr, u.lay.layout.editor)
	out := ansi.Strip(scr.Render())

	require.Contains(t, out, "anthropic/claude-haiku-4-5")
	require.NotContains(t, out, "default model")
}

// TestViewedModelIgnoresAnotherSessionsReading is the bug the sessionID on
// a reading exists for. Entering a delegation pushes its nav frame at once
// but loads its session asynchronously, so for as long as that takes the
// parent is still the loaded session — and still recording its own model
// against every message it streams. The sidebar showed the parent's model
// inside the delegation, and flipped between the two as messages arrived.
func TestViewedModelIgnoresAnotherSessionsReading(t *testing.T) {
	u := newCursorTestUI(t)
	u.wsCache.agentCache.value.ready = true
	u.wsCache.agentCache.value.model = workspace.AgentModel{
		CatalogCfg: catwalk.Model{ID: "gpt-5.6-sol", Name: "GPT-5.6-Sol"},
		ModelCfg:   config.SelectedModel{Provider: "codex", Model: "gpt-5.6-sol"},
	}
	u.sess.navStack = []sessionNavFrame{{
		parentSessionID: "main",
		childSessionID:  "child-1",
		model:           "codex/gpt-5.6-terra",
		effort:          "high",
	}}
	// The parent's own reading, while its child is still loading.
	u.sess.modelUsed = sessionModelRef{sessionID: "main", provider: "codex", model: "gpt-5.6-sol"}

	viewed := u.viewedModel()
	require.Equal(t, "gpt-5.6-terra", viewed.ModelCfg.Model, "the delegation runs on the model it is pinned to")
	require.Equal(t, "high", viewed.ModelCfg.ReasoningEffort)

	// And the parent going on streaming must not move it either.
	u.recordAssistantModel(message.Message{
		SessionID: "main", Role: message.Assistant, Provider: "codex", Model: "gpt-5.6-sol",
	})
	require.Equal(t, "gpt-5.6-terra", u.viewedModel().ModelCfg.Model)

	// Once the child answers, its own messages are what describe it.
	u.recordAssistantModel(message.Message{
		SessionID: "child-1", Role: message.Assistant, Provider: "codex", Model: "gpt-5.6-terra-preview",
	})
	require.Equal(t, "gpt-5.6-terra-preview", u.viewedModel().ModelCfg.Model)
}

// TestViewedModelFallsBackToTheSelectionWithoutAPin: a delegation with no
// model of its own really does inherit the app's, so naming the selection
// there is the true answer rather than a leaked one.
func TestViewedModelFallsBackToTheSelectionWithoutAPin(t *testing.T) {
	u := newCursorTestUI(t)
	u.wsCache.agentCache.value.ready = true
	u.wsCache.agentCache.value.model = workspace.AgentModel{
		CatalogCfg: catwalk.Model{ID: "gpt-5.6-sol", Name: "GPT-5.6-Sol"},
		ModelCfg:   config.SelectedModel{Provider: "codex", Model: "gpt-5.6-sol"},
	}
	u.sess.navStack = []sessionNavFrame{{parentSessionID: "main", childSessionID: "child-1"}}
	u.sess.modelUsed = sessionModelRef{sessionID: "main", provider: "anthropic", model: "claude-haiku-4-5"}

	require.Equal(t, "gpt-5.6-sol", u.viewedModel().ModelCfg.Model)
}

// TestChildPanelIgnoresAnotherSessionsReading: the delegation panel's own
// model line had the same leak as the sidebar — with no pin to name, it
// printed whatever the loaded session last recorded, which during the
// entry window is the parent's model.
func TestChildPanelIgnoresAnotherSessionsReading(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	frame := &u.sess.navStack[len(u.sess.navStack)-1]
	frame.model = ""
	frame.effort = ""
	u.sess.modelUsed = sessionModelRef{sessionID: "main", provider: "codex", model: "gpt-5.6-sol"}

	scr := uv.NewScreenBuffer(u.lay.width, u.lay.height)
	u.drawChildSessionPanel(scr, u.lay.layout.editor)
	out := ansi.Strip(scr.Render())

	require.NotContains(t, out, "gpt-5.6-sol", "that reading is the parent's, not this delegation's")
	require.Contains(t, out, "default model", "nothing is known about this one yet, and it says so")
}
