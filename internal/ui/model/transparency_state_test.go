package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTransparencyState_ZeroState(t *testing.T) {
	t.Parallel()

	var state transparencyState

	require.False(t, state.isLoading())
	require.False(t, state.complete(0), "zero state has no result to consume")
}

func TestTransparencyState_BeginRejectsWhileLoading(t *testing.T) {
	t.Parallel()

	var state transparencyState
	generation, started := state.begin()

	require.True(t, started)
	require.Equal(t, uint64(1), generation)
	require.True(t, state.isLoading())

	generation, started = state.begin()
	require.False(t, started)
	require.Zero(t, generation)
	require.True(t, state.isLoading())
}

func TestTransparencyState_GenerationProgressesAfterConsumption(t *testing.T) {
	t.Parallel()

	var state transparencyState
	first, started := state.begin()
	require.True(t, started)
	require.True(t, state.complete(first))

	second, started := state.begin()
	require.True(t, started)
	require.Equal(t, first+1, second)
}

func TestTransparencyState_StaleResultPreservesLoading(t *testing.T) {
	t.Parallel()

	var state transparencyState
	generation, started := state.begin()
	require.True(t, started)

	require.False(t, state.complete(generation+1))
	require.True(t, state.isLoading())
}

func TestTransparencyState_MatchingResultConsumesLoading(t *testing.T) {
	t.Parallel()

	var state transparencyState
	generation, started := state.begin()
	require.True(t, started)

	require.True(t, state.complete(generation))
	require.False(t, state.isLoading())
	require.False(t, state.complete(generation), "a result can only be consumed once")
}
