package model

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPromptQueueStateLifecycle(t *testing.T) {
	t.Run("zero state", func(t *testing.T) {
		var state promptQueueState
		require.Empty(t, state.prompts())
		require.Zero(t, state.count())
		require.False(t, state.fresh(time.Hour))
	})

	t.Run("accepts a begun fetch", func(t *testing.T) {
		var state promptQueueState
		generation, started := state.begin()
		require.True(t, started)
		require.True(t, state.inFlight())
		require.True(t, state.complete(generation))
		state.accept([]string{"first", "second"})
		require.Equal(t, []string{"first", "second"}, state.prompts())
		require.Equal(t, 2, state.count())
		require.True(t, state.fresh(time.Hour))
	})

	t.Run("deduplicates in-flight fetches", func(t *testing.T) {
		var state promptQueueState
		_, started := state.begin()
		require.True(t, started)
		_, started = state.begin()
		require.False(t, started)
	})

	t.Run("invalidation rejects an old result", func(t *testing.T) {
		var state promptQueueState
		generation, started := state.begin()
		require.True(t, started)
		state.invalidate()
		require.False(t, state.complete(generation))
		require.False(t, state.inFlight())
	})

	t.Run("clear writes through fresh empty state", func(t *testing.T) {
		var state promptQueueState
		state.accept([]string{"queued"})
		generation, started := state.begin()
		require.True(t, started)
		state.clear()
		require.Empty(t, state.prompts())
		require.True(t, state.fresh(time.Hour))
		require.False(t, state.complete(generation))
	})

	t.Run("can begin and accept again after completion", func(t *testing.T) {
		var state promptQueueState
		first, started := state.begin()
		require.True(t, started)
		require.True(t, state.complete(first))
		state.accept([]string{"old"})

		second, started := state.begin()
		require.True(t, started)
		require.Equal(t, first, second)
		require.True(t, state.complete(second))
		state.accept([]string{"new"})
		require.Equal(t, []string{"new"}, state.prompts())
	})
}
