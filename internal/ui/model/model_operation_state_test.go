package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAsyncOperationState(t *testing.T) {
	t.Parallel()

	t.Run("zero value has no operation to complete", func(t *testing.T) {
		t.Parallel()
		var state asyncOperationState

		require.False(t, state.isLoading())
		require.False(t, state.owns(0))
		require.False(t, state.complete(0))
	})

	t.Run("zero value starts at generation one", func(t *testing.T) {
		t.Parallel()
		var state asyncOperationState

		generation, started := state.begin()

		require.True(t, started)
		require.Equal(t, uint64(1), generation)
		require.True(t, state.isLoading())
	})

	t.Run("duplicate start preserves current ownership", func(t *testing.T) {
		t.Parallel()
		var state asyncOperationState
		generation, started := state.begin()
		require.True(t, started)

		duplicate, started := state.begin()

		require.False(t, started)
		require.Zero(t, duplicate)
		require.Equal(t, generation, state.generation)
		require.True(t, state.owns(generation))
	})

	t.Run("nonterminal stage retains ownership", func(t *testing.T) {
		t.Parallel()
		var state asyncOperationState
		generation, _ := state.begin()

		require.True(t, state.owns(generation))
		require.True(t, state.isLoading())
		require.True(t, state.complete(generation))
		require.False(t, state.isLoading())
	})

	t.Run("stale and unsolicited completions do not mutate state", func(t *testing.T) {
		t.Parallel()
		var state asyncOperationState
		generation, _ := state.begin()

		require.False(t, state.complete(generation+1))
		require.True(t, state.isLoading())
		require.True(t, state.complete(generation))
		require.False(t, state.complete(generation))
		require.False(t, state.isLoading())
	})

	t.Run("next operation advances after terminal result", func(t *testing.T) {
		t.Parallel()
		var state asyncOperationState
		first, _ := state.begin()
		require.True(t, state.complete(first))

		second, started := state.begin()

		require.True(t, started)
		require.Equal(t, first+1, second)
	})
}
