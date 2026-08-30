package lsp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/testenv"
	"github.com/stretchr/testify/require"
)

func TestClient_WorkspaceSymbolsAndHover_RealServer(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(file, []byte("package main\n"), 0o644))
	client, err := New("fake-symbols", config.LSPConfig{Command: exe, FileTypes: []string{"go"}, Env: map[string]string{fakeLSPServerEnv: "1", "SENNIT_LSP_FAKE_SCENARIO": "symbols", "SENNIT_LSP_FAKE_ROOT": dir}}, config.NewShellVariableResolver(testenv.New(map[string]string{})), dir, false)
	require.NoError(t, err)
	t.Cleanup(client.Kill)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err = client.Initialize(ctx, dir)
	require.NoError(t, err)
	require.NoError(t, client.WaitForServerReady(ctx))
	require.True(t, client.SupportsWorkspaceSymbols())
	require.True(t, client.SupportsHover())
	symbols, err := client.WorkspaceSymbolResults(ctx, "a")
	require.NoError(t, err)
	require.Len(t, symbols, 2)
	hover, err := client.Hover(ctx, file, 1, 0)
	require.NoError(t, err)
	require.Equal(t, "`Alpha() string`", hover.Contents.Value)
}

// TestClient_Hover_ConvertsToZeroBasedPosition pins requests.Hover
// converting the model's 1-based line/character into the LSP wire's
// 0-based Position, matching every other position-based request in
// requests.go (FindReferences, Rename, Definition, ...). Hover used to be
// the one request that forwarded the caller's coordinates unconverted, so
// a hover for line 3, character 5 landed on line 2, character 4 as far as
// the server was concerned.
func TestClient_Hover_ConvertsToZeroBasedPosition(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(file, []byte("package main\n"), 0o644))
	logPath := filepath.Join(dir, "lsp.log")
	client, err := New("fake-hover-position", config.LSPConfig{Command: exe, FileTypes: []string{"go"}, Env: map[string]string{
		fakeLSPServerEnv:           "1",
		"SENNIT_LSP_FAKE_SCENARIO": "symbols",
		"SENNIT_LSP_FAKE_LOG":      logPath,
	}}, config.NewShellVariableResolver(testenv.New(map[string]string{})), dir, false)
	require.NoError(t, err)
	t.Cleanup(client.Kill)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err = client.Initialize(ctx, dir)
	require.NoError(t, err)
	require.NoError(t, client.WaitForServerReady(ctx))

	// Ask for the 1-based position (line 3, character 5); the server must
	// see 0-based (line 2, character 4).
	_, err = client.Hover(ctx, file, 3, 5)
	require.NoError(t, err)

	contents, err := os.ReadFile(logPath)
	require.NoError(t, err)
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(string(contents)), "\n") {
		if strings.Contains(line, "textDocument/hover") {
			require.Contains(t, line, "line=2 character=4", "Hover must send the 0-based position on the wire")
			found = true
		}
	}
	require.True(t, found, "expected a logged textDocument/hover request")
}
