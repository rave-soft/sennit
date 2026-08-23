package agent

import (
	"context"
	"errors"
	"sync"

	"charm.land/fantasy"
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
}

type runtimeFlight struct {
	done chan struct{}
	run  *compiledRuntime
	err  error
}

func newRuntimeCache() *runtimeCache {
	return &runtimeCache{entries: make(map[runtimeKey]*compiledRuntime), flight: make(map[runtimeKey]*runtimeFlight)}
}

// getOrBuild returns a runtime only after its key remains current. A catalog or
// config update may race either a cache hit or an in-flight build, so every
// completion path validates the key and retries until it observes a stable
// snapshot or the caller cancels the context.
func (c *runtimeCache) getOrBuild(ctx context.Context, current func() runtimeKey, build func(context.Context, runtimeKey) (*compiledRuntime, error)) (*compiledRuntime, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := current()
		c.mu.Lock()
		if run := c.entries[key]; run != nil {
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
		flight := &runtimeFlight{done: make(chan struct{})}
		c.flight[key] = flight
		c.mu.Unlock()

		run, err := build(ctx, key)
		c.mu.Lock()
		if err == nil && current() == key {
			// Retain only the published generation. Existing callers hold
			// their own runtime pointer, so evicting stale entries cannot
			// invalidate an in-flight Run.
			clear(c.entries)
			c.entries[key] = run
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

func (c *runtimeCache) invalidate() {
	c.mu.Lock()
	clear(c.entries)
	c.mu.Unlock()
}
