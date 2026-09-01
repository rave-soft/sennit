package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPermissionResponseState(t *testing.T) {
	t.Parallel()

	t.Run("zero state opens a request", func(t *testing.T) {
		t.Parallel()
		var state permissionResponseState

		generation, opened := state.open("perm-1", false)

		require.True(t, opened)
		require.Equal(t, uint64(1), generation)
		require.Equal(t, "perm-1", state.permission)
		require.False(t, state.isLoading())
	})

	t.Run("duplicate delivery leaves an open request unchanged", func(t *testing.T) {
		t.Parallel()
		var state permissionResponseState
		generation, _ := state.open("perm-1", false)
		_, started := state.begin("perm-1")
		require.True(t, started)

		duplicateGeneration, opened := state.open("perm-1", true)

		require.False(t, opened)
		require.Equal(t, generation, duplicateGeneration)
		require.True(t, state.isLoading())
	})

	t.Run("replacement claims a new generation and clears response", func(t *testing.T) {
		t.Parallel()
		var state permissionResponseState
		generation, _ := state.open("perm-1", false)
		_, _ = state.begin("perm-1")

		replacementGeneration, opened := state.open("perm-2", true)

		require.True(t, opened)
		require.Greater(t, replacementGeneration, generation)
		require.Equal(t, "perm-2", state.permission)
		require.False(t, state.isLoading())
	})

	t.Run("dismissed request reopens as a fresh lifecycle", func(t *testing.T) {
		t.Parallel()
		var state permissionResponseState
		generation, _ := state.open("perm-1", false)

		reopenedGeneration, opened := state.open("perm-1", false)

		require.True(t, opened)
		require.Greater(t, reopenedGeneration, generation)
	})

	t.Run("response begin requires an opened request", func(t *testing.T) {
		t.Parallel()
		var state permissionResponseState

		generation, started := state.begin("")

		require.False(t, started)
		require.Zero(t, generation)
		require.False(t, state.isLoading())
	})

	t.Run("duplicate response begin is rejected", func(t *testing.T) {
		t.Parallel()
		var state permissionResponseState
		generation, _ := state.open("perm-1", false)
		_, started := state.begin("perm-1")
		require.True(t, started)

		duplicateGeneration, duplicateStarted := state.begin("perm-1")

		require.False(t, duplicateStarted)
		require.Zero(t, duplicateGeneration)
		require.Equal(t, generation, state.generation)
	})

	t.Run("unsolicited matching completion is ignored", func(t *testing.T) {
		t.Parallel()
		var state permissionResponseState
		generation, _ := state.open("perm-1", false)

		require.False(t, state.complete("perm-1", generation))
		permission, currentGeneration := state.current()
		require.Equal(t, "perm-1", permission)
		require.Equal(t, generation, currentGeneration)
		require.False(t, state.isLoading())
	})

	t.Run("stale and wrong completions are ignored", func(t *testing.T) {
		t.Parallel()
		var state permissionResponseState
		generation, _ := state.open("perm-1", false)
		_, _ = state.begin("perm-1")

		require.False(t, state.complete("perm-1", generation-1))
		require.True(t, state.isLoading())
		require.False(t, state.complete("perm-2", generation))
		require.True(t, state.isLoading())
	})

	t.Run("matching completion consumes response", func(t *testing.T) {
		t.Parallel()
		var state permissionResponseState
		generation, _ := state.open("perm-1", false)
		_, _ = state.begin("perm-1")

		require.True(t, state.complete("perm-1", generation))
		require.False(t, state.isLoading())
	})
}
