package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestWatchForExternalChanges_DetectsEditOfExistingFile verifies that an
// edit made to a config file outside of ConfigStore's own write path (e.g.
// an agent's Edit/Write tool touching .braid/braid.json directly) is picked
// up by the poll loop, reloaded, and reported via OnExternalChange.
func TestWatchForExternalChanges_DetectsEditOfExistingFile(t *testing.T) {
	dir := t.TempDir()
	braidDir := filepath.Join(dir, ".braid")
	require.NoError(t, os.MkdirAll(braidDir, 0o755))
	configPath := filepath.Join(braidDir, "braid.json")

	t.Setenv("BRAID_GLOBAL_CONFIG", dir)
	t.Setenv("BRAID_GLOBAL_DATA", dir)

	require.NoError(t, os.WriteFile(configPath, []byte(`{"mcp":{}}`), 0o600))

	store, err := Load(dir, "", false)
	require.NoError(t, err)
	require.Empty(t, store.Config().MCP)

	notified := make(chan struct{}, 1)
	store.OnExternalChange(func() { notified <- struct{}{} })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go store.WatchForExternalChanges(ctx)

	// Simulate an external process (or the agent's Write tool) adding an
	// MCP server directly on disk, bypassing SetConfigFields entirely.
	time.Sleep(10 * time.Millisecond) // ensure a distinct mtime
	require.NoError(t, os.WriteFile(configPath,
		[]byte(`{"mcp":{"added-by-agent":{"command":"echo"}}}`), 0o600))

	select {
	case <-notified:
	case <-time.After(5 * time.Second):
		t.Fatal("OnExternalChange callback was not invoked after external edit")
	}

	_, ok := store.Config().MCP["added-by-agent"]
	require.True(t, ok, "expected the externally-added MCP server to be visible after reload")
}

// TestWatchForExternalChanges_IgnoresOwnWrites verifies that a write made
// through SetConfigFields (which already reloads synchronously and
// refreshes the staleness snapshot) does not also trigger a second,
// watcher-driven reload/notification.
func TestWatchForExternalChanges_IgnoresOwnWrites(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRAID_GLOBAL_CONFIG", dir)
	t.Setenv("BRAID_GLOBAL_DATA", dir)

	configPath := filepath.Join(dir, "braid.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{}`), 0o600))

	store, err := Load(dir, "", false)
	require.NoError(t, err)

	var notifications int
	notified := make(chan struct{}, 8)
	store.OnExternalChange(func() {
		notifications++
		notified <- struct{}{}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go store.WatchForExternalChanges(ctx)

	require.NoError(t, store.SetConfigField(ScopeGlobal, "options.debug", true))

	// Give the poll loop a few cycles to (not) fire.
	select {
	case <-notified:
		t.Fatal("WatchForExternalChanges fired for this process's own write")
	case <-time.After(3 * externalChangePollInterval):
	}
	require.Zero(t, notifications)
}

// TestExternalChangeDetected_NewCandidateFile verifies the gap ConfigStaleness
// alone can't cover: a config file that did not exist as a tracked
// candidate at the last snapshot (a project's first .braid/braid.json,
// created mid-session) must still be detected once it appears.
func TestExternalChangeDetected_NewCandidateFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRAID_GLOBAL_CONFIG", dir)
	t.Setenv("BRAID_GLOBAL_DATA", dir)

	store, err := Load(dir, "", false)
	require.NoError(t, err)
	require.False(t, store.externalChangeDetected())

	braidDir := filepath.Join(dir, ".braid")
	require.NoError(t, os.MkdirAll(braidDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(braidDir, "braid.json"), []byte(`{}`), 0o600))

	require.True(t, store.externalChangeDetected(),
		"a freshly-created .braid/braid.json should be detected even though it wasn't a tracked candidate before")
}

// TestWatchForExternalChanges_NoWorkingDirNoops verifies the guard that
// keeps WatchForExternalChanges from spinning a poll loop on a bare
// ConfigStore with no working directory (e.g. in tests).
func TestWatchForExternalChanges_NoWorkingDirNoops(t *testing.T) {
	store := &ConfigStore{config: &Config{}}

	done := make(chan struct{})
	go func() {
		store.WatchForExternalChanges(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("WatchForExternalChanges did not return immediately when workingDir is empty")
	}
}
