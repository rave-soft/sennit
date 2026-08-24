package agent

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/config"
)

var errRuntimeChanged = errors.New("agent runtime changed while it was being built")

type runtimeKey struct {
	config uint64
	mcp    uint64
	local  uint64
}

type compiledRuntime struct {
	key          runtimeKey
	model        Model
	tools        []fantasy.AgentTool
	systemPrompt string

	// These values are captured with the model and tools. A Run must not
	// consult the live ConfigStore after runtimeFor returns: config reloads and
	// credential rotation are allowed to publish a new runtime while the old
	// one is still executing.
	providerCfg          config.ProviderConfig
	providerOptions      fantasy.ProviderOptions
	temperature          *float64
	topP                 *float64
	topK                 *int64
	frequencyPenalty     *float64
	presencePenalty      *float64
	maxOutputTokens      int64
	systemPromptPrefix   string
	disableAutoSummarize bool
}

type activeRuntime struct {
	mu      sync.RWMutex
	runtime *compiledRuntime
}

func newActiveRuntime(runtime *compiledRuntime) *activeRuntime {
	return &activeRuntime{runtime: runtime}
}

func (r *activeRuntime) load() *compiledRuntime {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.runtime
}

func (r *activeRuntime) store(runtime *compiledRuntime) {
	r.mu.Lock()
	r.runtime = runtime
	r.mu.Unlock()
}

type runtimeCache struct {
	mu      sync.Mutex
	entries map[runtimeKey]*compiledRuntime
	flight  map[runtimeKey]*runtimeFlight

	hits, misses, builds, invalidations atomic.Uint64
	lastKey                             runtimeKey
	hasLastKey                          bool
	pendingReason                       map[runtimeKey]string
}

type runtimeCacheStats struct {
	Hits, Misses, Builds, Invalidations uint64
}

type runtimeFlight struct {
	done chan struct{}
	run  *compiledRuntime
	err  error
}

func newRuntimeCache() *runtimeCache {
	return &runtimeCache{
		entries:       make(map[runtimeKey]*compiledRuntime),
		flight:        make(map[runtimeKey]*runtimeFlight),
		pendingReason: make(map[runtimeKey]string),
	}
}

// getOrBuild returns a runtime only after its key remains current. A catalog or
// config update may race either a cache hit or an in-flight build, so every
// completion path validates the key and retries until it observes a stable
// snapshot or the caller cancels the context.
func (c *runtimeCache) log(ctx context.Context, event, reason string, key runtimeKey, count uint64) {
	level := slog.LevelDebug
	if event == "invalidate" {
		level = slog.LevelInfo
	}
	slog.Log(ctx, level, "runtime cache", "component", "runtime_cache", "event", event, "reason", reason, "config_version", key.config, "mcp_version", key.mcp, "local_version", key.local, "count", count, "session_id", tools.GetSessionFromContext(ctx), "run_id", RunIDFromContext(ctx))
}

func (c *runtimeCache) missReasonLocked(key runtimeKey) string {
	if reason := c.pendingReason[key]; reason != "" {
		return reason
	}
	if !c.hasLastKey {
		return "cold"
	}
	if c.lastKey.config != key.config {
		return "config_changed"
	}
	if c.lastKey.mcp != key.mcp {
		return "mcp_changed"
	}
	if c.lastKey.local != key.local {
		return "local_changed"
	}
	return "cold"
}

func (c *runtimeCache) getOrBuild(ctx context.Context, current func() runtimeKey, build func(context.Context, runtimeKey) (*compiledRuntime, error)) (*compiledRuntime, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := current()
		c.mu.Lock()
		reason := c.missReasonLocked(key)

		if run := c.entries[key]; run != nil {
			count := c.hits.Add(1)
			c.log(ctx, "hit", "current_key", key, count)
			c.mu.Unlock()
			if current() == key {
				return run, nil
			}
			continue
		}
		if flight := c.flight[key]; flight != nil {
			c.mu.Unlock()
			select {
			case <-flight.done:
				if flight.err != nil {
					if errors.Is(flight.err, errRuntimeChanged) {
						continue
					}
					// The build ran on whichever caller started the
					// flight. If that one was cancelled, the failure is
					// theirs, not ours: adopting it failed a turn whose
					// own context was perfectly alive, just because it
					// arrived second. Go round again — with no flight in
					// progress, this caller becomes the builder and uses
					// its own context.
					if ctx.Err() == nil && (errors.Is(flight.err, context.Canceled) || errors.Is(flight.err, context.DeadlineExceeded)) {
						continue
					}
					return nil, flight.err
				}
				if current() == key {
					return flight.run, nil
				}
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		count := c.misses.Add(1)
		c.log(ctx, "miss", reason, key, count)
		flight := &runtimeFlight{done: make(chan struct{})}
		c.flight[key] = flight
		c.mu.Unlock()

		run, err := build(ctx, key)
		count = c.builds.Add(1)
		c.log(ctx, "build", "completed", key, count)
		c.mu.Lock()
		if err == nil && current() == key {
			// Publish only the current generation. The mutex makes this atomic
			// with invalidation, so an old in-flight builder cannot retain an
			// entry after the generation has advanced.
			clear(c.entries)
			c.entries[key] = run
			clear(c.pendingReason)
			c.lastKey, c.hasLastKey = key, true
		} else if err == nil {
			run = nil
			err = errRuntimeChanged
		}
		flight.run, flight.err = run, err
		delete(c.flight, key)
		close(flight.done)
		c.mu.Unlock()
		if errors.Is(err, errRuntimeChanged) {
			continue
		}
		return run, err
	}
}

func (c *runtimeCache) invalidate(ctx context.Context, reason string, key runtimeKey) {
	c.invalidateAndPublish(ctx, reason, key, nil)
}

// invalidateAndPublish records an invalidation reason and publishes its
// generation while holding the cache mutex. This closes the only window in
// which an old builder could otherwise publish and clear the pending reason
// between reason registration and generation publication.
func (c *runtimeCache) invalidateAndPublish(ctx context.Context, reason string, key runtimeKey, publish func()) {
	c.mu.Lock()
	// Retain at most the generation being published. A builder may publish only
	// after confirming its key is still current under this same mutex.
	for cachedKey := range c.entries {
		if cachedKey != key {
			delete(c.entries, cachedKey)
		}
	}
	clear(c.pendingReason)
	c.pendingReason[key] = reason
	if publish != nil {
		publish()
	}
	c.mu.Unlock()
	count := c.invalidations.Add(1)
	c.log(ctx, "invalidate", reason, key, count)
}

func (c *runtimeCache) stats() runtimeCacheStats {
	return runtimeCacheStats{Hits: c.hits.Load(), Misses: c.misses.Load(), Builds: c.builds.Load(), Invalidations: c.invalidations.Load()}
}
