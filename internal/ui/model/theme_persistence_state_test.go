package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestThemePersistenceState_ZeroValueRejectsCompletion(t *testing.T) {
	t.Parallel()

	var state themePersistenceState
	require.False(t, state.complete(1))
	require.Zero(t, state.generation)
	require.False(t, state.pending)
}

func TestThemePersistenceState_SuccessiveBeginsSupersedeEarlierGeneration(t *testing.T) {
	t.Parallel()

	var state themePersistenceState
	first := state.begin()
	second := state.begin()

	require.Equal(t, uint64(1), first)
	require.Equal(t, uint64(2), second)
	require.True(t, state.pending)
	require.False(t, state.complete(first), "older result must not consume newer selection")
	require.True(t, state.pending)
	require.True(t, state.complete(second))
	require.False(t, state.pending)
}

func TestThemePersistenceState_RejectsUnsolicitedAndDuplicateCompletions(t *testing.T) {
	t.Parallel()

	var state themePersistenceState
	generation := state.begin()

	require.False(t, state.complete(generation+1), "unsolicited result must be ignored")
	require.True(t, state.pending)
	require.True(t, state.complete(generation))
	require.False(t, state.complete(generation), "duplicate terminal result must be ignored")
}
