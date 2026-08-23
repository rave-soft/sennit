package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	sennitDir := filepath.Join(dir, ".sennit")
	require.NoError(t, os.MkdirAll(sennitDir, 0o755))
	configPath := filepath.Join(sennitDir, "sennit.json")

	t.Setenv("SENNIT_GLOBAL_CONFIG", dir)
	t.Setenv("SENNIT_GLOBAL_DATA", dir)

	require.NoError(t, os.WriteFile(configPath, []byte(`{"mcp":{}}`), 0o600))

	store, err := Load(dir, "", false)
	require.NoError(t, err)
	require.Empty(t, store.Config().MCP)
	store.externalChangePollInterval = 100 * time.Millisecond

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
	case <-time.After(2 * time.Second):
		t.Fatal("OnExternalChange callback was not invoked after external edit")
	}

	_, ok := store.Config().MCP["added-by-agent"]
	require.True(t, ok, "expected the externally-added MCP server to be visible after reload")
}

// TestWatchForExternalChanges_IgnoresOwnWrites_TightPoll guards the race
// TECHDEBT.md used to record under "The external-change watcher can
// misread this process's own write": SetConfigFields used to write the
// file, then run the whole (possibly slow) autoReload pipeline, and only
// refresh the staleness snapshot at the very end of that pipeline. A poll
// landing in that window saw the file's new on-disk mtime against the
// still-old snapshot and read it as an external change — reproducible,
// per TECHDEBT.md, once the poll interval was driven down below ~100ms
// under -race (production's real interval, 2s, never gets close).
// SetConfigFields now refreshes the snapshot for the written path
// immediately after the atomic write, before autoReload runs, closing
// the window the reload pipeline used to leave open.
//
// This is TestWatchForExternalChanges_IgnoresOwnWrites's own scenario at
// the tighter interval TECHDEBT.md asked the fix to survive, repeated
// several times; run with -race for the intended amplification.
func TestWatchForExternalChanges_IgnoresOwnWrites_TightPoll(t *testing.T) {
	for range 5 {
		dir := t.TempDir()
		t.Setenv("SENNIT_GLOBAL_CONFIG", dir)
		t.Setenv("SENNIT_GLOBAL_DATA", dir)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "sennit.json"), []byte(`{}`), 0o600))

		store, err := Load(dir, "", false)
		require.NoError(t, err)
		store.externalChangePollInterval = 10 * time.Millisecond

		notified := make(chan struct{}, 8)
		store.OnExternalChange(func() {
			select {
			case notified <- struct{}{}:
			default:
			}
		})

		ctx, cancel := context.WithCancel(context.Background())
		go store.WatchForExternalChanges(ctx)

		require.NoError(t, store.SetConfigField(ScopeGlobal, "options.debug", true))

		select {
		case <-notified:
			cancel()
			t.Fatalf("WatchForExternalChanges fired for this process's own write:%s",
				describeExternalChange(t, store))
		case <-time.After(15 * store.externalChangePollInterval):
		}
		cancel()
	}
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
	const pollInterval = 100 * time.Millisecond
	store.externalChangePollInterval = pollInterval

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
		t.Fatalf("WatchForExternalChanges fired for this process's own write:%s",
			describeExternalChange(t, store))
	case <-time.After(3 * pollInterval):
	}
	require.Zero(t, notifications)
}

// TestWatchForExternalChanges_IgnoresOwnRemoveConfigField_TightPoll is
// TestWatchForExternalChanges_IgnoresOwnRemoveConfigField's own scenario at
// the tighter poll interval TestWatchForExternalChanges_IgnoresOwnWrites_TightPoll
// uses to reliably land a poll inside the write/reload window; run with
// -race for the intended amplification.
func TestWatchForExternalChanges_IgnoresOwnRemoveConfigField_TightPoll(t *testing.T) {
	for range 5 {
		dir := t.TempDir()
		t.Setenv("SENNIT_GLOBAL_CONFIG", dir)
		t.Setenv("SENNIT_GLOBAL_DATA", dir)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "sennit.json"), []byte(`{"options": {"debug": true}}`), 0o600))

		store, err := Load(dir, "", false)
		require.NoError(t, err)
		store.externalChangePollInterval = 10 * time.Millisecond

		notified := make(chan struct{}, 8)
		store.OnExternalChange(func() {
			select {
			case notified <- struct{}{}:
			default:
			}
		})

		ctx, cancel := context.WithCancel(context.Background())
		go store.WatchForExternalChanges(ctx)

		require.NoError(t, store.RemoveConfigField(ScopeGlobal, "options.debug"))

		select {
		case <-notified:
			cancel()
			t.Fatalf("WatchForExternalChanges fired for this process's own RemoveConfigField write:%s",
				describeExternalChange(t, store))
		case <-time.After(15 * store.externalChangePollInterval):
		}
		cancel()
	}
}

