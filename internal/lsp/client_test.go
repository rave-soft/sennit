package lsp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/testenv"
	"github.com/stretchr/testify/require"
)

func TestClient(t *testing.T) {
	// Create a simple config for testing
	cfg := config.LSPConfig{
		Command:   "$THE_CMD", // Use echo as a dummy command that won't fail
		Args:      []string{"hello"},
		FileTypes: []string{"go"},
		Env:       map[string]string{},
	}

	// Test creating a powernap client - this will likely fail with echo
	// but we can still test the basic structure
	client, err := New("test", cfg, config.NewShellVariableResolver(testenv.New(map[string]string{
		"THE_CMD": "echo",
	})), ".", false)
	if err != nil {
		// Expected to fail with echo command, skip the rest
		t.Skipf("Powernap client creation failed as expected with dummy command: %v", err)
		return
	}

	// If we get here, test basic interface methods
	if client.GetName() != "test" {
		t.Errorf("Expected name 'test', got '%s'", client.GetName())
	}

	if !client.HandlesFile("test.go") {
		t.Error("Expected client to handle .go files")
	}

	if client.HandlesFile("test.py") {
		t.Error("Expected client to not handle .py files")
	}

	// Test server state
	client.SetServerState(StateReady)
	if client.GetServerState() != StateReady {
		t.Error("Expected server state to be StateReady")
	}

	// Clean up - expect this to fail with echo command
	if err := client.Close(t.Context()); err != nil {
		// Expected to fail with echo command
		t.Logf("Close failed as expected with dummy command: %v", err)
	}
}

// TestNew_ExpansionFailure_Args pins that a failing $(cmd) in LSP
// args surfaces as a load error prefixed "invalid lsp args:" and that
// no client is returned. Mirrors the MCP contract where expansion
// failure hard-stops transport creation rather than silently running
// with an empty or literal value.
func TestNew_ExpansionFailure_Args(t *testing.T) {
	t.Parallel()

	cfg := config.LSPConfig{
		Command: "echo",
		Args:    []string{"--root", "$(false)"},
	}
	resolver := config.NewShellVariableResolver(testenv.New(map[string]string{}))

	client, err := New("test-args-fail", cfg, resolver, ".", false)
	require.Error(t, err)
	require.Nil(t, client, "client must not start when args expansion fails")
	require.Contains(t, err.Error(), "invalid lsp args")
}

// TestNew_ExpansionFailure_Env pins the same contract for env values.
func TestNew_ExpansionFailure_Env(t *testing.T) {
	t.Parallel()

	cfg := config.LSPConfig{
		Command: "echo",
		Env:     map[string]string{"BAD": "$(false)"},
	}
	resolver := config.NewShellVariableResolver(testenv.New(map[string]string{}))

	client, err := New("test-env-fail", cfg, resolver, ".", false)
	require.Error(t, err)
	require.Nil(t, client, "client must not start when env expansion fails")
	require.Contains(t, err.Error(), "invalid lsp env")
}

func TestNilClient(t *testing.T) {
	t.Parallel()

	var c *Client

	require.False(t, c.HandlesFile("/some/file.go"))
	require.Equal(t, DiagnosticCounts{}, c.GetDiagnosticCounts())
	require.Nil(t, c.GetDiagnostics())
	require.Nil(t, c.OpenFileOnDemand(context.Background(), "/some/file.go"))
	require.Nil(t, c.NotifyChange(context.Background(), "/some/file.go"))
	c.WaitForDiagnostics(context.Background(), time.Second)
}

