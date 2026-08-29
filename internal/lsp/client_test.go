package lsp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/rave-soft/sennit/internal/config"
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

	oldGen := client.runtime.currentGeneration()
	client.diagnostics.publish(oldGen, []byte(`{"uri":"file:///stale.go","diagnostics":[{"message":"stale"}]}`))
	client.diagnostics.waitForDrain()
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
	gen := client.runtime.currentGeneration()

	client.Shutdown()

	require.ErrorIs(t, client.Restart(), errClientShutdown)
	require.Same(t, gen, client.runtime.currentGeneration())
}

func TestClient_RestartThenShutdownIsTerminal(t *testing.T) {
	client := newFakeRuntimeClient(t, "test-restart-shutdown")
	oldGen := client.runtime.currentGeneration()

	require.NoError(t, client.Restart())
	newGen := client.runtime.currentGeneration()
	require.NotSame(t, oldGen, newGen)

	client.Shutdown()
	require.ErrorIs(t, client.Restart(), errClientShutdown)
	require.Same(t, newGen, client.runtime.currentGeneration())
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
	oldGen := client.runtime.currentGeneration()

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
			client.diagnostics.publish(oldGen, []byte(`{"uri":"file:///stale.go","diagnostics":[{"message":"stale"}]}`))
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

	require.NotSame(t, oldGen, client.runtime.currentGeneration())
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
	c.runtime.cwd = dir
	c.fileTypes = []string{"go"}
	// HandlesFile delegates to files.handlesFile, which keeps its own copy
	// of cwd/fileTypes (set once from the same config at construction
	// time) rather than reading c.runtime's — update both here.
	c.files.cwd = dir
	c.files.fileTypes = []string{"go"}

	uri := string(protocol.URIFromPath(filePath))
	require.False(t, c.HandlesFile(uri), "HandlesFile must reject a raw URI")
	require.True(t, c.HandlesFile(filePath), "HandlesFile must accept the converted path")

	path, err := protocol.DocumentURI(uri).Path()
	require.NoError(t, err)
	require.Equal(t, filePath, path)
}

