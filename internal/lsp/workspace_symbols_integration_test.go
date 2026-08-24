package lsp

import (
	"context"
	"os"
	"path/filepath"
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