// TestClient_Restart_ReopensPreviouslyOpenFiles pins that Restart reopens
// files that were open before it ran. Before the fix, Restart collected
// c.openFiles' keys (URIs, e.g. "file:///...") and fed them straight to
// OpenFile, which expects a filesystem path and checks it via
// HandlesFile/fsext.HasPrefix(path, c.cwd) — a URI never has cwd as a
// prefix, so the reopen silently no-op'd every time. The "server" here is
// the test binary itself, re-exec'd as a minimal fake LSP server (see
// fakeserver_test.go), so Restart runs its real Close/Initialize/
// WaitForServerReady sequence rather than a stub.
func TestClient_Restart_ReopensPreviouslyOpenFiles(t *testing.T) {
	t.Parallel()

	exe, err := os.Executable()
	require.NoError(t, err)

	dir := t.TempDir()
	filePath := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(filePath, []byte("package main\n"), 0o644))

	cfg := config.LSPConfig{
		Command:   exe,
		FileTypes: []string{"go"},
		Env:       map[string]string{fakeLSPServerEnv: "1"},
	}
	resolver := config.NewShellVariableResolver(testenv.New(map[string]string{}))

	client, err := New("test-restart", cfg, resolver, dir, false)
	require.NoError(t, err)
	t.Cleanup(client.Kill)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, err = client.Initialize(ctx, dir)
	require.NoError(t, err)
	require.NoError(t, client.WaitForServerReady(ctx))

	require.NoError(t, client.OpenFile(ctx, filePath))
	require.True(t, client.IsFileOpen(filePath), "file should be open before restart")

	oldGen := client.currentGeneration()
	client.handleDiagnostics(oldGen, []byte(`{"uri":"file:///stale.go","diagnostics":[{"message":"stale"}]}`))
	client.waitForDiagnosticEvents()
	require.NotEmpty(t, client.GetDiagnostics())

	require.NoError(t, client.Restart())
	require.True(t, client.IsFileOpen(filePath), "file should be reopened after restart")
	require.Empty(t, client.GetDiagnostics())
}

// TestClient_ConcurrentVersionBumpsDoNotRace exercises NotifyChange and
// RefreshOpenFiles bumping the same open file's version concurrently (e.g.
// a debounced edit notification racing a workspace-wide refresh). Both read
// then write *OpenFileInfo.Version through the same shared pointer, so a
// plain int32 field is a data race; run with -race.
func TestClient_ShutdownPreventsRestart(t *testing.T) {
	client := newFakeRuntimeClient(t, "test-shutdown-restart")
	gen := client.currentGeneration()

	client.Shutdown()

	require.ErrorIs(t, client.Restart(), errClientShutdown)
	require.Same(t, gen, client.currentGeneration())
}

func TestClient_RestartThenShutdownIsTerminal(t *testing.T) {
	client := newFakeRuntimeClient(t, "test-restart-shutdown")
	oldGen := client.currentGeneration()

	require.NoError(t, client.Restart())
	newGen := client.currentGeneration()
	require.NotSame(t, oldGen, newGen)

	client.Shutdown()
	require.ErrorIs(t, client.Restart(), errClientShutdown)
	require.Same(t, newGen, client.currentGeneration())
	require.Error(t, newGen.ctx.Err())
}

func newFakeRuntimeClient(t *testing.T, name string) *Client {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	dir := t.TempDir()
	cfg := config.LSPConfig{
		Command: exe,
		Env:     map[string]string{fakeLSPServerEnv: "1"},
	}
	resolver := config.NewShellVariableResolver(testenv.New(map[string]string{}))
	client, err := New(name, cfg, resolver, dir, false)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	_, err = client.Initialize(ctx, dir)
	require.NoError(t, err)
	require.NoError(t, client.WaitForServerReady(ctx))
	return client
}

