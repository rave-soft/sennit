package csync

import (
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVersionedMap_Set(t *testing.T) {
	t.Parallel()

	vm := NewVersionedMap[string, int]()
	require.Equal(t, uint64(0), vm.Version())

	vm.Set("key1", 42)
	require.Equal(t, uint64(1), vm.Version())

	value, ok := vm.Get("key1")
	require.True(t, ok)
	require.Equal(t, 42, value)
}

func TestVersionedMap_Del(t *testing.T) {
	t.Parallel()

	vm := NewVersionedMap[string, int]()
	vm.Set("key1", 42)
	initialVersion := vm.Version()

	vm.Del("key1")
	require.Equal(t, initialVersion+1, vm.Version())

	_, ok := vm.Get("key1")
	require.False(t, ok)
}

func TestVersionedMap_VersionIncrement(t *testing.T) {
	t.Parallel()

	vm := NewVersionedMap[string, int]()
	initialVersion := vm.Version()

	// Setting a value should increment the version
	vm.Set("key1", 42)
	require.Equal(t, initialVersion+1, vm.Version())

	// Deleting a value should increment the version
	vm.Del("key1")
	require.Equal(t, initialVersion+2, vm.Version())

	// Deleting a non-existent key should still increment the version
	vm.Del("nonexistent")
	require.Equal(t, initialVersion+3, vm.Version())
}

func TestVersionedMap_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	vm := NewVersionedMap[int, int]()
	const numGoroutines = 100
	const numOperations = 100

	// Initial version
	initialVersion := vm.Version()

	// Perform concurrent Set and Del operations
	for i := range numGoroutines {
		go func(id int) {
			for j := range numOperations {
				key := id*numOperations + j
				vm.Set(key, key*2)
				vm.Del(key)
			}
		}(i)
	}

	// Wait for operations to complete by checking the version
	// This is a simplified check - in a real test you might want to use sync.WaitGroup
	expectedMinVersion := initialVersion + uint64(numGoroutines*numOperations*2)

	// Allow some time for operations to complete
	for vm.Version() < expectedMinVersion {
		// Busy wait - in a real test you'd use proper synchronization
	}

	// Final version should be at least the expected minimum
	require.GreaterOrEqual(t, vm.Version(), expectedMinVersion)
	require.Equal(t, 0, vm.Len())
}

// TestVersionedMap_SnapshotIsAtomic asserts the invariant the type exists
// for: a given version number always corresponds to the same content. It
// hammers a shared map with concurrent writers while a reader repeatedly
// takes Snapshot()s; whenever the reader sees a version it has seen
// before, the content attached to it must be identical. Against the old
// implementation (separate version counter bumped after an unlocked
// content mutation) two different contents can share a version, or a
// version can be observed with mismatched content, because there is a
// window where a writer has updated one without the other.
func TestVersionedMap_SnapshotIsAtomic(t *testing.T) {
	t.Parallel()

	vm := NewVersionedMap[int, int]()
	const numWriters = 8
	const numOperations = 5000

	var wg sync.WaitGroup
	done := make(chan struct{})

	for i := range numWriters {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := range numOperations {
				key := id
				vm.Set(key, j)
				vm.Del(key)
			}
		}(i)
	}
	go func() {
		wg.Wait()
		close(done)
	}()

	seen := make(map[uint64]string)
	var mismatches []string
	iterations := 0
	for {
		select {
		case <-done:
			require.Empty(t, mismatches, "version -> content invariant violated:\n%s", mismatches)
			require.Greater(t, iterations, 0, "reader never got a chance to run")
			return
		default:
		}

		content, version := vm.Snapshot()
		pairs := sortedPairs(content)
		sort.Strings(pairs)
		key := fmt.Sprintf("%v", pairs)
		if prev, ok := seen[version]; ok {
			if prev != key {
				mismatches = append(mismatches, fmt.Sprintf(
					"version %d: saw %q, then %q", version, prev, key))
			}
		} else {
			seen[version] = key
		}
		iterations++
	}
}

// sortedPairs gives a stable, comparable representation of a map's content
// for the mismatch messages above; map iteration order is randomized, so
// comparing fmt.Sprintf output on the raw map would produce false
// mismatches unrelated to the bug under test.
func sortedPairs[K comparable, V any](m map[K]V) []string {
	pairs := make([]string, 0, len(m))
	for k, v := range m {
		pairs = append(pairs, fmt.Sprintf("%v=%v", k, v))
	}
	return pairs
}
