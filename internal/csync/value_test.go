package csync

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValue_GetSet(t *testing.T) {
	t.Parallel()

	v := NewValue(42)
	require.Equal(t, 42, v.Get())

	v.Set(100)
	require.Equal(t, 100, v.Get())
}

func TestValue_ZeroValue(t *testing.T) {
	t.Parallel()

	v := NewValue("")
	require.Equal(t, "", v.Get())

	v.Set("hello")
	require.Equal(t, "hello", v.Get())
}

func TestValue_Struct(t *testing.T) {
	t.Parallel()

	type config struct {
		Name  string
		Count int
	}

	v := NewValue(config{Name: "test", Count: 1})
	require.Equal(t, config{Name: "test", Count: 1}, v.Get())

	v.Set(config{Name: "updated", Count: 2})
	require.Equal(t, config{Name: "updated", Count: 2}, v.Get())
}

func TestValue_Pointer(t *testing.T) {
	t.Parallel()

	type inner struct{ N int }

	v := NewValue(&inner{N: 1})
	require.Equal(t, &inner{N: 1}, v.Get())

	v.Set(&inner{N: 2})
	require.Equal(t, 2, v.Get().N)
}

func TestValue_Slice(t *testing.T) {
	t.Parallel()

	v := NewValue([]string{"a", "b"})
	require.Equal(t, []string{"a", "b"}, v.Get())

	v.Set([]string{"c"})
	require.Equal(t, []string{"c"}, v.Get())
}

func TestValue_Map(t *testing.T) {
	t.Parallel()

	v := NewValue(map[string]int{"a": 1})
	require.Equal(t, map[string]int{"a": 1}, v.Get())

	v.Set(map[string]int{"b": 2})
	require.Equal(t, map[string]int{"b": 2}, v.Get())
}

func TestValue_InterfaceHoldingPointer(t *testing.T) {
	t.Parallel()

	type stringer interface{ String() string }

	v := NewValue[stringer](&namedPtr{s: "one"})
	require.Equal(t, "one", v.Get().String())

	v.Set(&namedPtr{s: "two"})
	require.Equal(t, "two", v.Get().String())
}

type namedPtr struct{ s string }

func (n *namedPtr) String() string { return n.s }

func TestValue_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	v := NewValue(0)
	var wg sync.WaitGroup

	// Concurrent writers.
	for i := range 100 {
		wg.Go(func() {
			v.Set(i)
		})
	}

	// Concurrent readers.
	for range 100 {
		wg.Go(func() {
			_ = v.Get()
		})
	}

	wg.Wait()

	// Value should be one of the set values (0-99).
	got := v.Get()
	require.GreaterOrEqual(t, got, 0)
	require.Less(t, got, 100)
}