func TestClient_LifecycleConcurrentRequests(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)

	dir := t.TempDir()
	filePath := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(filePath, []byte("package main\n"), 0o644))
	cfg := config.LSPConfig{
		Command:   exe,
		FileTypes: []string{"go"},
		Env: map[string]string{
			fakeLSPServerEnv:           "1",
			"SENNIT_LSP_FAKE_SCENARIO": "symbols",
		},
	}
	resolver := config.NewShellVariableResolver(testenv.New(map[string]string{}))
	client, err := New("test-lifecycle-race", cfg, resolver, dir, false)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err = client.Initialize(ctx, dir)
	require.NoError(t, err)
	require.NoError(t, client.WaitForServerReady(ctx))
	oldGen := client.currentGeneration()

	var wg sync.WaitGroup
	wg.Add(4)
	go func() {
		defer wg.Done()
		for range 20 {
			_, _ = client.Hover(ctx, filePath, 0, 0)
		}
	}()
	go func() {
		defer wg.Done()
		for range 100 {
			client.handleDiagnostics(oldGen, []byte(`{"uri":"file:///stale.go","diagnostics":[{"message":"stale"}]}`))
		}
	}()
	restarted := make(chan struct{})
	go func() {
		defer wg.Done()
		require.NoError(t, client.Restart())
		close(restarted)
	}()
	go func() {
		defer wg.Done()
		<-restarted
		client.Shutdown()
	}()
	wg.Wait()

	require.NotSame(t, oldGen, client.currentGeneration())
	require.ErrorIs(t, client.Restart(), errClientShutdown)
	require.Empty(t, client.GetDiagnostics())
}

func TestClient_ConcurrentVersionBumpsDoNotRace(t *testing.T) {
	t.Parallel()

	exe, err := os.Executable()
	require.NoError(t, err)

	dir := t.TempDir()
	filePath := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(filePath, []byte("package main\n"), 0o644))

	cfg := config.LSPConfig{
		Command:   exe,
		FileTypes: []string{"go"},
		Env:       map[string]string{fakeLSPServerEnv: "1"},
	}
	resolver := config.NewShellVariableResolver(testenv.New(map[string]string{}))

	client, err := New("test-version-race", cfg, resolver, dir, false)
	require.NoError(t, err)
	t.Cleanup(client.Kill)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, err = client.Initialize(ctx, dir)
	require.NoError(t, err)
	require.NoError(t, client.WaitForServerReady(ctx))
	require.NoError(t, client.OpenFile(ctx, filePath))

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			require.NoError(t, client.NotifyChange(ctx, filePath))
		})
		wg.Go(func() {
			client.RefreshOpenFiles(ctx)
		})
	}
	wg.Wait()
}

// TestClient_HandlesFile_RejectsURIAcceptsPath documents the contract
// OpenFile/HandlesFile rely on: HandlesFile takes a filesystem path, not a
// URI, since it compares against c.cwd via fsext.HasPrefix. A "file://"
// URI never has cwd as a prefix, which is exactly the mismatch that made
// Restart's reopen loop a silent no-op before it converted URIs back to
// paths.
func TestClient_HandlesFile_RejectsURIAcceptsPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(filePath, []byte("package main\n"), 0o644))

	c := newTestClient()
	c.cwd = dir
	c.fileTypes = []string{"go"}

	uri := string(protocol.URIFromPath(filePath))
	require.False(t, c.HandlesFile(uri), "HandlesFile must reject a raw URI")
	require.True(t, c.HandlesFile(filePath), "HandlesFile must accept the converted path")

	path, err := protocol.DocumentURI(uri).Path()
	require.NoError(t, err)
	require.Equal(t, filePath, path)
}

func newTestClient() *Client {
	c := &Client{
		name:        "test",
		diagnostics: csync.NewVersionedMap[protocol.DocumentURI, []protocol.Diagnostic](),
		openFiles:   csync.NewMap[string, *OpenFileInfo](),
	}
	c.serverState.Store(StateStopped)
	gen := &clientGeneration{}
	c.generation.Store(gen)
	c.diagnosticGeneration = gen
	c.startDiagnosticDispatcher()
	return c
}

func TestClient_DiagnosticsResetConcurrent(t *testing.T) {
	c := newTestClient()
	const iterations = 1000

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for range iterations {
			HandleDiagnostics(c, []byte(`{"uri":"file:///test.go","diagnostics":[]}`))
		}
	}()
	go func() {
		defer wg.Done()
		for range iterations {
			c.resetDiagnostics()
		}
	}()
	go func() {
		defer wg.Done()
		for range iterations {
			_ = c.GetDiagnostics()
			_ = c.GetDiagnosticCounts()
		}
	}()
	wg.Wait()
}

