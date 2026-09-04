package lsp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/testenv"
	"github.com/stretchr/testify/require"
)

// TestClient_OpenFileConcurrentSendsSingleDidOpen pins down that two
// concurrent OpenFile calls for the same path send exactly one didOpen: the
// Get / send / Set sequence used to leave a window where both callers could
// observe the file as unopened and both notify the server, which some
// servers treat as a protocol error for an already-open document.
func TestClient_OpenFileConcurrentSendsSingleDidOpen(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	dir := t.TempDir()
	userFile := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(userFile, []byte("package main\n"), 0o644))
	logPath := filepath.Join(dir, "lsp.log")

	client, err := New("test-concurrent-open", config.LSPConfig{
		Command:   exe,
		FileTypes: []string{"go"},
		Env: map[string]string{
			fakeLSPServerEnv:      "1",
			"SENNIT_LSP_FAKE_LOG": logPath,
		},
	}, config.NewShellVariableResolver(testenv.New(map[string]string{})), dir, false)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err = client.Initialize(ctx, dir)
	require.NoError(t, err)
	require.NoError(t, client.WaitForServerReady(ctx))

	before, err := os.ReadFile(logPath)
	require.NoError(t, err)

	const callers = 20
	var wg sync.WaitGroup
	errs := make([]error, callers)
	wg.Add(callers)
	for i := range callers {
		go func(i int) {
			defer wg.Done()
			errs[i] = client.OpenFile(ctx, userFile)
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	require.True(t, client.IsFileOpen(userFile))

	// The fake server logs notifications as it reads them off the wire,
	// which happens on its own goroutine relative to this test, so give it
	// a moment to catch up before counting.
	countDidOpens := func() int {
		contents, err := os.ReadFile(logPath)
		require.NoError(t, err)
		didOpens := 0
		for _, line := range strings.Split(strings.TrimSpace(string(contents[len(before):])), "\n") {
			if strings.HasSuffix(line, " textDocument/didOpen") {
				didOpens++
			}
		}
		return didOpens
	}
	require.Eventually(t, func() bool { return countDidOpens() >= 1 }, 2*time.Second, 10*time.Millisecond)
	require.Equal(t, 1, countDidOpens(), "concurrent OpenFile calls for the same path must send exactly one didOpen")
	client.Shutdown()
}

// TestClient_RootMarkerIsTrackedLikeAnyOpenFile is the regression test for
// finding 1.2: prepareSyncOn opens a root marker (go.mod here) on the
// server but used to leave it out of f.files entirely, on the theory that
// markers are candidate-only bootstrap documents. That made IsFileOpen
// report false for a file the server plainly has open, which in turn made
// a later OpenFile call for the same path send a second didOpen for an
// already-open document — a protocol violation the LSP spec forbids.
func TestClient_RootMarkerIsTrackedLikeAnyOpenFile(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	dir := t.TempDir()
	goMod := filepath.Join(dir, "go.mod")
	require.NoError(t, os.WriteFile(goMod, []byte("module example.com/marker\n\ngo 1.24\n"), 0o644))
	logPath := filepath.Join(dir, "lsp.log")

	client, err := New("test-marker-tracked", config.LSPConfig{
		Command:     exe,
		FileTypes:   []string{"go"},
		RootMarkers: []string{"go.mod"},
		Env: map[string]string{
			fakeLSPServerEnv:      "1",
			"SENNIT_LSP_FAKE_LOG": logPath,
		},
	}, config.NewShellVariableResolver(testenv.New(map[string]string{})), dir, false)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err = client.Initialize(ctx, dir)
	require.NoError(t, err)
	require.NoError(t, client.WaitForServerReady(ctx))

	require.True(t, client.IsFileOpen(goMod),
		"a root marker the server has open must be reported open, the same as any other open file")

	countDidOpens := func() int {
		contents, err := os.ReadFile(logPath)
		require.NoError(t, err)
		didOpens := 0
		for _, line := range strings.Split(strings.TrimSpace(string(contents)), "\n") {
			if strings.HasSuffix(line, " textDocument/didOpen") {
				didOpens++
			}
		}
		return didOpens
	}
	require.Eventually(t, func() bool { return countDidOpens() >= 1 }, 2*time.Second, 10*time.Millisecond)
	before := countDidOpens()

	// A tool that reads or edits go.mod through the ordinary path calls
	// OpenFile on it. Since the marker is already open, this must be a
	// no-op — not a second didOpen for a document the server already has.
	require.NoError(t, client.OpenFile(ctx, goMod))
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, before, countDidOpens(),
		"OpenFile on an already-open root marker must not send a second didOpen")

	client.Shutdown()
}

