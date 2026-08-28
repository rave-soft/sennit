package csync

import (
	"iter"
	"maps"
	"sync"
)

// NewVersionedMap creates a new versioned, thread-safe map.
func NewVersionedMap[K comparable, V any]() *VersionedMap[K, V] {
	return &VersionedMap[K, V]{
		inner: make(map[K]V),
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
}

// Del deletes the specified key from the map and increments the version,
// atomically with respect to readers.
func (m *VersionedMap[K, V]) Del(key K) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.inner, key)
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

// Snapshot returns a copy of the map's content together with the version
// it corresponds to, read atomically. Unlike calling Copy and Version
// separately, the pair returned here is always consistent: no writer can
// land between the two reads.
func (m *VersionedMap[K, V]) Snapshot() (map[K]V, uint64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return maps.Clone(m.inner), m.v
}
