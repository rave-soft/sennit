package lsp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
