package csync

import (
	"encoding/json"
	"iter"
	"maps"
	"sync"
)

// Map is a concurrent map implementation that provides thread-safe access.
type Map[K comparable, V any] struct {
	schemaAlias[K, V]
	inner  map[K]V
	frozen bool
	mu     sync.RWMutex
}

// NewMap creates a thread-safe map, optionally initialized from a map. The
// initial map is copied, so the caller keeps ownership of it: later reads or
// writes to the original don't bypass the lock, and mutations via Map don't
// leak back into it.
func NewMap[K comparable, V any](initial ...map[K]V) *Map[K, V] {
	inner := make(map[K]V)
	if len(initial) > 0 {
		inner = maps.Clone(initial[0])
		if inner == nil {
			inner = make(map[K]V)
		}
	}
	return &Map[K, V]{inner: inner}
}

// Reset replaces the inner map with a copy of input. Like NewMap, input is
// cloned so the caller keeps ownership of it: later reads or writes to the
// original don't bypass the lock, and mutations via Map don't leak back
// into it.
func (m *Map[K, V]) Reset(input map[K]V) {
	inner := maps.Clone(input)
	if inner == nil {
		inner = make(map[K]V)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.panicIfFrozenLocked()
	m.inner = inner
}

// Set sets the value for the specified key in the map.
func (m *Map[K, V]) Set(key K, value V) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.panicIfFrozenLocked()
	m.inner[key] = value
}

// Del deletes the specified key from the map.
func (m *Map[K, V]) Del(key K) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.panicIfFrozenLocked()
	delete(m.inner, key)
}

// Freeze marks the map read-only: every subsequent mutation — Set, Del,
// Reset, Take, GetOrSet installing a missing key, CompareAndDelete
// removing one, or UnmarshalJSON — panics instead of changing it. It is
// idempotent: freezing an already-frozen map is a no-op, not an error.
//
// This exists for published, supposedly-immutable snapshots (e.g.
// config.Config.Providers) that are reached through a plain exported field
// rather than an API that could simply omit mutators. Freeze turns the
// "don't mutate this" convention into something the race detector and a
// panic enforce instead of something callers have to remember.
func (m *Map[K, V]) Freeze() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.frozen = true
}

// panicIfFrozenLocked panics if the map has been frozen. Caller must hold
// m.mu (read or write) so the check and the mutation it guards observe a
// consistent frozen flag.
func (m *Map[K, V]) panicIfFrozenLocked() {
	if m.frozen {
		panic("mutation of a frozen csync.Map: the published config snapshot must be replaced via clone-and-swap, not edited in place")
	}
}

// CompareAndDelete deletes key from m only if its current value equals
// expected. Returns true if the deletion occurred. This is the ABA-safe
// cleanup primitive: it prevents a deferred cleanup from removing a value
// that was replaced by a newer writer in the window between the explicit
// Del and the deferred Del.
//
// This is a package-level function, not a method, so V can be constrained
// to comparable: the compiler rejects Map[K, V] instantiations whose V
// can't be compared with ==, rather than panicking at runtime the way a
// method taking `expected any` would for a V holding an uncomparable
// dynamic type (e.g. a slice or map). Callers typically use a pointer V
// (e.g. agent.activeRequests, keyed by *activeCancel), which is always
// comparable.
func CompareAndDelete[K comparable, V comparable](m *Map[K, V], key K, expected V) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.inner[key]
	if !ok {
		return false
	}
	if current != expected {
		return false
	}
	m.panicIfFrozenLocked()
	delete(m.inner, key)
	return true
}

// Get gets the value for the specified key from the map.
func (m *Map[K, V]) Get(key K) (V, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.inner[key]
	return v, ok
}

// Len returns the number of items in the map.
func (m *Map[K, V]) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.inner)
}

// GetOrSet gets and returns the key if it exists, otherwise, it executes the
// given function, set its return value for the given key, and returns it.
// The whole read-or-compute-and-store sequence is atomic: fn runs under the
// map's write lock, so concurrent GetOrSet calls for the same missing key
// never race to call fn twice, and no writer can observe or store a value
// for that key while fn is running.
//
// Contract: fn must not call any method on this same *Map (directly or
// transitively) — doing so deadlocks, since the lock fn runs under is not
// reentrant. Touching a different Map instance from within fn is fine.
func (m *Map[K, V]) GetOrSet(key K, fn func() V) V {
	m.mu.Lock()
	defer m.mu.Unlock()
	if got, ok := m.inner[key]; ok {
		return got
	}
	m.panicIfFrozenLocked()
	value := fn()
	m.inner[key] = value
	return value
}

// Take gets an item and then deletes it.
func (m *Map[K, V]) Take(key K) (V, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.panicIfFrozenLocked()
	v, ok := m.inner[key]
	delete(m.inner, key)
	return v, ok
}

// Copy returns a copy of the inner map.
func (m *Map[K, V]) Copy() map[K]V {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return maps.Clone(m.inner)
}

// Seq2 returns an iter.Seq2 that yields key-value pairs from the map.
//
// It iterates a snapshot, not the map: the whole map is cloned up front,
// under the read lock, and the returned sequence walks that copy. So the
// lock is not held while the caller's loop body runs (a body that calls
// back into this Map cannot deadlock), the caller sees a consistent view
// even if a writer lands mid-loop, and each call costs one full copy.
// Callers on a hot path that only need a count or a single lookup should
// use Len or Get instead of ranging.
func (m *Map[K, V]) Seq2() iter.Seq2[K, V] {
	dst := m.Copy()
	return func(yield func(K, V) bool) {
		for k, v := range dst {
			if !yield(k, v) {
				return
			}
		}
	}
}

// Seq returns an iter.Seq that yields values from the map. Like Seq2,
// which it delegates to, it iterates a snapshot and therefore copies the
// whole map once per call.
func (m *Map[K, V]) Seq() iter.Seq[V] {
	return func(yield func(V) bool) {
		for _, v := range m.Seq2() {
			if !yield(v) {
				return
			}
		}
	}
}

var (
	_ json.Unmarshaler = &Map[string, any]{}
	_ json.Marshaler   = &Map[string, any]{}
)

// schemaAlias carries JSONSchemaAlias for Map. It exists as a separate
// zero-size type because the method needs a value receiver — jsonschema
// checks interface satisfaction on the non-pointer type after stripping
// pointers (reflect.go, refOrReflectTypeToSchema) — and a value receiver on
// Map itself would copy Map's RWMutex on every call, which go vet's
// copylocks rightly flags. Embedding promotes the method into Map's method
// set while the call copies only this empty struct.
type schemaAlias[K comparable, V any] struct{}

// JSONSchemaAlias returns the underlying map type for JSON schema generation.
func (schemaAlias[K, V]) JSONSchemaAlias() any {
	m := map[K]V{}
	return m
}

// UnmarshalJSON implements json.Unmarshaler.
func (m *Map[K, V]) UnmarshalJSON(data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Decoding replaces m.inner wholesale, same as Reset — guard it the
	// same way. In practice this only ever runs against a fresh, unfrozen
	// Map produced by json.Unmarshal allocating a new Config; a frozen
	// target would mean something decoded straight into an already-
	// published snapshot, which is the exact mistake Freeze exists to
	// catch.
	m.panicIfFrozenLocked()
	m.inner = make(map[K]V)
	return json.Unmarshal(data, &m.inner)
}

// MarshalJSON implements json.Marshaler.
func (m *Map[K, V]) MarshalJSON() ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return json.Marshal(m.inner)
}
