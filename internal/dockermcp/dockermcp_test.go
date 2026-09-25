package dockermcp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var errDockerUnavailable = errors.New("docker unavailable")

func setVersionRunner(t *testing.T, runner func(context.Context) error) {
	t.Helper()
	orig := versionRunner
	versionRunner = runner
	t.Cleanup(func() {
		versionRunner = orig
	})
}

// swapCacheForTest installs a fresh cache as defaultCache for the duration
// of the test, so this test cannot observe (or leave behind for a sibling
// test to observe) whatever another test warmed the shared cache to.
func swapCacheForTest(t *testing.T) *cache {
	t.Helper()
	orig := defaultCache
	fresh := newCache()
	defaultCache = fresh
	t.Cleanup(func() {
		defaultCache = orig
	})
	return fresh
}

// TestCache_TTLUsesInjectedClockNotWallTime pins that the cache's freshness
// check goes through an injected clock instead of time.Since, so a test can
// drive it past its TTL deterministically instead of sleeping (or risking a
// slow run crossing the boundary by accident).
func TestCache_TTLUsesInjectedClockNotWallTime(t *testing.T) {
	now := time.Now()
	c := newCache()
	c.now = func() time.Time { return now }

	c.set(true)
	available, known := c.cached()
	require.True(t, known, "freshly set value must be known")
	require.True(t, available)

	// Advance the injected clock past the TTL without any real time
	// passing or any t.Sleep.
	now = now.Add(availabilityTTL + time.Millisecond)
	available, known = c.cached()
	require.False(t, known, "a value past its TTL must be reported stale")
	require.True(t, available, "the stale value itself is still returned, just marked unknown")
}

// TestCache_IsolatedAcrossInstances pins the other half of the same fix:
// whatever one test's cache instance was warmed to must not leak into
// another's.
func TestCache_IsolatedAcrossInstances(t *testing.T) {
	first := newCache()
	first.set(true)

	second := newCache()
	_, known := second.cached()
	require.False(t, known, "a fresh cache instance must not see another instance's state")
}

// TestAvailabilityCached_IsolatedBetweenTests exercises the actual
// package-level entry points (AvailabilityCached, RefreshAvailability)
// rather than cache directly, via the swap seam a test uses for isolation.
func TestAvailabilityCached_IsolatedBetweenTests(t *testing.T) {
	swapCacheForTest(t)
	setVersionRunner(t, func(context.Context) error { return nil })

	_, known := AvailabilityCached()
	require.False(t, known, "a swapped-in fresh cache must start unknown")

	refreshed, err := RefreshAvailability(context.Background())
	require.NoError(t, err)
	require.True(t, refreshed)
	available, known := AvailabilityCached()
	require.True(t, known)
	require.True(t, available)
}

func TestIsAvailable(t *testing.T) {
	t.Run("true when the probe succeeds", func(t *testing.T) {
		setVersionRunner(t, func(context.Context) error { return nil })
		available, err := IsAvailable(context.Background())
		require.NoError(t, err)
		require.True(t, available)
	})

	t.Run("false when the probe fails", func(t *testing.T) {
		setVersionRunner(t, func(context.Context) error { return errDockerUnavailable })
		available, err := IsAvailable(context.Background())
		require.NoError(t, err)
		require.False(t, available)
	})

	t.Run("error when the caller's context is already done", func(t *testing.T) {
		setVersionRunner(t, func(ctx context.Context) error { return ctx.Err() })
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		available, err := IsAvailable(ctx)
		require.Error(t, err)
		require.False(t, available)
	})
}
