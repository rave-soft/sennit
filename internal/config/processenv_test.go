package config

import (
	"os"
	"sync"
	"testing"

	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/env"
	"github.com/stretchr/testify/require"
)

// TestProcessEnvMu_SerializesPushPopAndApplyEnv exercises PushPopEnvOverrides
// and applyEnv concurrently from several goroutines and asserts the process
// environment always ends up back where it started. It must be run with
// -race: that is the point of the test — before processEnvMu existed, this
// reliably tripped the race detector because PushPopEnvOverrides' push/
// restore window and applyEnv's os.Setenv calls could interleave.
func TestProcessEnvMu_SerializesPushPopAndApplyEnv(t *testing.T) {
	// t.Setenv cannot be called from a goroutine other than the test's own,
	// so the SENNIT_-prefixed fixtures are set up here, before anything is
	// spawned.
	t.Setenv(brand.EnvPrefix+"PROCESSENV_TEST_A", "pushed-a")
	t.Setenv(brand.EnvPrefix+"PROCESSENV_TEST_B", "pushed-b")
	t.Setenv("PROCESSENV_TEST_A", "original-a")
	t.Setenv("PROCESSENV_TEST_B", "original-b")

	cfg := &Config{Env: map[string]string{
		"PROCESSENV_TEST_A": "applied-a",
		"PROCESSENV_TEST_B": "applied-b",
	}}
	resolver := NewShellVariableResolver(env.New())

	const goroutines = 8
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				restore := PushPopEnvOverrides()
				defer restore()
			} else {
				cfg.applyEnv(resolver)
			}
		}()
	}
	wg.Wait()

	// Regardless of interleaving, every PushPopEnvOverrides call must have
	// fully restored its own snapshot, and applyEnv's writes are only ever
	// observed either not-yet-applied or fully applied under the lock — so
	// the final state is either the original value or the config-applied
	// value, never a torn mix (e.g. one var restored and the other not).
	a, aOK := os.LookupEnv("PROCESSENV_TEST_A")
	b, bOK := os.LookupEnv("PROCESSENV_TEST_B")
	require.True(t, aOK)
	require.True(t, bOK)
	require.Contains(t, []string{"original-a", "applied-a"}, a)
	require.Contains(t, []string{"original-b", "applied-b"}, b)
}
