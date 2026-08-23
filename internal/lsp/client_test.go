package lsp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

	require.NoError(t, client.Restart())
	require.True(t, client.IsFileOpen(filePath), "file should be reopened after restart")
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
	return c
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
