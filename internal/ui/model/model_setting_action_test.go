package model

import (
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/stretchr/testify/require"
)

// coderModelConfig returns a config with just enough shape (a "coder" agent
// entry and a model) for ActionToggleThinking/ActionSelectReasoningEffort to
// pass their agent-lookup guard.
func coderModelConfig(model config.SelectedModel) *config.Config {
	cfg := newSettingsConfig()
	cfg.Agents = map[string]config.Agent{config.AgentCoder: {}}
	cfg.Model = model
	return cfg
}

func modelSettingResult(t *testing.T, cmd tea.Cmd) modelSettingUpdatedMsg {
	t.Helper()

	result, ok := cmd().(modelSettingUpdatedMsg)
	require.True(t, ok)
	return result
}

func TestApplySettingsDialogAction_ToggleThinking(t *testing.T) {
	t.Parallel()

	t.Run("no configuration reports an error", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(nil)

		cmd, handled := m.applySettingsDialogAction(dialog.ActionToggleThinking{})

		require.True(t, handled)
		got, ok := cmd().(util.InfoMsg)
		require.True(t, ok)
		require.Equal(t, util.InfoTypeError, got.Type)
		require.Equal(t, "configuration not found", got.Msg)
	})

	t.Run("no coder agent reports an error", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())

		cmd, handled := m.applySettingsDialogAction(dialog.ActionToggleThinking{})

		require.True(t, handled)
		got, ok := cmd().(util.InfoMsg)
		require.True(t, ok)
		require.Equal(t, util.InfoTypeError, got.Type)
		require.Equal(t, "agent configuration not found", got.Msg)
	})

	t.Run("in-flight operation warns without starting another", func(t *testing.T) {
		t.Parallel()
		m, ws := newSettingsUI(coderModelConfig(config.SelectedModel{Think: false}))
		m.modelOperation.begin()

		cmd, handled := m.applySettingsDialogAction(dialog.ActionToggleThinking{})

		require.True(t, handled)
		got, ok := cmd().(util.InfoMsg)
		require.True(t, ok)
		require.Equal(t, util.InfoTypeWarn, got.Type)
		require.Equal(t, "Model settings are already being updated", got.Msg)
		require.Empty(t, ws.preferredCalls)
	})

	t.Run("success flips Think, closes the dialog, and reports a toast", func(t *testing.T) {
		t.Parallel()
		m, ws := newSettingsUI(coderModelConfig(config.SelectedModel{Provider: "p", Model: "m", Think: false}))
		m.dialog.OpenDialog(stubIDDialog{id: dialog.CommandsID})

		cmd, handled := m.applySettingsDialogAction(dialog.ActionToggleThinking{})

		require.True(t, handled)
		require.False(t, m.dialog.ContainsDialog(dialog.CommandsID))

		result := modelSettingResult(t, cmd)
		require.NoError(t, result.Err)
		require.Equal(t, "Thinking mode enabled", result.Info)
		require.Len(t, ws.preferredCalls, 1)
		require.True(t, ws.preferredCalls[0].model.Think)
	})
}

func TestApplySettingsDialogAction_SelectReasoningEffort(t *testing.T) {
	t.Parallel()

	t.Run("agent busy warns before touching configuration", func(t *testing.T) {
		t.Parallel()
		m, ws := newSettingsUI(nil)
		m.wsCache.agentBusyCache.Set(true)

		cmd, handled := m.applySettingsDialogAction(dialog.ActionSelectReasoningEffort{Effort: "high"})

		require.True(t, handled)
		got, ok := cmd().(util.InfoMsg)
		require.True(t, ok)
		require.Equal(t, util.InfoTypeWarn, got.Type)
		require.Equal(t, "Agent is busy, please wait...", got.Msg)
		require.Empty(t, ws.preferredCalls)
	})

	t.Run("no configuration reports an error", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(nil)

		cmd, handled := m.applySettingsDialogAction(dialog.ActionSelectReasoningEffort{Effort: "high"})

		require.True(t, handled)
		got, ok := cmd().(util.InfoMsg)
		require.True(t, ok)
		require.Equal(t, util.InfoTypeError, got.Type)
		require.Equal(t, "configuration not found", got.Msg)
	})

	t.Run("in-flight operation warns without starting another", func(t *testing.T) {
		t.Parallel()
		m, ws := newSettingsUI(coderModelConfig(config.SelectedModel{}))
		m.modelOperation.begin()

		cmd, handled := m.applySettingsDialogAction(dialog.ActionSelectReasoningEffort{Effort: "high"})

		require.True(t, handled)
		got, ok := cmd().(util.InfoMsg)
		require.True(t, ok)
		require.Equal(t, util.InfoTypeWarn, got.Type)
		require.Equal(t, "Model settings are already being updated", got.Msg)
		require.Empty(t, ws.preferredCalls)
	})

	t.Run("success sets ReasoningEffort, closes the dialog, and reports a toast", func(t *testing.T) {
		t.Parallel()
		m, ws := newSettingsUI(coderModelConfig(config.SelectedModel{Provider: "p", Model: "m"}))
		m.dialog.OpenDialog(stubIDDialog{id: dialog.ReasoningID})

		cmd, handled := m.applySettingsDialogAction(dialog.ActionSelectReasoningEffort{Effort: "high"})

		require.True(t, handled)
		require.False(t, m.dialog.ContainsDialog(dialog.ReasoningID))

		result := modelSettingResult(t, cmd)
		require.NoError(t, result.Err)
		require.Equal(t, "Reasoning effort set to high", result.Info)
		require.Len(t, ws.preferredCalls, 1)
		require.Equal(t, "high", ws.preferredCalls[0].model.ReasoningEffort)
	})

	t.Run("update error surfaces without UpdateAgentModel side effect", func(t *testing.T) {
		t.Parallel()
		m, ws := newSettingsUI(coderModelConfig(config.SelectedModel{Provider: "p", Model: "m"}))
		ws.updatePreferredModelErr = errors.New("update failed")

		cmd, handled := m.applySettingsDialogAction(dialog.ActionSelectReasoningEffort{Effort: "medium"})

		require.True(t, handled)
		result := modelSettingResult(t, cmd)
		require.EqualError(t, result.Err, "update failed")
	})
}
