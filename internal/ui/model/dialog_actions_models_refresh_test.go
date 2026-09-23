package model

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// TestApplyProviderDialogAction_RefreshModelsRunsOffThread proves the dialog
// action defers workspace IO until Bubble Tea runs its command.
func TestApplyProviderDialogAction_RefreshModelsRunsOffThread(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{
		refreshModelsResults: []workspace.ModelRefreshResult{{ID: "custom", Added: 2}},
	}
	m := newCmdDrivenUI(ws)

	cmd, handled := m.applyProviderDialogAction(dialog.ActionRefreshModels{ProviderID: "custom"})
	require.True(t, handled)
	require.NotNil(t, cmd)
	require.Zero(t, ws.refreshModelsCalls, "refresh must not run synchronously")

	msg := findAccountsMsg(t, cmd, func(msg tea.Msg) bool {
		_, ok := msg.(dialog.ActionRefreshModelsResult)
		return ok
	})
	result, ok := msg.(dialog.ActionRefreshModelsResult)
	require.True(t, ok, "expected ActionRefreshModelsResult, got %#v", msg)
	require.NoError(t, result.Err)
	require.Equal(t, 1, ws.refreshModelsCalls)
	require.Equal(t, "custom", ws.lastRefreshProvider)
	require.Equal(t, ws.refreshModelsResults, result.Results)
}