// TestFilesync_OpenFileRetargetsToNewGenerationAfterConcurrentSwap is the
// regression test for finding 1.3: restart holds r.mu for its whole run,
// but openFile was never gated by it — it only serializes against other
// openFile calls for the same path, via f.files.GetOrSet, not against a
// restart in flight. So a restart's generation swap can land in the
// window between openFile reading the current generation and its didOpen
// finishing: the file's entry survives into the new generation's f.files
// (a restart's commit only overwrites its own snapshot, it never deletes
// what openFile concurrently added), so IsFileOpen reports true, but the
// new generation was never sent didOpen for it.
//
// Rather than trying to win a real timing race, this drives filesync
// directly with a stub gen() that returns two independently live fake
// servers (gen1 then gen2 on every later call) — deterministically
// simulating a swap landing exactly between openFile's initial didOpen and
// its post-check. Without the fix, only gen1's server ever sees the
// didOpen; gen2, the one that matters after the "swap", sees nothing.
func TestFilesync_OpenFileRetargetsToNewGenerationAfterConcurrentSwap(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(file, []byte("package main\n"), 0o644))

	newLiveClient := func(logPath string) *Client {
		t.Helper()
		c, err := New("test-swap-"+filepath.Base(logPath), config.LSPConfig{
			Command:   exe,
			FileTypes: []string{"go"},
			Env: map[string]string{
				fakeLSPServerEnv:      "1",
				"SENNIT_LSP_FAKE_LOG": logPath,
			},
		}, config.NewShellVariableResolver(testenv.New(map[string]string{})), dir, false)
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		t.Cleanup(cancel)
		_, err = c.Initialize(ctx, dir)
		require.NoError(t, err)
		require.NoError(t, c.WaitForServerReady(ctx))
		t.Cleanup(c.Kill)
		return c
	}

	log1 := filepath.Join(dir, "gen1.log")
	log2 := filepath.Join(dir, "gen2.log")
	c1 := newLiveClient(log1)
	c2 := newLiveClient(log2)
	gen1 := c1.runtime.currentGeneration()
	gen2 := c2.runtime.currentGeneration()

	calls := 0
	fsync := newFileSync(func() *clientGeneration {
		calls++
		if calls == 1 {
			return gen1
		}
		return gen2
	}, dir, "test-swap", []string{"go"}, nil, false)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, fsync.openFile(ctx, file))
	require.True(t, fsync.isFileOpen(file))

	countDidOpens := func(logPath string) int {
		contents, err := os.ReadFile(logPath)
		require.NoError(t, err)
		n := 0
		for _, line := range strings.Split(strings.TrimSpace(string(contents)), "\n") {
			if strings.HasSuffix(line, " textDocument/didOpen") {
				n++
			}
		}
		return n
	}
	require.Eventually(t, func() bool { return countDidOpens(log2) >= 1 }, 2*time.Second, 10*time.Millisecond,
		"gen2 — the generation current after the simulated swap — must receive didOpen for the file, not just gen1")
	require.Equal(t, 1, countDidOpens(log1), "gen1 must still get exactly the one didOpen from before the swap")
}

// TestClient_NotifyChangeOnDeletedFileClosesAndClearsDiagnostics is the
// regression test for finding 1.4: didClose was only ever sent from
// closeAllFiles at a graceful shutdown, so a file deleted outside the
// edit tools (git checkout, rm, another process) stayed registered as
// open forever — NotifyChange's read failed, filesync logged it and
// carried on, leaving the entry in f.files (IsFileOpen still true) and
// its last diagnostics sitting in project_diagnostics for the rest of
// the session.
func TestClient_NotifyChangeOnDeletedFileClosesAndClearsDiagnostics(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(file, []byte("package main\n"), 0o644))
	logPath := filepath.Join(dir, "lsp.log")

	client, err := New("test-vanish", config.LSPConfig{
		Command:   exe,
		FileTypes: []string{"go"},
		Env: map[string]string{
			fakeLSPServerEnv:      "1",
			"SENNIT_LSP_FAKE_LOG": logPath,
		},
	}, config.NewShellVariableResolver(testenv.New(map[string]string{})), dir, false)
	require.NoError(t, err)
	t.Cleanup(client.Kill)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err = client.Initialize(ctx, dir)
	require.NoError(t, err)
	require.NoError(t, client.WaitForServerReady(ctx))

	require.NoError(t, client.OpenFile(ctx, file))
	require.True(t, client.IsFileOpen(file))

	// protocol.URIFromPath, not a hand-built "file://"+path: on Windows a
	// path starts with a drive letter and the canonical URI carries a
	// third slash before it, so the hand-built form keys nothing the
	// store ever wrote and the diagnostics below come back empty.
	uri := string(protocol.URIFromPath(file))
	gen := client.runtime.currentGeneration()
	client.diagnostics.publish(gen, []byte(fmt.Sprintf(`{"uri":%q,"diagnostics":[{"message":"stale"}]}`, uri)))
	client.diagnostics.waitForDrain()
	require.NotEmpty(t, client.GetFileDiagnostics(protocol.DocumentURI(uri)), "diagnostics must be recorded before the file vanishes")

	require.NoError(t, os.Remove(file))

	err = client.NotifyChange(ctx, file)
	require.Error(t, err, "NotifyChange on a deleted file must still report the read failure")

	client.diagnostics.waitForDrain()
	require.False(t, client.IsFileOpen(file), "a vanished file must no longer be reported open")
	require.Empty(t, client.GetFileDiagnostics(protocol.DocumentURI(uri)), "diagnostics for a vanished file must be cleared, not linger")

	// The fake server logs notifications as it reads them off the wire, on
	// its own goroutine relative to this test, so give it a moment to
	// catch up before checking — see the identical pattern in
	// TestClient_OpenFileConcurrentSendsSingleDidOpen above.
	require.Eventually(t, func() bool {
		contents, err := os.ReadFile(logPath)
		require.NoError(t, err)
		return strings.Contains(string(contents), "textDocument/didClose")
	}, 2*time.Second, 10*time.Millisecond,
		"a vanished file must be closed on the server, the same as a graceful close would")
}