func newTestClient() *Client {
	c := &Client{
		name:      "test",
		fileTypes: []string{"go"},
	}
	c.runtime = newRuntime("test", config.LSPConfig{FileTypes: []string{"go"}}, nil, ".", false)
	gen := &clientGeneration{}
	c.runtime.gen = gen
	c.diagnostics = newDiagnosticsStore("test", gen)
	c.files = newFileSync(c.runtime.currentGeneration, ".", "test", c.fileTypes, nil, false)
	c.requests = newRequests(c.runtime.currentGeneration, nil)
	c.shutdownDone = make(chan struct{})
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
			handleDiagnostics(c, []byte(`{"uri":"file:///test.go","diagnostics":[]}`))
		}
	}()
	go func() {
		defer wg.Done()
		for range iterations {
			c.diagnostics.reset()
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
	oldGen := c.runtime.currentGeneration()
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
		c.diagnostics.publishDiag(oldGen, protocol.PublishDiagnosticsParams{
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
		c.runtime.mu.Lock()
		c.runtime.publishSwap(newGen, c.diagnostics, oldGen, StateReady)
		c.runtime.mu.Unlock()
		close(published)
	}()
	<-published
	close(proceed)
	<-finished

	require.Empty(t, c.GetDiagnostics())
	require.Equal(t, []int{1}, []int{<-counts})
	require.Same(t, newGen, c.runtime.currentGeneration())
}

// TestClient_DiagnosticsPublicationActivatesNewGenerationForImmediatePublish
// pins the lost-notification half of the atomic publication contract: a
// diagnostics notification for the new generation that arrives
// immediately after the swap (the server republishes as soon as the new
// process is up) must be accepted, never dropped as "active old". This is
// the exact window that used to lose the first new-server diagnostics:
// the runtime generation was swapped before diagnostics.active was, so
// the enqueue check rejected the new generation's traffic.
func TestClient_DiagnosticsPublicationActivatesNewGenerationForImmediatePublish(t *testing.T) {
	c := newTestClient()
	oldGen := c.runtime.currentGeneration()
	newGen := &clientGeneration{}
	counts := make(chan int, 2)
	c.SetDiagnosticsCallback(func(_ string, count int) {
		counts <- count
	})

	// Publish the swap, then immediately enqueue a notification that
	// claims the NEW generation — exactly what the real publishDiagnostics
	// handler does (it captures the generation it was registered on).
	c.diagnostics.publishGeneration(oldGen, newGen)
	c.diagnostics.publishDiag(newGen, protocol.PublishDiagnosticsParams{
		URI:         protocol.DocumentURI("file:///new.go"),
		Diagnostics: []protocol.Diagnostic{{Message: "new"}},
	}, nil)

	c.diagnostics.waitForDrain()
	require.NotEmpty(t, c.GetDiagnostics())
	require.Equal(t, 1, <-counts)
}

func TestClient_DiagnosticsPublicationWinsBeforeEventStartCommit(t *testing.T) {
	c := newTestClient()
	oldGen := c.runtime.currentGeneration()
	newGen := &clientGeneration{}
	popped := make(chan struct{})
	proceed := make(chan struct{})
	counts := make(chan int, 1)
	var once sync.Once
	c.diagnostics.hook = func() {
		once.Do(func() {
			close(popped)
			<-proceed
		})
	}
	c.SetDiagnosticsCallback(func(_ string, count int) {
		counts <- count
	})

	c.diagnostics.publish(oldGen, []byte(`{"uri":"file:///test.go","diagnostics":[{"message":"old"}]}`))
	<-popped
	c.diagnostics.publishGeneration(oldGen, newGen)
	close(proceed)

	c.diagnostics.waitForDrain()
	require.Empty(t, c.GetDiagnostics())
	select {
	case count := <-counts:
		t.Fatalf("callback started after publication with count %d", count)
	default:
	}
}

func TestClient_DiagnosticsPublicationOrdersResetAfterRunningCallback(t *testing.T) {
	c := newTestClient()
	oldGen := c.runtime.currentGeneration()
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

	c.diagnostics.publish(oldGen, []byte(`{"uri":"file:///test.go","diagnostics":[{"message":"old"}]}`))
	<-started
	c.runtime.mu.Lock()
	c.runtime.publishSwap(newGen, c.diagnostics, oldGen, StateReady)
	c.runtime.mu.Unlock()

	require.Same(t, newGen, c.runtime.currentGeneration())
	require.Empty(t, c.GetDiagnostics())
	close(release)
	require.Equal(t, []int{1, 0}, []int{<-counts, <-counts})
}

func TestClient_DiagnosticsQueuedBeforePublicationAreDropped(t *testing.T) {
	c := newTestClient()
	oldGen := c.runtime.currentGeneration()
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

	c.diagnostics.publish(oldGen, []byte(`{"uri":"file:///first.go","diagnostics":[{"message":"first"}]}`))
	<-started
	c.diagnostics.publish(oldGen, []byte(`{"uri":"file:///queued.go","diagnostics":[{"message":"queued"}]}`))
	c.diagnostics.publishGeneration(oldGen, newGen)
	close(release)

	require.Equal(t, []int{1, 0}, []int{<-counts, <-counts})
	c.diagnostics.waitForDrain()
	select {
	case count := <-counts:
		t.Fatalf("queued stale callback delivered count %d", count)
	default:
	}
}

func TestClient_ShutdownPurgesQueuedDiagnosticsAndStopsDispatcher(t *testing.T) {
	client := newFakeRuntimeClient(t, "test-shutdown-queue")
	gen := client.runtime.currentGeneration()
	started := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	var callbackReturns []int
	client.SetDiagnosticsCallback(func(_ string, count int) {
		if count != 0 {
			select {
			case <-started:
			default:
				close(started)
				<-release
			}
		}
		mu.Lock()
		callbackReturns = append(callbackReturns, count)
		mu.Unlock()
	})

	client.diagnostics.publish(gen, []byte(`{"uri":"file:///running.go","diagnostics":[{"message":"running"}]}`))
	<-started

	// The in-flight callback is blocked on release. External Shutdown must
	// not return before that callback has finished (strict quiescence).
	shutDown := make(chan struct{})
	go func() {
		client.Shutdown()
		close(shutDown)
	}()
	select {
	case <-shutDown:
		t.Fatal("Shutdown returned while a diagnostics callback was still running")
	case <-time.After(500 * time.Millisecond):
	}
	close(release)
	select {
	case <-shutDown:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown never completed after the in-flight callback returned")
	}
	select {
	case <-client.diagnostics.done:
	case <-time.After(5 * time.Second):
		t.Fatal("diagnostics dispatcher never terminated after Shutdown")
	}
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	require.Empty(t, client.GetDiagnostics())
	// The in-flight callback ran (count 1); the terminal zero-count
	// callback for the data present at shutdown ran last (count 0). The
	// queued stale event was purged and never ran.
	require.Equal(t, []int{1, 0}, callbackReturns,
		"in-flight callback plus terminal zero-count only; the queued one must be purged")
}

// TestClient_DiagnosticsPublishParsedBeforeShutdownIsRejected verifies the
// enqueue linearization point. JSON parsing can finish before Shutdown starts,
// but if Shutdown obtains d.mu before the parsed event is enqueued, that
// notification must be rejected rather than revive the cleared store.
func TestClient_DiagnosticsPublishParsedBeforeShutdownIsRejected(t *testing.T) {
	client := newFakeRuntimeClient(t, "test-publish-shutdown-linearization")
	gen := client.runtime.currentGeneration()
	parsed := make(chan struct{})
	release := make(chan struct{})
	client.diagnostics.beforeEnqueue = func() {
		close(parsed)
		<-release
	}

	published := make(chan struct{})
	go func() {
		client.diagnostics.publish(gen, []byte(`{"uri":"file:///late.go","diagnostics":[{"message":"late"}]}`))
		close(published)
	}()
	<-parsed

	shutdownDone := make(chan struct{})
	go func() {
		client.Shutdown()
		close(shutdownDone)
	}()
	// Shutdown must acquire d.mu and establish stop before the blocked
	// producer is allowed to attempt enqueue.
	for {
		client.diagnostics.mu.Lock()
		stopped := client.diagnostics.stop
		client.diagnostics.mu.Unlock()
		if stopped {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	select {
	case <-published:
	case <-time.After(5 * time.Second):
		t.Fatal("parsed diagnostics publish did not return")
	}
	select {
	case <-shutdownDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return")
	}
	require.Empty(t, client.GetDiagnostics())
}

func TestClient_DiagnosticsCallbackCanRestart(t *testing.T) {
	client := newFakeRuntimeClient(t, "test-callback-restart")
	oldGen := client.runtime.currentGeneration()
	finished := make(chan error, 1)
	client.SetDiagnosticsCallback(func(_ string, count int) {
		if count != 0 {
			finished <- client.Restart()
		}
	})

	go client.diagnostics.publish(oldGen, []byte(`{"uri":"file:///test.go","diagnostics":[{"message":"restart"}]}`))

	select {
	case err := <-finished:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("diagnostics callback deadlocked while restarting")
	}
	require.NotSame(t, oldGen, client.runtime.currentGeneration())
	require.Empty(t, client.GetDiagnostics())
	client.Shutdown()
}

// TestClient_ShutdownClosesRestartGateBeforeWaitingForCallback proves the
// lifecycle gate is terminal before Shutdown waits for an already-running
// callback. The callback must not be able to restart the client in that gap.
func TestClient_ShutdownClosesRestartGateBeforeWaitingForCallback(t *testing.T) {
	client := newFakeRuntimeClient(t, "test-shutdown-restart-gate")
	gen := client.runtime.currentGeneration()
	started := make(chan struct{})
	release := make(chan struct{})
	restartResult := make(chan error, 1)

	client.SetDiagnosticsCallback(func(_ string, count int) {
		if count == 0 {
			return
		}
		close(started)
		<-release
		restartResult <- client.Restart()
	})
	client.diagnostics.publish(gen, []byte(`{"uri":"file:///test.go","diagnostics":[{"message":"shutdown"}]}`))
	<-started

	shutDown := make(chan struct{})
	go func() {
		client.Shutdown()
		close(shutDown)
	}()
	for {
		client.runtime.mu.Lock()
		terminal := client.runtime.shutdown
		client.runtime.mu.Unlock()
		if terminal {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	require.ErrorIs(t, <-restartResult, errClientShutdown)
	select {
	case <-shutDown:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not finish after callback returned")
	}
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
			handleDiagnostics(c, []byte(`{"uri":"file:///test.go","diagnostics":[]}`))
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
			c.diagnostics.waitForDiagnostics(t.Context(), time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond)
		}
	}()
	go func() {
		defer wg.Done()
		for range iterations {
			c.diagnostics.reset()
		}
	}()
	wg.Wait()
}

func TestWaitForDiagnostics_NoChange(t *testing.T) {
	t.Parallel()

	c := newTestClient()
	start := time.Now()
	c.diagnostics.waitForDiagnostics(t.Context(), time.Second, 50*time.Millisecond, 30*time.Millisecond, 5*time.Millisecond)
	elapsed := time.Since(start)

	require.Less(t, elapsed, 200*time.Millisecond, "should return early when no diagnostics change")
}

func TestWaitForDiagnostics_ImmediateChange(t *testing.T) {
	t.Parallel()

	c := newTestClient()

	go func() {
		time.Sleep(20 * time.Millisecond)
		c.diagnostics.store.Set(protocol.DocumentURI("file:///test.go"), nil)
	}()

	start := time.Now()
	c.diagnostics.waitForDiagnostics(t.Context(), time.Second, 100*time.Millisecond, 30*time.Millisecond, 5*time.Millisecond)
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
			c.diagnostics.store.Set(protocol.DocumentURI("file:///test.go"), []protocol.Diagnostic{
				{Message: fmt.Sprintf("diag-%d", i)},
			})
		}
	}()

	start := time.Now()
	c.diagnostics.waitForDiagnostics(t.Context(), time.Second, 100*time.Millisecond, 30*time.Millisecond, 5*time.Millisecond)
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
	c.diagnostics.waitForDiagnostics(ctx, time.Second, 100*time.Millisecond, 30*time.Millisecond, 5*time.Millisecond)
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

// TestClient_ServerStateConcurrentReadWrite pins blocker 1: the server
// state is written by the runtime lifecycle (Restart/WaitForServerReady
// state transitions) and the manager while the UI polls GetServerState
// from other goroutines. A plain field data-races here; the state must be
// atomic-typed. Run with -race.
func TestClient_ServerStateConcurrentReadWrite(t *testing.T) {
	client := newFakeRuntimeClient(t, "test-state-race")

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for range 50 {
			client.SetServerState(StateStarting)
			client.SetServerState(StateReady)
		}
	}()
	go func() {
		defer wg.Done()
		// The restart rewrites state as a lifecycle transition (Stopped,
		// Starting, Ready) on the goroutine that owns the lifecycle gate.
		require.NoError(t, client.Restart())
		require.Equal(t, StateReady, client.GetServerState())
	}()
	go func() {
		defer wg.Done()
		for range 200 {
			s := client.GetServerState()
			if s < StateUnstarted || s > StateDisabled {
				t.Errorf("GetServerState returned an invalid state %d", s)
				return
			}
		}
	}()
	wg.Wait()
	client.Shutdown()
}

// TestClient_RestartDoesNotPublishUninitializedGeneration pins blocker 2:
// the new generation is only published as a single swap after
// Initialize+WaitForServerReady have succeeded on it. Concretely:
//   - while the restart runs, the current generation is never an
//     in-flight candidate (the sampler below only ever sees the old or
//     the new one);
//   - the generation the restart publishes is alive, usable, and
//     different from the old one;
//   - the old generation is retired (context canceled) only after the
//     successful swap, so a failed restart never leaves a killed
//     candidate current.
func TestClient_RestartDoesNotPublishUninitializedGeneration(t *testing.T) {
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
	client, err := New("test-preinit-visibility", cfg, resolver, dir, false)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	_, err = client.Initialize(ctx, dir)
	require.NoError(t, err)
	require.NoError(t, client.WaitForServerReady(ctx))
	require.NoError(t, client.OpenFile(ctx, filePath))
	oldGen := client.runtime.currentGeneration()

	restarted := make(chan struct{})
	restartErr := make(chan error, 1)
	go func() {
		restartErr <- client.Restart()
		close(restarted)
	}()

	// Sample the published generation continuously while the restart
	// runs. It may only ever be the old generation or, after the atomic
	// swap, the new one — never a third, in-flight candidate.
	var mu sync.Mutex
	observed := map[*clientGeneration]bool{oldGen: true}
	stop := make(chan struct{})
	samplerDone := make(chan struct{})
	go func() {
		defer close(samplerDone)
		ticker := time.NewTicker(1 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				g := client.runtime.currentGeneration()
				mu.Lock()
				observed[g] = true
				mu.Unlock()
			}
		}
	}()

	// Steady requests while the restart runs; each resolves the current
	// generation, which is either the old one (success before the close,
	// fast failure after) or, after the swap, the ready new one.
	for range 100 {
		_, err := client.Hover(ctx, filePath, 0, 0)
		_ = err
	}

	<-restarted
	require.NoError(t, <-restartErr)
	newGen := client.runtime.currentGeneration()

	close(stop)
	<-samplerDone

	mu.Lock()
	defer mu.Unlock()
	// Whatever the sampler caught, it must be a subset of {old, new}:
	// never a third, in-flight candidate.
	for g := range observed {
		require.True(t, g == oldGen || g == newGen,
			"an unpublished candidate was made current during the restart")
	}

	require.NotSame(t, oldGen, newGen)
	require.Error(t, oldGen.ctx.Err(), "retired generation must be canceled after the swap")
	require.NoError(t, newGen.ctx.Err(), "published generation must be alive")

	// The published generation is fully initialized and ready: a request
	// against it succeeds.
	_, err = client.Hover(ctx, filePath, 0, 0)
	require.NoError(t, err)
}

// TestClient_FailedRestartKeepsCurrentGenerationCoherent pins blocker 3:
// when the replacement server fails the handshake, the failed candidate
// must never be published as current. The (closed, dead) old generation
// stays identifiable and current, StateError is published, and a later
// Restart retries from there and succeeds.
func TestClient_FailedRestartKeepsCurrentGenerationCoherent(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	dir := t.TempDir()

	client, err := New("test-failed-restart", config.LSPConfig{
		Command: exe,
		Env:     map[string]string{fakeLSPServerEnv: "1"},
	}, config.NewShellVariableResolver(testenv.New(map[string]string{})), dir, false)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	_, err = client.Initialize(ctx, dir)
	require.NoError(t, err)
	require.NoError(t, client.WaitForServerReady(ctx))
	cancel()
	readyGen := client.runtime.currentGeneration()

	// The candidate answers the initialize request with a payload that is
	// not an object, so Initialize fails on the new process. The runtime
	// re-resolves its config for every generation it creates, so swap the
	// runtime copy (the façade's is not consulted again).
	badCfg := client.runtime.config
	badCfg.Env = map[string]string{
		fakeLSPServerEnv:           "1",
		"SENNIT_LSP_FAKE_SCENARIO": "bad-init",
	}
	client.runtime.config = badCfg

	err = client.Restart()
	require.Error(t, err, "restart against a server that fails initialize must fail")
	require.Same(t, readyGen, client.runtime.currentGeneration(),
		"the failed candidate must not be published as current")
	require.Equal(t, StateError, client.GetServerState())
	// The old generation is dead: its process was closed during the
	// restart, and its context is canceled at the moment it stops being
	// the live one — here, at the point the dead generation is retired.
	// No live context and no live process leak behind a dead current
	// generation, and a later retry does not re-run the graceful shutdown
	// against the closed process.
	require.Error(t, readyGen.ctx.Err())
	require.True(t, readyGen.isUsable() == false)

	// A later restart from the failed state must retry and succeed,
	// publishing a fresh, live generation.
	goodCfg := client.runtime.config
	goodCfg.Env = map[string]string{fakeLSPServerEnv: "1"}
	client.runtime.config = goodCfg

	require.NoError(t, client.Restart())
	newGen := client.runtime.currentGeneration()
	require.NotSame(t, readyGen, newGen)
	require.NoError(t, newGen.ctx.Err())
	require.Equal(t, StateReady, client.GetServerState())
}

// TestClient_ShutdownCallbackQuiescence pins the strict shutdown contract:
// once a callback is running, an external Shutdown must not return before
// that callback has finished; after it returns, no further callback may
// ever fire. The store is quiescent and its queue is purged, and the
// dispatcher goroutine has terminated by the time Shutdown returns.
func TestClient_ShutdownCallbackQuiescence(t *testing.T) {
	client := newFakeRuntimeClient(t, "test-shutdown-quiescence")
	gen := client.runtime.currentGeneration()

	var mu sync.Mutex
	var callbackReturns int
	var countsAfterLastCallback []int
	started := make(chan struct{})
	release := make(chan struct{})
	shutDown := make(chan struct{})

	client.SetDiagnosticsCallback(func(_ string, count int) {
		if count != 0 {
			close(started)
			<-release
		}
		mu.Lock()
		callbackReturns++
		// The terminal zero-count callback (for the data present at
		// shutdown time) is allowed as the final event. Any NON-zero
		// callback after the first is a stale event and is forbidden.
		if callbackReturns > 1 && count != 0 {
			countsAfterLastCallback = append(countsAfterLastCallback, count)
		}
		mu.Unlock()
	})

	go func() {
		client.diagnostics.publish(gen, []byte(`{"uri":"file:///running.go","diagnostics":[{"message":"running"}]}`))
	}()
	<-started

	go func() {
		client.Shutdown()
		close(shutDown)
	}()

	// Strict quiescence: Shutdown must NOT return while the callback is
	// still blocked.
	select {
	case <-shutDown:
		t.Fatal("Shutdown returned while a diagnostics callback was still running")
	case <-time.After(200 * time.Millisecond):
	}

	// Let the running callback finish; it is the last one allowed.
	close(release)
	select {
	case <-shutDown:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown never returned after the in-flight callback finished")
	}
	// By contract the dispatcher is already terminated; give a
	// hypothetical stale callback a chance to (wrongly) fire.
	select {
	case <-client.diagnostics.done:
	case <-time.After(5 * time.Second):
		t.Fatal("diagnostics dispatcher never terminated after Shutdown")
	}
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	// The in-flight callback (count 1) and the terminal zero-count
	// callback (count 0) are the only two allowed: the zero-count is the
	// finalization for the data present at shutdown time, dispatched as
	// part of the quiescence step. Any further callback is stale.
	require.Equal(t, 2, callbackReturns, "exactly two callbacks may run: the in-flight one and the terminal zero-count")
	if len(countsAfterLastCallback) != 0 {
		t.Fatalf("stale diagnostics callback fired after the terminal zero-count: %v", countsAfterLastCallback)
	}
	require.Empty(t, client.GetDiagnostics())
}

// TestClient_ShutdownConcurrentRepeatedIsIdempotent pins that repeated
// concurrent Shutdown calls are safe and idempotent: exactly one performs
// the teardown, the rest wait for the same quiescence point, and every
// caller returns without deadlocking. Run with -race.
func TestClient_ShutdownConcurrentRepeatedIsIdempotent(t *testing.T) {
	client := newFakeRuntimeClient(t, "test-shutdown-concurrent")

	const n = 8
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			client.Shutdown()
		})
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent Shutdown calls deadlocked")
	}

	select {
	case <-client.diagnostics.done:
	case <-time.After(5 * time.Second):
		t.Fatal("diagnostics dispatcher never terminated after concurrent Shutdown")
	}
	require.ErrorIs(t, client.Restart(), errClientShutdown)
	require.Equal(t, StateStopped, client.GetServerState())
	require.Empty(t, client.GetDiagnostics())
}

