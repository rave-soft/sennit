package model

import "time"

// ttlCache holds a value that is refreshed asynchronously. Its zero value is
// ready to use. Callers use begin to make a request single-flight, and apply
// accepts a result only when it belongs to the current generation.
//
// Invalidate intentionally preserves value: rendering continues from the last
// known state while a replacement is fetched.
//
// Failures are tracked separately from values (failedAt, not timestamp)
// because the two answer different questions: timestamp says how old the
// displayed value is, failedAt says how recently an attempt to replace it
// came back empty-handed. Conflating them would either make a failure look
// like a fresh value or - the bug this split fixes - leave a failing
// refresh with no record at all, so the TTL never held it back and it
// re-dispatched on every single Update. A refresh that fails on every
// attempt (a workspace that refuses the call outright, say) then spins as
// fast as the event loop turns, and the failure's own result message keeps
// the loop turning.
type ttlCache[T any] struct {
	value     T
	timestamp time.Time
	// failedAt is when the last refresh failed, zeroed by a success. See
	// backingOff.
	failedAt   time.Time
	inFlight   bool
	generation uint64
}

// fresh reports whether the most recently accepted value is within ttl.
func (c *ttlCache[T]) fresh(ttl time.Duration) bool {
	return !c.timestamp.IsZero() && time.Since(c.timestamp) < ttl
}

// backingOff reports whether the last refresh failed less than d ago, so
// the caller should hold off rather than retry immediately. Always false
// once a refresh has succeeded.
func (c *ttlCache[T]) backingOff(d time.Duration) bool {
	return !c.failedAt.IsZero() && time.Since(c.failedAt) < d
}

// set writes a known-good value through the cache, clearing any recorded
// failure: a success means whatever was wrong no longer is.
func (c *ttlCache[T]) set(value T) {
	c.value = value
	c.timestamp = time.Now()
	c.failedAt = time.Time{}
}

// invalidate makes the cached value stale and rejects any request that began
// before the invalidation. The cached value remains available to readers.
func (c *ttlCache[T]) invalidate() {
	c.timestamp = time.Time{}
	c.generation++
}

// begin starts a refresh if one is not already running. It returns the
// generation to include in the result and whether the caller owns the fetch.
func (c *ttlCache[T]) begin() (uint64, bool) {
	if c.inFlight {
		return 0, false
	}
	c.inFlight = true
	return c.generation, true
}

// complete clears the in-flight marker and reports whether generation still
// matches the request's generation. It is useful when a failed fetch must
// preserve the previously accepted value and timestamp.
func (c *ttlCache[T]) complete(generation uint64) bool {
	c.inFlight = false
	return generation == c.generation
}

// fail clears the in-flight marker and, when generation still matches,
// records the failure so backingOff holds the next attempt back. A stale
// generation is left unrecorded on purpose: something invalidated this
// cache while the request was out, and that invalidation is entitled to a
// fresh attempt rather than inheriting a superseded request's failure.
// Returns whether the generation matched.
func (c *ttlCache[T]) fail(generation uint64) bool {
	if !c.complete(generation) {
		return false
	}
	c.failedAt = time.Now()
	return true
}
