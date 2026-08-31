package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPendingSendStateSerializesAndPreservesQueueOrder(t *testing.T) {
	t.Parallel()

	state := pendingSendState{}
	state.enqueue(sendQueueItem{content: "first"})
	state.enqueue(sendQueueItem{content: "second"})

	first, ok := state.dequeue()
	require.True(t, ok)
	require.Equal(t, "first", first.content)
	state.beginActive()
	_, ok = state.dequeue()
	require.False(t, ok, "an active send must prevent a second dispatch")

	state.finishActive()
	second, ok := state.dequeue()
	require.True(t, ok)
	require.Equal(t, "second", second.content)
}

func TestPendingSendStateLoadingGenerationAndResetBoundaries(t *testing.T) {
	t.Parallel()

	state := pendingSendState{}
	generation := state.beginLoading()
	state.enqueue(sendQueueItem{content: "waiting", generation: generation})
	state.beginActive()

	require.True(t, state.acceptsLoadingResult(generation))
	require.False(t, state.acceptsLoadingResult(generation+1))

	state.discardLoading()
	require.Empty(t, state.queue)
	require.Zero(t, state.generation)
	require.False(t, state.loading)
	require.True(t, state.activeNow(), "a failed session load must not clear an already active send")

	state.rejectCreation()
	require.Empty(t, state.queue)
	require.False(t, state.loading)
	require.False(t, state.activeNow())
}

func TestPendingSendStateSessionChangeKeepsLoadingGeneration(t *testing.T) {
	t.Parallel()

	state := pendingSendState{}
	generation := state.beginLoading()
	state.enqueue(sendQueueItem{content: "stale"})
	state.beginActive()

	state.discardForSessionChange()

	require.Empty(t, state.queue)
	require.False(t, state.activeNow())
	require.True(t, state.acceptsLoadingResult(generation), "the in-flight creation still owns its generation")
}
