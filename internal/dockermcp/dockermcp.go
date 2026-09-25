// Package dockermcp probes for and caches whether the Docker MCP gateway
// (the "docker mcp" CLI) is available on this machine. It is a leaf
// package: it shells out to a vendor binary and memoises the answer in a
// process-global cache, which is not configuration data and does not
// belong in internal/config ("Config is a store, not global state"). It
// imports nothing else from this repo so any package, including
// internal/config, can depend on it without risking an import cycle.
package dockermcp

import (
	"context"
	"os/exec"
	"sync"
	"time"
)

var versionRunner = func(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "docker", "mcp", "version")
	return cmd.Run()
}

const availabilityTTL = 10 * time.Second

// cache holds Docker MCP availability behind an injectable clock, rather
// than a bare struct measured against time.Since. A single package-level
// cache makes every reader order-dependent across tests in the same
// process (whichever test warms the cache first decides what later tests
// observe), and a TTL measured against wall time lets a slow test run
// cross the 10s boundary mid-suite. now is swapped out in tests instead of
// sleeping past the TTL, and a test that needs isolation from others
// constructs its own instance rather than reaching for the package-level
// default.
type cache struct {
	mu        sync.Mutex
	available bool
	checkedAt time.Time
	known     bool

	ttl time.Duration
	now func() time.Time
}

func newCache() *cache {
	return &cache{ttl: availabilityTTL, now: time.Now}
}

// cached returns the cached availability and whether it is still fresh.
func (c *cache) cached() (available, known bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.known {
		return false, false
	}
	if c.now().Sub(c.checkedAt) > c.ttl {
		return c.available, false
	}
	return c.available, true
}

// set records a freshly checked availability, timestamped with c.now().
func (c *cache) set(available bool) {
	c.mu.Lock()
	c.available = available
	c.checkedAt = c.now()
	c.known = true
	c.mu.Unlock()
}

// defaultCache is the process-wide cache AvailabilityCached and
// RefreshAvailability go through. Tests that need isolation from other
// tests' cache state swap this out for a fresh instance rather than
// relying on the TTL to expire it; see swapCacheForTest in
// dockermcp_test.go.
var defaultCache = newCache()

// IsAvailable checks if Docker MCP is available by running
// 'docker mcp version'. The bool alone cannot distinguish "checked, and
// it's not there" from "couldn't check" (the caller's ctx was cancelled or
// expired before the probe finished), so the error return is set only for
// the latter — an ordinary probe failure (docker missing, non-zero exit)
// still reports (false, nil).
func IsAvailable(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	err := versionRunner(ctx)
	if err != nil && ctx.Err() != nil {
		return false, ctx.Err()
	}
	return err == nil, nil
}

// AvailabilityCached returns the cached Docker MCP availability and
// whether the cached value is still fresh.
func AvailabilityCached() (available bool, known bool) {
	return defaultCache.cached()
}

// RefreshAvailability refreshes and caches Docker MCP availability. On
// error the cache is left untouched: a probe that couldn't run at all
// must not overwrite a previously known-good answer.
func RefreshAvailability(ctx context.Context) (bool, error) {
	available, err := IsAvailable(ctx)
	if err != nil {
		return false, err
	}
	defaultCache.set(available)
	return available, nil
}