// TestClient_ShutdownWhileDiagnosticsRunningThenRestartBlocked pins the
// strict external contract end to end: while a diagnostics callback is
// blocked, an external Shutdown does not return; once the callback
// finishes, Shutdown completes and the dispatcher terminates; afterwards
// the client is terminal (Restart fails) and no callback ever fires.
func TestClient_ShutdownWhileDiagnosticsRunningThenRestartBlocked(t *testing.T) {
	client := newFakeRuntimeClient(t, "test-shutdown-running-strict")
	gen := client.runtime.currentGeneration()

	started := make(chan struct{})
	release := make(chan struct{})
	calledAfter := make(chan struct{}, 1)
	var callbackCount atomic.Int32
	client.SetDiagnosticsCallback(func(_ string, count int) {
		n := callbackCount.Add(1)
		if n == 1 {
			close(started)
			<-release
			return
		}
		if n == 2 {
			// The terminal zero-count callback for the data present at
			// shutdown time: allowed, part of the quiescence step.
			if count != 0 {
				select {
				case calledAfter <- struct{}{}:
				default:
				}
			}
			return
		}
		// Any callback after the terminal one is stale: report it and
		// stop.
		select {
		case calledAfter <- struct{}{}:
		default:
		}
	})

	client.diagnostics.publish(gen, []byte(`{"uri":"file:///running.go","diagnostics":[{"message":"running"}]}`))
	<-started

	shutDown := make(chan struct{})
	go func() {
		client.Shutdown()
		close(shutDown)
	}()

	select {
	case <-shutDown:
		t.Fatal("Shutdown returned before the in-flight diagnostics callback finished")
	case <-time.After(500 * time.Millisecond):
	}

	close(release)
	select {
	case <-shutDown:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown never completed after the callback returned")
	}
	select {
	case <-client.diagnostics.done:
	case <-time.After(5 * time.Second):
		t.Fatal("diagnostics dispatcher never terminated")
	}
	select {
	case <-calledAfter:
		t.Fatal("a stale diagnostics callback fired after the terminal zero-count")
	default:
	}
	require.Equal(t, int32(2), callbackCount.Load())
	require.ErrorIs(t, client.Restart(), errClientShutdown)
	require.Empty(t, client.GetDiagnostics())
}

