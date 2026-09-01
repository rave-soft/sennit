package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNotificationStyleState_ZeroState(t *testing.T) {
	t.Parallel()

	var state notificationStyleState

	require.False(t, state.isLoading())
	require.False(t, state.complete(0), "zero state has no result to consume")
}

func TestNotificationStyleState_BeginRejectsWhileLoading(t *testing.T) {
	t.Parallel()

	var state notificationStyleState
	generation, started := state.begin()

	require.True(t, started)
	require.Equal(t, uint64(1), generation)
	require.True(t, state.isLoading())

	generation, started = state.begin()
	require.False(t, started)
	require.Zero(t, generation)
	require.True(t, state.isLoading())
}

func TestNotificationStyleState_GenerationProgressesAfterConsumption(t *testing.T) {
	t.Parallel()

	var state notificationStyleState
	first, started := state.begin()
	require.True(t, started)
	require.True(t, state.complete(first))

	second, started := state.begin()
	require.True(t, started)
	require.Equal(t, first+1, second)
}

func TestNotificationStyleState_StaleResultPreservesLoading(t *testing.T) {
	t.Parallel()

	var state notificationStyleState
	generation, started := state.begin()
	require.True(t, started)

	require.False(t, state.complete(generation+1))
	require.True(t, state.isLoading())
}

func TestNotificationStyleState_MatchingResultConsumesLoading(t *testing.T) {
	t.Parallel()

	var state notificationStyleState
	generation, started := state.begin()
	require.True(t, started)

	require.True(t, state.complete(generation))
	require.False(t, state.isLoading())
	require.False(t, state.complete(generation), "a result can only be consumed once")
}
