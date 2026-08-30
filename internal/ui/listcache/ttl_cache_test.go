package listcache

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTTLCacheZeroValueAndSet(t *testing.T) {
	var cache TTLCache[int]
	require.False(t, cache.Fresh(time.Hour))
	require.False(t, cache.InFlight)
	require.Equal(t, uint64(0), cache.Generation)

	cache.Set(42)
	require.Equal(t, 42, cache.Value)
	require.True(t, cache.Fresh(time.Hour))
}

func TestTTLCacheBeginIsSingleFlight(t *testing.T) {
	var cache TTLCache[string]
	Generation, started := cache.Begin()
	require.True(t, started)
	require.Equal(t, uint64(0), Generation)
	require.True(t, cache.InFlight)

	_, started = cache.Begin()
	require.False(t, started)
}

func TestTTLCacheInvalidatePreservesValue(t *testing.T) {
	var cache TTLCache[string]
	cache.Set("old")
	Generation, started := cache.Begin()
	require.True(t, started)

	cache.Invalidate()
	require.Equal(t, "old", cache.Value)
	require.False(t, cache.Fresh(time.Hour))
	require.Equal(t, Generation+1, cache.Generation)
}