// TestClient_FailedRestartRetiresDeadGeneration pins the failed-restart
// cleanup contract: when a restart fails, the old generation (whose
// process was closed during the restart) must have its context canceled
// — no live context or process leaks behind a dead current generation —
// and a later retry must not re-run the graceful shutdown against the
// already-closed process: it re-closes only through the idempotent kill
// path and then succeeds with a fresh, live generation.
func TestClient_FailedRestartRetiresDeadGeneration(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	dir := t.TempDir()

	client, err := New("test-failed-restart-retire", config.LSPConfig{
		Command: exe,
		Env:     map[string]string{fakeLSPServerEnv: "1"},
	}, config.NewShellVariableResolver(testenv.New(map[string]string{})), dir, false)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	_, err = client.Initialize(ctx, dir)
	require.NoError(t, err)
	require.NoError(t, client.WaitForServerReady(ctx))
	cancel()
	readyGen := client.runtime.currentGeneration()
	require.NoError(t, readyGen.ctx.Err())

	badCfg := client.runtime.config
	badCfg.Env = map[string]string{
		fakeLSPServerEnv:           "1",
		"SENNIT_LSP_FAKE_SCENARIO": "bad-init",
	}
	client.runtime.config = badCfg

	err = client.Restart()
	require.Error(t, err)
	require.Same(t, readyGen, client.runtime.currentGeneration())
	// The dead old generation is retired: context canceled, dead flag set.
	require.Error(t, readyGen.ctx.Err(), "a dead current generation must not keep a live context")
	require.True(t, readyGen.dead.Load(), "a closed generation must be marked dead")

	// A later retry must succeed and must not re-run the graceful
	// shutdown against the closed process (the dead flag routes it to the
	// idempotent kill path).
	goodCfg := client.runtime.config
	goodCfg.Env = map[string]string{fakeLSPServerEnv: "1"}
	client.runtime.config = goodCfg

	start := time.Now()
	require.NoError(t, client.Restart())
	require.Less(t, time.Since(start), 8*time.Second,
		"retry against a dead generation must not pay a graceful-close timeout")

	newGen := client.runtime.currentGeneration()
	require.NotSame(t, readyGen, newGen)
	require.NoError(t, newGen.ctx.Err())
	require.False(t, newGen.dead.Load())
	require.Equal(t, StateReady, client.GetServerState())
	t.Cleanup(newGen.client.Kill)
}

