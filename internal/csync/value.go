package csync

import "sync"

// Value is a generic thread-safe wrapper for any value type.
//
// Value stores T as a whole and swaps it under a lock, so it protects the
// value itself, not what it points to. For a slice or map that only
// protects the header (pointer/len/cap, or the map header) — concurrent
// access to the underlying elements still races, so prefer [Slice] or [Map]
// for those, which lock around element access too. A pointer is fine to
// store directly: the pointer itself is what gets protected, same as any
// other value.
type Value[T any] struct {
	v  T
	mu sync.RWMutex
}

// NewValue creates a new Value with the given initial value.
//
// For slices and maps, consider [Slice] and [Map] instead: they lock
// around element access, whereas Value only protects the stored
// header/reference as a whole. See the [Value] doc for details.
func NewValue[T any](t T) *Value[T] {
	return &Value[T]{v: t}
}

// Get returns the current value.
func (v *Value[T]) Get() T {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.v
}

// Set updates the value.
func (v *Value[T]) Set(t T) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.v = t
}
