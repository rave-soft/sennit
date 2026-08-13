package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rave-soft/braid/internal/config"
	"github.com/stretchr/testify/require"
)

func TestRuntimeCacheHitAndConcurrentMiss(t *testing.T) {
	cache := newRuntimeCache()
	key := runtimeKey{config: 1}
	var calls atomic.Int32
	build := func(ctx context.Context, _ runtimeKey) (*compiledRuntime, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		calls.Add(1)
		return &compiledRuntime{key: key}, nil
	}

	const workers = 16
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			_, err := cache.getOrBuild(context.Background(), func() runtimeKey { return key }, build)
			require.NoError(t, err)
		}()
	}
	wg.Wait()
	require.Equal(t, int32(1), calls.Load())

	_, err := cache.getOrBuild(context.Background(), func() runtimeKey { return key }, build)
	require.NoError(t, err)
	require.Equal(t, int32(1), calls.Load())
}

func TestRuntimeCacheRetriesStaleHitUntilStable(t *testing.T) {
	cache := newRuntimeCache()
	key := runtimeKey{config: 1}
	var current atomic.Uint64
	current.Store(key.config)
	var calls atomic.Int32
	build := func(_ context.Context, k runtimeKey) (*compiledRuntime, error) {
		calls.Add(1)
		if k.config == 1 {
			current.Store(2)
		}
		return &compiledRuntime{key: k}, nil
	}

	run, err := cache.getOrBuild(context.Background(), func() runtimeKey { return runtimeKey{config: current.Load()} }, build)
	require.NoError(t, err)
	require.Equal(t, uint64(2), run.key.config)
	require.Equal(t, int32(2), calls.Load())

	// A hit is also revalidated. Force a change between hit validation reads.
	changed := false
	run, err = cache.getOrBuild(context.Background(), func() runtimeKey {
		if !changed {
			changed = true
			return runtimeKey{config: 2}
		}
		return runtimeKey{config: 3}
	}, build)
	require.NoError(t, err)
	require.Equal(t, uint64(3), run.key.config)
	require.Equal(t, int32(3), calls.Load())
}

func TestActiveRuntimeOnlyReplacesMatchingModelCredentials(t *testing.T) {
	originalModel := Model{ModelCfg: config.SelectedModel{Provider: "provider", Model: "model"}}
	refreshedModel := Model{ModelCfg: config.SelectedModel{Provider: "provider", Model: "model"}}
	active := newActiveRuntime(&compiledRuntime{large: originalModel})
	active.store(&compiledRuntime{large: refreshedModel})
	require.Equal(t, "provider", active.load().large.ModelCfg.Provider)
	require.Equal(t, "model", active.load().large.ModelCfg.Model)
}

func TestRuntimeCacheDoesNotCacheFailures(t *testing.T) {
	cache := newRuntimeCache()
	key := runtimeKey{config: 1}
	var calls atomic.Int32
	_, err := cache.getOrBuild(context.Background(), func() runtimeKey { return key }, func(context.Context, runtimeKey) (*compiledRuntime, error) {
		calls.Add(1)
		return nil, errors.New("temporary failure")
	})
	require.Error(t, err)
	_, err = cache.getOrBuild(context.Background(), func() runtimeKey { return key }, func(context.Context, runtimeKey) (*compiledRuntime, error) {
		calls.Add(1)
		return &compiledRuntime{key: key}, nil
	})
	require.NoError(t, err)
	require.Equal(t, int32(2), calls.Load())
}