func TestClient_RestartRootMarkerOpensOnCandidate(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	dir := t.TempDir()
	marker := filepath.Join(dir, "go.mod")
	require.NoError(t, os.WriteFile(marker, []byte("module test\n"), 0o644))
	logPath := filepath.Join(dir, "lsp.log")
	client, err := New("test-root-marker-generation", config.LSPConfig{
		Command:     exe,
		FileTypes:   []string{"mod"},
		RootMarkers: []string{"go.mod"},
		Env: map[string]string{
			fakeLSPServerEnv:      "1",
			"SENNIT_LSP_FAKE_LOG": logPath,
		},
	}, config.NewShellVariableResolver(testenv.New(map[string]string{})), dir, false)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, func() error { _, err := client.Initialize(ctx, dir); return err }())
	require.NoError(t, client.WaitForServerReady(ctx))
	oldGen := client.runtime.currentGeneration()
	before, err := os.ReadFile(logPath)
	require.NoError(t, err)
	require.NoError(t, client.Restart())
	newGen := client.runtime.currentGeneration()
	require.NotSame(t, oldGen, newGen)
	contents, err := os.ReadFile(logPath)
	require.NoError(t, err)
	restartLines := strings.Split(strings.TrimSpace(string(contents[len(before):])), "\n")
	var initializePID, didOpenPID string
	for _, line := range restartLines {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		switch fields[1] {
		case "initialize":
			initializePID = fields[0]
		case "textDocument/didOpen":
			didOpenPID = fields[0]
		}
	}
	require.NotEmpty(t, initializePID)
	require.Equal(t, initializePID, didOpenPID,
		"restart root marker must be opened on the unpublished candidate process")
	client.Shutdown()
}

