package listcache

import "time"

// TTLCache holds a Value that is refreshed asynchronously. Its zero Value is
// ready to use. Callers use Begin to make a request single-flight, and apply
// accepts a result only when it belongs to the current generation.
//
// Invalidate intentionally preserves Value: rendering continues from the last
// known state while a replacement is fetched.
//
// Failures are tracked separately from values (FailedAt, not Timestamp)
// because the two answer different questions: Timestamp says how old the
// displayed Value is, FailedAt says how recently an attempt to replace it
// came back empty-handed. Conflating them would either make a failure look
// like a Fresh Value or - the bug this split fixes - leave a failing
// refresh with no record at all, so the TTL never held it back and it
// re-dispatched on every single Update. A refresh that fails on every
// attempt (a workspace that refuses the call outright, say) then spins as
// fast as the event loop turns, and the failure's own result message keeps
// the loop turning.
type TTLCache[T any] struct {
	Value     T
	Timestamp time.Time
	// FailedAt is when the last refresh failed, zeroed by a success. See
	// BackingOff.
	FailedAt   time.Time
	InFlight   bool
	Generation uint64
}

// Fresh reports whether the most recently accepted Value is within TTL.
func (c *TTLCache[T]) Fresh(ttl time.Duration) bool {
	return !c.Timestamp.IsZero() && time.Since(c.Timestamp) < ttl
}

// BackingOff reports whether the last refresh failed less than d ago, so
// the caller should hold off rather than retry immediately. Always false
// once a refresh has succeeded.
func (c *TTLCache[T]) BackingOff(d time.Duration) bool {
	return !c.FailedAt.IsZero() && time.Since(c.FailedAt) < d
}

// Set writes a known-good Value through the cache, clearing any recorded
// failure: a success means whatever was wrong no longer is.
func (c *TTLCache[T]) Set(value T) {
	c.Value = value
	c.Timestamp = time.Now()
	c.FailedAt = time.Time{}
}

// Invalidate makes the cached Value stale and rejects any request that began
// before the invalidation. The cached Value remains available to readers.
func (c *TTLCache[T]) Invalidate() {
	c.Timestamp = time.Time{}
	c.Generation++
}

// Begin starts a refresh if one is not already running. It returns the
// generation to include in the result and whether the caller owns the fetch.
func (c *TTLCache[T]) Begin() (uint64, bool) {
	if c.InFlight {
		return 0, false
	}
	c.InFlight = true
	return c.Generation, true
}

// Complete clears the in-flight marker and reports whether the cache's
// generation still matches the request's. It is useful when a failed fetch must
// preserve the previously accepted Value and Timestamp.
func (c *TTLCache[T]) Complete(generation uint64) bool {
	c.InFlight = false
	return generation == c.Generation
}

// Fail clears the in-flight marker and, when the generation still matches,
// records the failure so BackingOff holds the next attempt back. A stale
// generation is left unrecorded on purpose: something invalidated this
// cache while the request was out, and that invalidation is entitled to a
// fresh attempt rather than inheriting a superseded request's failure.
// Returns whether the generation matched.
func (c *TTLCache[T]) Fail(generation uint64) bool {
	if !c.Complete(generation) {
		return false
	}
	c.FailedAt = time.Now()
	return true
}