// TestWatchForExternalChanges_IgnoresOwnRemoveConfigField verifies that
// RemoveConfigField, like SetConfigFields, refreshes the staleness snapshot
// atomically with its write. Before this, RemoveConfigField wrote the file
// and reloaded without ever touching the snapshot itself (relying on the
// reload pipeline to eventually do it, same gap
// TestWatchForExternalChanges_IgnoresOwnWrites_TightPoll documents for the
// old SetConfigFields), so a poll landing in that window read this
// process's own deletion as an external change and fired a redundant
// reload/notification.
func TestWatchForExternalChanges_IgnoresOwnRemoveConfigField(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", dir)
	t.Setenv("SENNIT_GLOBAL_DATA", dir)

	configPath := filepath.Join(dir, "sennit.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{"options": {"debug": true}}`), 0o600))

	store, err := Load(dir, "", false)
	require.NoError(t, err)
	const pollInterval = 100 * time.Millisecond
	store.externalChangePollInterval = pollInterval

	var notifications int
	notified := make(chan struct{}, 8)
	store.OnExternalChange(func() {
		notifications++
		notified <- struct{}{}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go store.WatchForExternalChanges(ctx)

	require.NoError(t, store.RemoveConfigField(ScopeGlobal, "options.debug"))

	// Give the poll loop a few cycles to (not) fire.
	select {
	case <-notified:
		t.Fatalf("WatchForExternalChanges fired for this process's own write:%s",
			describeExternalChange(t, store))
	case <-time.After(3 * pollInterval):
	}
	require.Zero(t, notifications)
}

// describeExternalChange reports why externalChangeDetected() is true, so a
// failure on a platform we cannot run locally arrives with the answer
// attached instead of sending us back for another CI round. Three rounds of
// reasoning about this from a Linux box have not found it; see the "still
// open" entry in TECHDEBT.md.
func describeExternalChange(t *testing.T, s *ConfigStore) string {
	t.Helper()
	staleness := s.ConfigStaleness()
	tracked := s.trackedConfigPathSet()
	trackedList := make([]string, 0, len(tracked))
	for p := range tracked {
		trackedList = append(trackedList, p)
	}
	sort.Strings(trackedList)

	var untracked []string
	for _, p := range lookupConfigs(s.workingDir) {
		abs, err := filepath.Abs(p)
		if err != nil {
			untracked = append(untracked, fmt.Sprintf("%s (Abs failed: %v)", p, err))
			continue
		}
		if _, ok := tracked[abs]; !ok {
			untracked = append(untracked, abs)
		}
	}

	return fmt.Sprintf(
		"\n  staleness.Dirty=%v\n  workingDir=%q\n  workspacePath=%q\n  globalDataPath=%q"+
			"\n  untracked candidates=%q\n  tracked=%q\n  agentFilesChanged=%v",
		staleness.Dirty, s.workingDir, s.workspacePath.Get(), s.globalDataPath,
		untracked, trackedList, s.agentFilesChanged())
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
	require.False(t, store.externalChangeDetected(),
		"a freshly loaded store reports an external change before anything changed:%s",
		describeExternalChange(t, store))

	sennitDir := filepath.Join(dir, ".sennit")
	require.NoError(t, os.MkdirAll(sennitDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sennitDir, "sennit.json"), []byte(`{}`), 0o600))

	require.True(t, store.externalChangeDetected(),
		"a freshly-created .sennit/sennit.json should be detected even though it wasn't a tracked candidate before")
}

// TestHasUntrackedCandidate_EmptyPathIgnored pins a Windows-only production
// bug from Linux: systemConfigPath is "" on Windows (config_windows.go —
// there is no system-wide config there), so on that platform lookupConfigs
// used to hand externalChangeDetected a candidate list containing "".
// filepath.Abs("") silently resolves to the process's working directory
// rather than erroring, and the cwd is never a tracked config path, so the
// old code reported an external change on every single poll — a permanent
// 2-second reload/MCP-reinit loop for every Windows user.
//
// systemConfigPath is a per-OS const, so this test cannot reproduce the bug
// by loading a real store on Linux; instead it exercises the exact defect
// shape directly, via hasUntrackedCandidate, which both lookupConfigs (by
// filtering "" out of globalConfigPaths, see globalonly.go) and this
// function's own empty check now guard against.
func TestHasUntrackedCandidate_EmptyPathIgnored(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", dir)
	t.Setenv("SENNIT_GLOBAL_DATA", dir)

	store, err := Load(dir, "", false)
	require.NoError(t, err)

	require.False(t, store.hasUntrackedCandidate([]string{""}),
		"an empty candidate path must never be treated as untracked; "+
			"filepath.Abs(\"\") resolves to the cwd (%q), which is never a tracked config path",
		mustGetwd(t))
}

// TestGlobalConfigPaths_NoEmptyCandidate is the general invariant that
// would have caught the Windows bug at its source: an empty string is
// never a config path and must never enter the candidate lists that feed
// isGlobalConfigPath, lookupConfigs, and externalChangeDetected.
func TestGlobalConfigPaths_NoEmptyCandidate(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", dir)
	t.Setenv("SENNIT_GLOBAL_DATA", dir)

	require.NotContains(t, globalConfigPaths(), "")
	require.NotContains(t, lookupConfigs(dir), "")
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	return wd
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
	store.externalChangePollInterval = 100 * time.Millisecond

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
		case <-time.After(2 * time.Second):
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
	store.externalChangePollInterval = 100 * time.Millisecond

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
	case <-time.After(2 * time.Second):
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
