package model

import "time"

// ttlCache holds a value that is refreshed asynchronously. Its zero value is
// ready to use. Callers use begin to make a request single-flight, and apply
// accepts a result only when it belongs to the current generation.
//
// Invalidate intentionally preserves value: rendering continues from the last
// known state while a replacement is fetched.
type ttlCache[T any] struct {
	value      T
	timestamp  time.Time
	inFlight   bool
	generation uint64
}

// fresh reports whether the most recently accepted value is within ttl.
func (c *ttlCache[T]) fresh(ttl time.Duration) bool {
	return !c.timestamp.IsZero() && time.Since(c.timestamp) < ttl
}

// set writes a known-good value through the cache.
func (c *ttlCache[T]) set(value T) {
	c.value = value
	c.timestamp = time.Now()
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

// apply clears the in-flight marker and accepts value only when generation
// still matches the request's generation. A stale result therefore cannot
// overwrite a newer optimistic value or invalidation.
func (c *ttlCache[T]) apply(generation uint64, value T) bool {
	if !c.complete(generation) {
		return false
	}
	c.set(value)
	return true
}