func TestClient_StaleDiagnosticsBlockedAcrossGenerationPublication(t *testing.T) {
	c := newTestClient()
	oldGen := c.currentGeneration()
	newGen := &clientGeneration{}
	entered := make(chan struct{})
	proceed := make(chan struct{})
	finished := make(chan struct{})
	counts := make(chan int, 2)
	c.SetDiagnosticsCallback(func(_ string, count int) {
		counts <- count
	})

	go func() {
		defer close(finished)
		c.applyDiagnostics(oldGen, protocol.PublishDiagnosticsParams{
			URI:         protocol.DocumentURI("file:///stale.go"),
			Diagnostics: []protocol.Diagnostic{{Message: "stale"}},
		}, func() {
			close(entered)
			<-proceed
		})
	}()

	<-entered
	published := make(chan struct{})
	go func() {
		c.publishGeneration(newGen)
		close(published)
	}()
	<-published
	close(proceed)
	<-finished

	require.Empty(t, c.GetDiagnostics())
	require.Equal(t, []int{1, 0}, []int{<-counts, <-counts})
	require.Same(t, newGen, c.currentGeneration())
}

func TestClient_DiagnosticsPublicationWinsBeforeEventStartCommit(t *testing.T) {
	c := newTestClient()
	oldGen := c.currentGeneration()
	newGen := &clientGeneration{}
	popped := make(chan struct{})
	proceed := make(chan struct{})
	counts := make(chan int, 1)
	var once sync.Once
	c.beforeDiagnosticEventCommit = func() {
		once.Do(func() {
			close(popped)
			<-proceed
		})
	}
	c.SetDiagnosticsCallback(func(_ string, count int) {
		counts <- count
	})

	c.handleDiagnostics(oldGen, []byte(`{"uri":"file:///test.go","diagnostics":[{"message":"old"}]}`))
	<-popped
	c.publishGeneration(newGen)
	close(proceed)

	c.waitForDiagnosticEvents()
	require.Empty(t, c.GetDiagnostics())
	select {
	case count := <-counts:
		t.Fatalf("callback started after publication with count %d", count)
	default:
	}
}

func TestClient_DiagnosticsPublicationOrdersResetAfterRunningCallback(t *testing.T) {
	c := newTestClient()
	oldGen := c.currentGeneration()
	newGen := &clientGeneration{}
	started := make(chan struct{})
	release := make(chan struct{})
	counts := make(chan int, 2)
	c.SetDiagnosticsCallback(func(_ string, count int) {
		if count != 0 {
			close(started)
			<-release
		}
		counts <- count
	})

	c.handleDiagnostics(oldGen, []byte(`{"uri":"file:///test.go","diagnostics":[{"message":"old"}]}`))
	<-started
	c.publishGeneration(newGen)

	require.Same(t, newGen, c.currentGeneration())
	require.Empty(t, c.GetDiagnostics())
	close(release)
	require.Equal(t, []int{1, 0}, []int{<-counts, <-counts})
}

func TestClient_DiagnosticsQueuedBeforePublicationAreDropped(t *testing.T) {
	c := newTestClient()
	oldGen := c.currentGeneration()
	newGen := &clientGeneration{}
	started := make(chan struct{})
	release := make(chan struct{})
	counts := make(chan int, 2)
	c.SetDiagnosticsCallback(func(_ string, count int) {
		if count != 0 {
			close(started)
			<-release
		}
		counts <- count
	})

	c.handleDiagnostics(oldGen, []byte(`{"uri":"file:///first.go","diagnostics":[{"message":"first"}]}`))
	<-started
	c.handleDiagnostics(oldGen, []byte(`{"uri":"file:///queued.go","diagnostics":[{"message":"queued"}]}`))
	c.publishGeneration(newGen)
	close(release)

	require.Equal(t, []int{1, 0}, []int{<-counts, <-counts})
	c.waitForDiagnosticEvents()
	select {
	case count := <-counts:
		t.Fatalf("queued stale callback delivered count %d", count)
	default:
	}
}

