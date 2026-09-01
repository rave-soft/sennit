package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInitializeSelectionStateDefaultsToYes(t *testing.T) {
	var state initializeSelectionState

	require.True(t, state.yesSelected())
}

func TestInitializeSelectionStateToggle(t *testing.T) {
	var state initializeSelectionState

	state.toggle()
	require.False(t, state.yesSelected())

	state.toggle()
	require.True(t, state.yesSelected())
}
