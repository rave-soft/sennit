package cmd

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/config"
)

// discoveryModeTestCommand builds the minimal flag set effectiveDaemonMode
// reads, the same pattern trustTestCommand uses for initConfig -- a fresh
// *cobra.Command rather than the package's singleton rootCmd, so this
// test's flags can't race or leak state with any other test's.
func discoveryModeTestCommand() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().Bool("no-daemon", false, "")
	cmd.Flags().Bool("daemon", false, "")
	cmd.Flags().Bool("debug", false, "")
	cmd.Flags().String("data-dir", "", "")
	return cmd
}

// discoveringProviderConfig seeds a global config with disable_default_
// providers and one custom provider whose base_url points at srv and
// whose discover_models is explicitly true with no hand-written models --
// runDiscoveryRequests (internal/providerload/discover.go) fires an HTTP
// request for exactly this shape whenever a RuntimeProcessor actually
// runs. Used to prove effectiveDaemonMode and the daemon-mode prefs load
// never trigger it.
func discoveringProviderConfig(baseURL string) string {
	return fmt.Sprintf(`{
  "options": {"disable_default_providers": true, "daemon": {"mode": "auto"}},
  "providers": {"discoverme": {"id": "discoverme", "name": "DiscoverMe", "type": "openai",
    "base_url": %q, "api_key": "test-key", "discover_models": true}},
  "models": {"large": {"provider": "discoverme", "model": "whatever"},
             "small": {"provider": "discoverme", "model": "whatever"}}
}`, baseURL)
}

// countingDiscoveryServer is an httptest.Server standing in for a
// provider's /v1/models endpoint, counting every request it receives.
func countingDiscoveryServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

// TestEffectiveDaemonMode_NoProviderDiscovery is the regression test for
// the finding in PR 2.3a round 1: effectiveDaemonMode must read
// options.daemon.mode without ever running provider model discovery.
// Reintroducing configruntime.Load (the RuntimeProcessor-backed loader)
// here turns this red -- see this file's sibling test for the daemon-mode
// prefs-store half of the same finding.
func TestEffectiveDaemonMode_NoProviderDiscovery(t *testing.T) {
	srv, requests := countingDiscoveryServer(t)

	dir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", dir)
	t.Setenv("SENNIT_GLOBAL_DATA", filepath.Join(dir, "data"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sennit.json"), []byte(discoveringProviderConfig(srv.URL+"/v1")), 0o644))

	cwd := t.TempDir()
	mode, err := effectiveDaemonMode(discoveryModeTestCommand(), cwd)
	require.NoError(t, err)
	require.Equal(t, "auto", mode)
	require.Zero(t, requests.Load(), "effectiveDaemonMode must not trigger provider model discovery")
}

// TestSetupDaemonWorkspace_PrefsLoadDoesNoProviderDiscovery covers the
// other surface of the same finding: the client-side config load
// setupDaemonWorkspace does purely to build the UI prefs store must not
// repeat the discovery the daemon it just connected to already ran.
//
// This calls config.LoadData and uiPrefsStoreFromConfig directly --
// exactly the two calls setupDaemonWorkspace makes to build the prefs
// store -- rather than going through the whole EnsureRunning/daemon-spawn
// pipeline: EnsureRunning's own ResolveSocketPath call already loads
// config with the real RuntimeProcessor for an unrelated reason (finding
// the project's socket/lock directory) and would confound a "zero
// requests" assertion made across the whole pipeline with a request this
// fix has nothing to do with.
func TestSetupDaemonWorkspace_PrefsLoadDoesNoProviderDiscovery(t *testing.T) {
	srv, requests := countingDiscoveryServer(t)

	dir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", dir)
	t.Setenv("SENNIT_GLOBAL_DATA", filepath.Join(dir, "data"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sennit.json"), []byte(discoveringProviderConfig(srv.URL+"/v1")), 0o644))

	cwd := t.TempDir()
	cfgStore, err := config.LoadData(cwd, "", false)
	require.NoError(t, err)
	prefs := uiPrefsStoreFromConfig(cfgStore)

	require.NotNil(t, prefs)
	require.Zero(t, requests.Load(), "the daemon-mode prefs store's config load must not trigger provider model discovery")
}