func TestClient_ShutdownPurgesQueuedDiagnosticsAndStopsDispatcher(t *testing.T) {
	client := newFakeRuntimeClient(t, "test-shutdown-queue")
	gen := client.currentGeneration()
	started := make(chan struct{})
	release := make(chan struct{})
	counts := make(chan int, 3)
	client.SetDiagnosticsCallback(func(_ string, count int) {
		if count != 0 {
			select {
			case <-started:
			default:
				close(started)
				<-release
			}
		}
		counts <- count
	})

	client.handleDiagnostics(gen, []byte(`{"uri":"file:///running.go","diagnostics":[{"message":"running"}]}`))
	<-started
	client.handleDiagnostics(gen, []byte(`{"uri":"file:///queued.go","diagnostics":[{"message":"queued"}]}`))
	shutdownDone := make(chan struct{})
	go func() {
		client.Shutdown()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown waited for running diagnostics callback")
	}
	close(release)
	<-client.diagnosticEventsDone

	require.Empty(t, client.GetDiagnostics())
	require.Equal(t, []int{1, 0}, []int{<-counts, <-counts})
	select {
	case count := <-counts:
		t.Fatalf("queued diagnostic callback delivered count %d", count)
	default:
	}
}

func TestClient_DiagnosticsCallbackCanRestart(t *testing.T) {
	client := newFakeRuntimeClient(t, "test-callback-restart")
	oldGen := client.currentGeneration()
	finished := make(chan error, 1)
	client.SetDiagnosticsCallback(func(_ string, count int) {
		if count != 0 {
			finished <- client.Restart()
		}
	})

	go client.handleDiagnostics(oldGen, []byte(`{"uri":"file:///test.go","diagnostics":[{"message":"restart"}]}`))

	select {
	case err := <-finished:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("diagnostics callback deadlocked while restarting")
	}
	require.NotSame(t, oldGen, client.currentGeneration())
	require.Empty(t, client.GetDiagnostics())
	client.Shutdown()
}

func TestClient_DiagnosticsCallbackCanShutdown(t *testing.T) {
	client := newFakeRuntimeClient(t, "test-callback-shutdown")
	gen := client.currentGeneration()
	finished := make(chan struct{}, 1)
	client.SetDiagnosticsCallback(func(_ string, count int) {
		if count != 0 {
			client.Shutdown()
			finished <- struct{}{}
		}
	})

	client.handleDiagnostics(gen, []byte(`{"uri":"file:///test.go","diagnostics":[{"message":"shutdown"}]}`))

	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("diagnostics callback deadlocked while shutting down")
	}
	require.ErrorIs(t, client.Restart(), errClientShutdown)
	require.Error(t, gen.ctx.Err())
}

func TestClient_DiagnosticsCallbackConcurrent(t *testing.T) {
	c := newTestClient()
	const iterations = 1000

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range iterations {
			c.SetDiagnosticsCallback(func(string, int) {})
		}
	}()
	go func() {
		defer wg.Done()
		for range iterations {
			HandleDiagnostics(c, []byte(`{"uri":"file:///test.go","diagnostics":[]}`))
		}
	}()
	wg.Wait()
}

func TestWaitForDiagnostics_ConcurrentReset(t *testing.T) {
	c := newTestClient()
	const iterations = 1000

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range iterations {
			c.waitForDiagnostics(t.Context(), time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond)
		}
	}()
	go func() {
		defer wg.Done()
		for range iterations {
			c.resetDiagnostics()
		}
	}()
	wg.Wait()
}

func TestWaitForDiagnostics_NoChange(t *testing.T) {
	t.Parallel()

	c := newTestClient()
	start := time.Now()
	c.waitForDiagnostics(t.Context(), time.Second, 50*time.Millisecond, 30*time.Millisecond, 5*time.Millisecond)
	elapsed := time.Since(start)

	require.Less(t, elapsed, 200*time.Millisecond, "should return early when no diagnostics change")
}

