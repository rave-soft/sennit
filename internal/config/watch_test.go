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
// an agent's Edit/Write tool touching .sennit/sennit.json directly) is picked
// up by the poll loop, reloaded, and reported via OnExternalChange.
func TestWatchForExternalChanges_DetectsEditOfExistingFile(t *testing.T) {
	dir := t.TempDir()
	braidDir := filepath.Join(dir, ".sennit")
	require.NoError(t, os.MkdirAll(braidDir, 0o755))
	configPath := filepath.Join(braidDir, "sennit.json")

	t.Setenv("SENNIT_GLOBAL_CONFIG", dir)
	t.Setenv("SENNIT_GLOBAL_DATA", dir)

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
	t.Setenv("SENNIT_GLOBAL_CONFIG", dir)
	t.Setenv("SENNIT_GLOBAL_DATA", dir)

	configPath := filepath.Join(dir, "sennit.json")
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
// candidate at the last snapshot (a project's first .sennit/sennit.json,
// created mid-session) must still be detected once it appears.
func TestExternalChangeDetected_NewCandidateFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", dir)
	t.Setenv("SENNIT_GLOBAL_DATA", dir)

	store, err := Load(dir, "", false)
	require.NoError(t, err)
	require.False(t, store.externalChangeDetected())

	braidDir := filepath.Join(dir, ".sennit")
	require.NoError(t, os.MkdirAll(braidDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(braidDir, "sennit.json"), []byte(`{}`), 0o600))

	require.True(t, store.externalChangeDetected(),
		"a freshly-created .sennit/sennit.json should be detected even though it wasn't a tracked candidate before")
}

// TestWatchForExternalChanges_DetectsAgentFileChanges verifies that adding,
// editing, and removing a markdown subagent file (e.g. an agent's Write tool
// touching .sennit/agents/dev.md, or a human editing it directly) is picked
// up by the same poll loop that watches config files, and that cfg.Agents
// reflects the change after each reload.
func TestWatchForExternalChanges_DetectsAgentFileChanges(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", dir)
	t.Setenv("SENNIT_GLOBAL_DATA", dir)
	t.Setenv("ANTHROPIC_API_KEY", "test-key") // needed for cfg.IsConfigured(), which gates SetupAgents on reload.

	require.NoError(t, os.WriteFile(filepath.Join(dir, "sennit.json"), []byte(`{}`), 0o600))

	store, err := Load(dir, "", false)
	require.NoError(t, err)
	require.NotContains(t, store.Config().Agents, "dev")

	notified := make(chan struct{}, 8)
	store.OnExternalChange(func() {
		select {
		case notified <- struct{}{}:
		default:
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go store.WatchForExternalChanges(ctx)

	agentsDir := filepath.Join(dir, ".sennit", "agents")
	require.NoError(t, os.MkdirAll(agentsDir, 0o755))
	agentPath := filepath.Join(agentsDir, "dev.md")

	writeAgent := func(description string) {
		content := "---\nname: dev\ndescription: " + description + "\n---\nYou are a helpful dev agent.\n"
		require.NoError(t, os.WriteFile(agentPath, []byte(content), 0o600))
	}

	waitNotified := func(msg string) {
		t.Helper()
		select {
		case <-notified:
		case <-time.After(5 * time.Second):
			t.Fatal(msg)
		}
	}

	// Creating a brand new agent file.
	writeAgent("a dev agent")
	waitNotified("OnExternalChange was not invoked after a new agent file was added")
	require.Contains(t, store.Config().Agents, "dev")

	// Editing an existing agent file's frontmatter.
	time.Sleep(10 * time.Millisecond) // ensure a distinct mtime
	writeAgent("an updated dev agent")
	waitNotified("OnExternalChange was not invoked after the agent file was edited")
	require.Equal(t, "an updated dev agent", store.Config().Agents["dev"].Description)

	// Removing the agent file.
	require.NoError(t, os.Remove(agentPath))
	waitNotified("OnExternalChange was not invoked after the agent file was removed")
	require.NotContains(t, store.Config().Agents, "dev")
}

// TestWatchForExternalChanges_DetectsAgentDirCreatedLater verifies that an
// agent directory that does not exist when the watcher starts (a project's
// first .sennit/agents) is still picked up once it is created mid-session.
func TestWatchForExternalChanges_DetectsAgentDirCreatedLater(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", dir)
	t.Setenv("SENNIT_GLOBAL_DATA", dir)
	t.Setenv("ANTHROPIC_API_KEY", "test-key") // needed for cfg.IsConfigured(), which gates SetupAgents on reload.

	require.NoError(t, os.WriteFile(filepath.Join(dir, "sennit.json"), []byte(`{}`), 0o600))

	store, err := Load(dir, "", false)
	require.NoError(t, err)

	notified := make(chan struct{}, 1)
	store.OnExternalChange(func() {
		select {
		case notified <- struct{}{}:
		default:
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go store.WatchForExternalChanges(ctx)

	agentsDir := filepath.Join(dir, ".sennit", "agents")
	require.NoError(t, os.MkdirAll(agentsDir, 0o755))
	content := "---\nname: dev\ndescription: a dev agent\n---\nYou are a helpful dev agent.\n"
	require.NoError(t, os.WriteFile(filepath.Join(agentsDir, "dev.md"), []byte(content), 0o600))

	select {
	case <-notified:
	case <-time.After(5 * time.Second):
		t.Fatal("OnExternalChange was not invoked after the agents directory was created and populated")
	}
	require.Contains(t, store.Config().Agents, "dev")
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
