package model

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTTLCacheZeroValueAndSet(t *testing.T) {
	var cache ttlCache[int]
	require.False(t, cache.fresh(time.Hour))
	require.False(t, cache.inFlight)
	require.Equal(t, uint64(0), cache.generation)

	cache.set(42)
	require.Equal(t, 42, cache.value)
	require.True(t, cache.fresh(time.Hour))
}

func TestTTLCacheBeginIsSingleFlight(t *testing.T) {
	var cache ttlCache[string]
	generation, started := cache.begin()
	require.True(t, started)
	require.Equal(t, uint64(0), generation)
	require.True(t, cache.inFlight)

	_, started = cache.begin()
	require.False(t, started)
}

func TestTTLCacheApplyMatchingGeneration(t *testing.T) {
	var cache ttlCache[int]
	generation, started := cache.begin()
	require.True(t, started)

	require.True(t, cache.apply(generation, 7))
	require.Equal(t, 7, cache.value)
	require.True(t, cache.fresh(time.Hour))
	require.False(t, cache.inFlight)
}

func TestTTLCacheInvalidatePreservesValueAndRejectsStaleApply(t *testing.T) {
	var cache ttlCache[string]
	cache.set("old")
	generation, started := cache.begin()
	require.True(t, started)

	cache.invalidate()
	require.Equal(t, "old", cache.value)
	require.False(t, cache.fresh(time.Hour))
	require.Equal(t, generation+1, cache.generation)

	require.False(t, cache.apply(generation, "stale"))
	require.Equal(t, "old", cache.value)
	require.False(t, cache.inFlight)
}