func TestWaitForDiagnostics_ImmediateChange(t *testing.T) {
	t.Parallel()

	c := newTestClient()

	go func() {
		time.Sleep(20 * time.Millisecond)
		c.diagnostics.Set(protocol.DocumentURI("file:///test.go"), nil)
	}()

	start := time.Now()
	c.waitForDiagnostics(t.Context(), time.Second, 100*time.Millisecond, 30*time.Millisecond, 5*time.Millisecond)
	elapsed := time.Since(start)

	require.Less(t, elapsed, 200*time.Millisecond, "should return after settling, not full timeout")
	require.Greater(t, elapsed, 30*time.Millisecond, "should wait for settle duration")
}

func TestWaitForDiagnostics_RepeatedChanges(t *testing.T) {
	t.Parallel()

	c := newTestClient()

	// Simulate an LSP server that publishes diagnostics in bursts.
	go func() {
		for i := range 5 {
			time.Sleep(10 * time.Millisecond)
			c.diagnostics.Set(protocol.DocumentURI("file:///test.go"), []protocol.Diagnostic{
				{Message: fmt.Sprintf("diag-%d", i)},
			})
		}
	}()

	start := time.Now()
	c.waitForDiagnostics(t.Context(), time.Second, 100*time.Millisecond, 30*time.Millisecond, 5*time.Millisecond)
	elapsed := time.Since(start)

	// Should wait for diagnostics to settle after the burst finishes.
	require.Less(t, elapsed, 250*time.Millisecond, "should return after settling, not full timeout")
	require.Greater(t, elapsed, 60*time.Millisecond, "should wait for all changes to settle")
}

func TestWaitForDiagnostics_ContextCancellation(t *testing.T) {
	t.Parallel()

	c := newTestClient()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	c.waitForDiagnostics(ctx, time.Second, 100*time.Millisecond, 30*time.Millisecond, 5*time.Millisecond)
	elapsed := time.Since(start)

	require.Less(t, elapsed, 200*time.Millisecond, "should return shortly after context cancellation")
}

func TestWaitForDiagnostics_NilClient(t *testing.T) {
	t.Parallel()

	var c *Client
	// Should not panic.
	c.WaitForDiagnostics(context.Background(), time.Second)
}

// TestClient_CloseAllFiles_DropsBookkeepingWhenTheServerIsDead pins the
// state Restart depends on. Every didClose fails against a server that is
// no longer answering — which is exactly when a restart happens — and the
// bookkeeping used to be kept on a failed close. The URIs stayed in
// c.openFiles, so Restart's reopen loop hit OpenFile's already-open check
// for every one of them and the fresh server received not a single
// didOpen: an LSP that came back alive and blind.
func TestClient_CloseAllFiles_DropsBookkeepingWhenTheServerIsDead(t *testing.T) {
	t.Parallel()

	exe, err := os.Executable()
	require.NoError(t, err)

	dir := t.TempDir()
	filePath := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(filePath, []byte("package main\n"), 0o644))

	cfg := config.LSPConfig{
		Command:   exe,
		FileTypes: []string{"go"},
		Env:       map[string]string{fakeLSPServerEnv: "1"},
	}
	resolver := config.NewShellVariableResolver(testenv.New(map[string]string{}))

	client, err := New("test-close-dead", cfg, resolver, dir, false)
	require.NoError(t, err)
	t.Cleanup(client.Kill)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	_, err = client.Initialize(ctx, dir)
	require.NoError(t, err)
	require.NoError(t, client.WaitForServerReady(ctx))
	require.NoError(t, client.OpenFile(ctx, filePath))
	require.True(t, client.IsFileOpen(filePath))

	// The server is gone; the close notifications have nothing to reach.
	client.Kill()

	client.CloseAllFiles(ctx)
	require.False(t, client.IsFileOpen(filePath),
		"a close that could not be delivered must still drop what it was tracking")
}
