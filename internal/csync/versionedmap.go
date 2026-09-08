package csync

import (
	"iter"
	"maps"
	"sync"
)

// NewVersionedMap creates a new versioned, thread-safe map.
func NewVersionedMap[K comparable, V any]() *VersionedMap[K, V] {
	return &VersionedMap[K, V]{
		inner:    make(map[K]V),
		versions: make(map[K]uint64),
	}
}

// VersionedMap is a thread-safe map that keeps track of its version: every
// mutation bumps the version, so a consumer can cheaply tell "nothing
// changed" apart from "something changed" without diffing content.
//
// That guarantee only holds if a reader can never observe a version and a
// content snapshot that don't correspond to each other. VersionedMap used
// to delegate storage to a *Map and bump a separate atomic counter after
// each call, which left a window between the two where a concurrent reader
// could pair the new content with the old version (or vice versa). Rather
// than add a second lock on top of Map's own — a lock-hierarchy problem —
// VersionedMap holds its map directly under its own mutex, so content and
// version are one critical section, not two.
type VersionedMap[K comparable, V any] struct {
	mu    sync.RWMutex
	inner map[K]V
	v     uint64
	// versions stamps each live key with the map version its own last
	// mutation produced, so a consumer interested in one key can tell
	// "this key changed" apart from "some other key changed" — a
	// distinction the whole-map version cannot make. Stamps come from the
	// same counter as v, so they are unique and monotonic across keys and
	// a per-key comparison never has to reason about wraparound.
	//
	// A deleted key drops its stamp rather than keeping one: KeyVersion
	// then reports 0 for it, which differs from whatever stamp a reader
	// held, so the deletion still reads as a change, and a long-lived map
	// does not accumulate stamps for keys it no longer holds.
	versions map[K]uint64
}

// Get gets the value for the specified key from the map.
func (m *VersionedMap[K, V]) Get(key K) (V, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.inner[key]
	return v, ok
}

// Set sets the value for the specified key in the map and increments the
// version, atomically with respect to readers.
func (m *VersionedMap[K, V]) Set(key K, value V) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inner[key] = value
	m.v++
	m.versions[key] = m.v
}

// Del deletes the specified key from the map and increments the version,
// atomically with respect to readers.
func (m *VersionedMap[K, V]) Del(key K) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.inner, key)
	delete(m.versions, key)
	m.v++
}

// Seq2 returns an iter.Seq2 that yields key-value pairs from the map.
func (m *VersionedMap[K, V]) Seq2() iter.Seq2[K, V] {
	dst := m.Copy()
	return func(yield func(K, V) bool) {
		for k, v := range dst {
			if !yield(k, v) {
				return
			}
		}
	}
}

// Copy returns a copy of the inner map.
func (m *VersionedMap[K, V]) Copy() map[K]V {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return maps.Clone(m.inner)
}

// Len returns the number of items in the map.
func (m *VersionedMap[K, V]) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.inner)
}

// Version returns the current version of the map.
func (m *VersionedMap[K, V]) Version() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.v
}

// KeyVersion returns the map version stamped on key by its own last
// mutation, or 0 if the map does not currently hold it. Two reads of the
// same key returning the same value means that key was not touched in
// between, however many other keys were.
func (m *VersionedMap[K, V]) KeyVersion(key K) uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.versions[key]
}

// Snapshot returns a copy of the map's content together with the version
// it corresponds to, read atomically. Unlike calling Copy and Version
// separately, the pair returned here is always consistent: no writer can
// land between the two reads.
func (m *VersionedMap[K, V]) Snapshot() (map[K]V, uint64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return maps.Clone(m.inner), m.v
}
