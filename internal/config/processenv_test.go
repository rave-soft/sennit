package config

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

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

// TestConfigureProviders_DiscoveryDoesNotHoldProcessEnvMu is a regression
// test for configureProviders holding processEnvMu across the whole call,
// including runDiscoveryRequests' HTTP round trip. Two workspaces loading
// concurrently (one process, one config store each — see processEnvMu's doc
// comment) used to have the second one block behind the first one's slow
// discovery endpoint for up to its full 3s timeout, just to push/pop its
// own env overrides. Resolving what discovery needs to send before
// releasing the lock (see resolveDiscoveryRequests) means the HTTP wait
// itself never needs it.
func TestConfigureProviders_DiscoveryDoesNotHoldProcessEnvMu(t *testing.T) {
	const serverDelay = 200 * time.Millisecond

	dir := t.TempDir()
	configPath := filepath.Join(dir, "sennit.json")
	t.Setenv("SENNIT_GLOBAL_CONFIG", dir)
	t.Setenv("SENNIT_GLOBAL_DATA", dir)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(serverDelay)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [{"id": "slow-model", "object": "model"}]}`))
	}))
	defer server.Close()

	// A custom provider with no models auto-triggers discovery against the
	// slow server on Load.
	slowConfig := fmt.Sprintf(`{
		"providers": {
			"custom": {
				"api_key": "test-key",
				"base_url": %q
			}
		}
	}`, server.URL+"/v1")
	require.NoError(t, os.WriteFile(configPath, []byte(slowConfig), 0o600))

	loadDone := make(chan error, 1)
	go func() {
		_, err := loadRuntimeForTest(dir, dir, false)
		loadDone <- err
	}()

	// Give the Load goroutine time to get past mergeCatalogProviders and
	// resolveDiscoveryRequests and into the HTTP wait, but stay well under
	// serverDelay so we're observing mid-discovery state.
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	restore := PushPopEnvOverrides()
	elapsed := time.Since(start)
	restore()

	// A strict, small bound rather than "less than serverDelay": the whole
	// point is that PushPopEnvOverrides returns essentially immediately,
	// not merely sooner than the full 200ms round trip.
	require.Less(t, elapsed, 50*time.Millisecond,
		"PushPopEnvOverrides should not block for the discovery HTTP round trip")

	require.NoError(t, <-loadDone)
}