func TestClient_FailedRestartRollsBackCandidateFilesAndRetries(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	dir := t.TempDir()
	marker := filepath.Join(dir, "go.mod")
	userFile := filepath.Join(dir, "main.go")
	logPath := filepath.Join(dir, "lsp.log")
	require.NoError(t, os.WriteFile(marker, []byte("module test\n"), 0o644))
	require.NoError(t, os.WriteFile(userFile, []byte("package main\n"), 0o644))
	client, err := New("test-restart-file-rollback", config.LSPConfig{
		Command:     exe,
		FileTypes:   []string{"go", "mod"},
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
	require.NoError(t, client.OpenFile(ctx, userFile))

	before, err := os.ReadFile(logPath)
	require.NoError(t, err)
	require.NoError(t, os.Remove(userFile))
	require.Error(t, client.Restart(), "candidate must fail when a tracked file cannot reopen")
	require.True(t, client.IsFileOpen(userFile), "failed candidate must not alter the user-file snapshot")

	require.NoError(t, os.WriteFile(userFile, []byte("package main\n"), 0o644))
	require.NoError(t, client.Restart(), "retry must reopen the preserved user-file snapshot")
	require.True(t, client.IsFileOpen(userFile))

	// What this asserts is the retry's two opens — the root marker and the
	// automatically reopened user file — landing on one and the same
	// process. That is the property the rollback exists for: the preserved
	// snapshot is replayed onto the generation that actually took over.
	//
	// Deliberately not "three opens across all processes". The failed
	// candidate is killed, and nothing sequences its death after it has
	// read the didOpen the client already wrote into its stdin: the client
	// sends a notification and moves on, so the message can still be
	// sitting in the pipe when the process dies, and that line then never
	// appears at all. Counting it made this test wait out its whole
	// timeout on a loaded CI runner for a line that was not coming
	// (run 33239477400, ubuntu). A longer timeout does not fix a write
	// that was never made.
	//
	// Still polled rather than read once: Restart returns when the
	// *client* is done, not when the surviving server has flushed its log.
	require.Eventually(t, func() bool {
		contents, err := os.ReadFile(logPath)
		if err != nil {
			return false
		}
		opensByPID := map[string]int{}
		for _, line := range strings.Split(strings.TrimSpace(string(contents[len(before):])), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[1] == "textDocument/didOpen" {
				opensByPID[fields[0]]++
			}
		}
		for _, n := range opensByPID {
			if n == 2 {
				return true
			}
		}
		return false
	}, 10*time.Second, 10*time.Millisecond,
		"the surviving retry must show two didOpens: the root marker and the reopened user file")
	client.Shutdown()
}

// TestClient_FailedCandidateCleanupCannotBlockKillOrShutdown wedges a
// candidate generation on a send the server has stopped reading, then
// forces its cleanup: the kill and the shutdown that follow must not wait
// on that send.
//
// The wedge is: the test fills the server's stdin pipe with 8MB the fake
// server never reads, and relies on the write blocking on back-pressure.
//
// This used to skip Windows, on the strength of that write returning
// "large send completed before process destruction: jsonrpc2: connection
// is closed" there — read at the time as a platform property. The same
// message then appeared twice running on macOS, and the cause turned out
// to be the fake server killing itself on `select {}`. With that fixed
// the skip was removed, and the Windows leg passes: the wedge holds on
// all three platforms, and the write really does block on back-pressure
// everywhere. The skip had been standing in for a bug in this file.
func TestClient_FailedCandidateCleanupCannotBlockKillOrShutdown(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	dir := t.TempDir()
	marker := filepath.Join(dir, "go.mod")
	userFile := filepath.Join(dir, "main.go")
	logPath := filepath.Join(dir, "lsp.log")
	require.NoError(t, os.WriteFile(marker, []byte("module test\n"), 0o644))
	require.NoError(t, os.WriteFile(userFile, []byte("package main\n"), 0o644))
	client, err := New("test-candidate-cleanup", config.LSPConfig{
		Command:     exe,
		FileTypes:   []string{"go", "mod"},
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
	require.NoError(t, client.OpenFile(ctx, userFile))

	badCfg := client.runtime.config
	badCfg.Env = map[string]string{
		fakeLSPServerEnv:           "1",
		"SENNIT_LSP_FAKE_LOG":      logPath,
		"SENNIT_LSP_FAKE_SCENARIO": "stop-reading-after-workspace-change",
	}
	client.runtime.config = badCfg

	prepare := client.files.prepareSync()
	var candidate *clientGeneration
	blockedSend := make(chan struct{})
	largeSendDone := make(chan error, 1)
	lockWaiterDone := make(chan error, 1)
	restartDone := make(chan error, 1)
	go func() {
		restartErr := client.runtime.restart(client.diagnostics, func(ctx context.Context, gen *clientGeneration) (func(), error) {
			candidate = gen
			commit, prepareErr := prepare(ctx, gen)
			if prepareErr != nil {
				return nil, prepareErr
			}
			if notifyErr := gen.client.NotifyDidChangeWatchedFiles(ctx, []protocol.FileEvent{{
				URI:  protocol.URIFromPath(dir),
				Type: protocol.Changed,
			}}); notifyErr != nil {
				return nil, notifyErr
			}
			// Generous: this waits on a spawned process writing a log
			// file, which a loaded Windows runner can take seconds to
			// get to. Nothing is asserted by the wait being short - the
			// deadline exists only so a server that never confirms
			// fails with a reason instead of hanging.
			deadline := time.Now().Add(30 * time.Second)
			for {
				contents, readErr := os.ReadFile(logPath)
				if readErr == nil && strings.Contains(string(contents), " workspace/didChangeWatchedFiles") {
					break
				}
				if time.Now().After(deadline) {
					return nil, fmt.Errorf("fake server did not confirm stopped reader: %w", readErr)
				}
				time.Sleep(time.Millisecond)
			}

			largeURI := string(protocol.URIFromPath(filepath.Join(dir, "blocked.go")))
			go func() {
				largeSendDone <- gen.client.NotifyDidOpenTextDocument(
					context.Background(), largeURI, "go", 1, strings.Repeat("x", 8<<20),
				)
			}()
			select {
			case sendErr := <-largeSendDone:
				// Whether the candidate process is still alive is the
				// whole question here and the error alone does not say:
				// "connection is closed" reads the same whether the send
				// raced a transport we closed or whether the fake server
				// died and took its end of the pipe with it. The wedge
				// needs a live process that has stopped reading, so
				// record which one happened.
				return nil, fmt.Errorf("large send completed before process destruction (candidate still running: %t): %w",
					gen.client.IsRunning(), sendErr)
			case <-time.After(2 * time.Second):
			}
			lockWaiterStarted := make(chan struct{})
			go func() {
				close(lockWaiterStarted)
				lockWaiterDone <- gen.client.NotifyDidCloseTextDocument(context.Background(), largeURI)
			}()
			<-lockWaiterStarted
			select {
			case sendErr := <-lockWaiterDone:
				return nil, fmt.Errorf("send-lock waiter completed while transport was blocked: %w", sendErr)
			case <-time.After(time.Second):
			}
			close(blockedSend)
			return commit, fmt.Errorf("force candidate cleanup after confirmed blocked send")
		})
		restartDone <- restartErr
	}()

	select {
	case <-blockedSend:
	case <-time.After(60 * time.Second):
		// The restart error, when there is one already, says which of
		// the steps above gave up - without it this timeout reports only
		// that something upstream did not happen.
		select {
		case restartErr := <-restartDone:
			t.Fatalf("candidate transport never reached a confirmed blocked-send state: %v", restartErr)
		default:
			t.Fatal("candidate transport never reached a confirmed blocked-send state")
		}
	}
	select {
	case err = <-restartDone:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("candidate cleanup blocked before mandatory Kill")
	}
	require.NotNil(t, candidate)
	require.True(t, candidate.dead.Load())
	require.False(t, candidate.client.IsRunning())
	select {
	case <-largeSendDone:
	case <-time.After(5 * time.Second):
		t.Fatal("process destruction did not release blocked transport send")
	}
	select {
	case <-lockWaiterDone:
	case <-time.After(5 * time.Second):
		t.Fatal("process destruction did not release send-lock waiter")
	}
	require.True(t, client.IsFileOpen(userFile), "failed candidate must preserve tracked files")

	beforeRetry, err := os.ReadFile(logPath)
	require.NoError(t, err)
	goodCfg := client.runtime.config
	goodCfg.Env = map[string]string{
		fakeLSPServerEnv:      "1",
		"SENNIT_LSP_FAKE_LOG": logPath,
	}
	client.runtime.config = goodCfg
	require.NoError(t, client.Restart(), "retry must reopen the preserved files")
	require.True(t, client.IsFileOpen(userFile))
	require.Eventually(t, func() bool {
		contents, readErr := os.ReadFile(logPath)
		if readErr != nil || len(contents) < len(beforeRetry) {
			return false
		}
		return strings.Count(string(contents[len(beforeRetry):]), " textDocument/didOpen") >= 2
	}, 5*time.Second, time.Millisecond, "successful retry must send didOpen for the root marker and tracked file")
	shutdownDone := make(chan struct{})
	go func() {
		client.Shutdown()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown blocked after failed candidate cleanup")
	}
}

func TestClient_RestartPublishesReadyOnlyWithCandidate(t *testing.T) {
	client := newFakeRuntimeClient(t, "test-ready-publication")
	oldGen := client.runtime.currentGeneration()
	client.SetServerState(StateStopped)
	generationPublished := make(chan struct{})
	releaseState := make(chan struct{})
	client.runtime.afterGenerationPublish = func() {
		close(generationPublished)
		<-releaseState
	}

	restartDone := make(chan error, 1)
	go func() { restartDone <- client.Restart() }()
	<-generationPublished

	// afterGenerationPublish runs inside publishSwap while genMu is held, so
	// calling currentGeneration() (which also takes genMu) from here would
	// deadlock against the hook below. GetServerState no longer takes any
	// lock, so it is safe to call: StateReady is reported only after this
	// hook returns, so it must not be visible yet.
	require.NotEqual(t, StateReady, client.GetServerState())

	close(releaseState)
	require.NoError(t, <-restartDone)
	require.NotSame(t, oldGen, client.runtime.currentGeneration())
	require.Equal(t, StateReady, client.GetServerState())
	client.Shutdown()
}
