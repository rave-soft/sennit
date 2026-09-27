package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/daemon/supervisor"
	"github.com/rave-soft/sennit/internal/transport"
)

// writeGlobalConfigWithTheme is writeGlobalConfig (daemon_client_test.go)
// plus a distinctive options.tui.theme, so a test can tell "the client's
// own global config" apart from any project-scoped one by which theme
// comes back.
func writeGlobalConfigWithTheme(t *testing.T, theme string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", dir)
	t.Setenv("SENNIT_GLOBAL_DATA", filepath.Join(dir, "data"))
	cfg := fmt.Sprintf(`{
  "options": {"disable_default_providers": true, "tui": {"theme": %q}},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}
}`, theme)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sennit.json"), []byte(cfg), 0o644))
}

// TestConnectRemoteWorkspace_PrefsIgnoreLocalProjectConfig is CLIENT-
// SERVER.md's PR 3.1 test-plan item: with the client process's OWN
// current directory holding a trusted, different-themed project config,
// connectRemoteWorkspace's prefs store must still come back with the
// client's GLOBAL theme, never the local project's -- the project being
// worked on lives on the remote daemon's machine, so a project layer
// found under the client's own cwd names an unrelated project and must
// never leak in (config.LoadGlobalData's own doc comment; see also
// internal/config/load_global_test.go for the same property one layer
// down, without a daemon in the loop).
func TestConnectRemoteWorkspace_PrefsIgnoreLocalProjectConfig(t *testing.T) {
	writeGlobalConfigWithTheme(t, "global-theme")

	// The client's own cwd: an unrelated, trusted project with a
	// different theme configured locally.
	localProjectDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(localProjectDir, "sennit.json"),
		[]byte(`{"options":{"tui":{"theme":"local-project-theme"}}}`),
		0o644,
	))
	require.NoError(t, config.Trust(localProjectDir))

	// t.Chdir restores the directory itself and refuses to run in a
	// parallel test, where a process-wide chdir would leak into others.
	t.Chdir(localProjectDir)

	// The remote daemon serves a completely different directory -- what
	// actually matters here is that the CLIENT's cwd (above) is not the
	// one connectRemoteWorkspace consults for prefs at all.
	daemonProjectDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	ctx, cancel := context.WithTimeout(t.Context(), daemonTestTimeout)
	defer cancel()

	socketPath, _, err := supervisor.EnsureRunning(ctx, daemonProjectDir, supervisor.Options{Command: helperCommand(t)})
	require.NoError(t, err)
	t.Cleanup(func() { dialAndRequestShutdown(t, context.Background(), daemonProjectDir) })

	target := transport.Target{Host: "fake-remote-host", Path: daemonProjectDir}
	dialerOpts := transport.DialerOptions{Command: fakeSSHCommand(t, socketPath)}

	_, prefs, cleanup, err := connectRemoteWorkspace(ctx, target, dialerOpts, "", false)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	require.Equal(t, "global-theme", prefs.Prefs().ThemeID,
		"remote-mode prefs must come from the client's global config, never a local project layer under its cwd")
}
