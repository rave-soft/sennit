package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestThemePreviewState_LiveFallsBackToConfigured(t *testing.T) {
	t.Parallel()

	var state themePreviewState
	require.Equal(t, "configured", state.live("configured"))

	state.setLive("applied")
	require.Equal(t, "applied", state.live("configured"))
}

func TestThemePreviewState_PreviewKeepsFirstRestorePoint(t *testing.T) {
	t.Parallel()

	var state themePreviewState
	require.True(t, state.preview("first-preview", "configured"))

	state.setLive("first-preview")
	require.True(t, state.preview("second-preview", "configured"))

	state.setLive("second-preview")
	restore, needed := state.cancel("second-preview")
	require.True(t, needed)
	require.Equal(t, "configured", restore)

	restore, needed = state.cancel("second-preview")
	require.False(t, needed)
	require.Empty(t, restore)
}

func TestThemePreviewState_PreviewAndCancelNoOps(t *testing.T) {
	t.Parallel()

	var state themePreviewState
	require.False(t, state.preview("configured", "configured"))

	restore, needed := state.cancel("configured")
	require.False(t, needed)
	require.Empty(t, restore)
}

func TestThemePreviewState_ConfirmClearsRestorePoint(t *testing.T) {
	t.Parallel()

	var state themePreviewState
	require.True(t, state.preview("preview", "configured"))
	state.setLive("preview")
	state.confirm()

	restore, needed := state.cancel("preview")
	require.False(t, needed)
	require.Empty(t, restore)
	require.Equal(t, "preview", state.live("configured"))
}
