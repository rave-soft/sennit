package lsp

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	powernapconfig "github.com/charmbracelet/x/powernap/pkg/config"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/stretchr/testify/require"
)

func TestUnavailableBackoff(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 3, 26, 0, 0, 0, 0, time.UTC)
	now := base

	manager := &Manager{
		unavailable: csync.NewMap[string, time.Time](),
		now:         func() time.Time { return now },
	}

	require.False(t, manager.recentlyUnavailable("gopls"))

	manager.markUnavailable("gopls")
	require.True(t, manager.recentlyUnavailable("gopls"))

	now = now.Add(unavailableRetryDelay + time.Second)
	require.False(t, manager.recentlyUnavailable("gopls"))
	_, exists := manager.unavailable.Get("gopls")
	require.False(t, exists)

	manager.markUnavailable("gopls")
	manager.clearUnavailable("gopls")
	require.False(t, manager.recentlyUnavailable("gopls"))
}

func TestCanAutoStartFiltersBeforeLookingUpCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		server  *powernapconfig.ServerConfig
		want    bool
		lookups int
	}{
		{
			name: "unhandled file type",
			server: &powernapconfig.ServerConfig{
				Command:   "typescript-language-server",
				FileTypes: []string{"typescript"},
			},
		},
		{
			name: "generic command",
			server: &powernapconfig.ServerConfig{
				Command:   "node",
				FileTypes: []string{"go"},
			},
		},
		{
			name: "handled file type",
			server: &powernapconfig.ServerConfig{
				Command:   "gopls",
				FileTypes: []string{"go"},
			},
			want:    true,
			lookups: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			lookups := 0
			manager := &Manager{
				unavailable: csync.NewMap[string, time.Time](),
				now:         time.Now,
				lookPath: func(string) (string, error) {
					lookups++
					return "/usr/local/bin/gopls", nil
				},
			}

			got := manager.canAutoStart("test", "main.go", t.TempDir(), tt.server)

			require.Equal(t, tt.want, got)
			require.Equal(t, tt.lookups, lookups)
		})
	}
}

func TestCanAutoStartCachesMissingCommand(t *testing.T) {
	t.Parallel()

	lookups := 0
	manager := &Manager{
		unavailable: csync.NewMap[string, time.Time](),
		now:         time.Now,
		lookPath: func(string) (string, error) {
			lookups++
			return "", errors.New("not found")
		},
	}
	server := &powernapconfig.ServerConfig{
		Command:   "gopls",
		FileTypes: []string{"go"},
	}

	require.False(t, manager.canAutoStart("gopls", "main.go", t.TempDir(), server))
	require.False(t, manager.canAutoStart("gopls", "main.go", t.TempDir(), server))
	require.Equal(t, 1, lookups)
}

// TestStartServer_ConcurrentStartsCreateOneClient pins that concurrent
// Start calls for the same server name never spawn two LSP processes.
// Before the fix, startServer's check-then-create-then-store sequence was
// not atomic per name: two goroutines could both pass every check and
// both call New(), leaking whichever process lost the race. The "server"
// here is the test binary itself, re-exec'd as a minimal fake LSP server
// (see fakeserver_test.go), so this exercises the real Client.Initialize/
// WaitForServerReady path rather than a stub.
func TestStartServer_ConcurrentStartsCreateOneClient(t *testing.T) {
	t.Parallel()

	exe, err := os.Executable()
	require.NoError(t, err)

	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(path, []byte("package main\n"), 0o644))

	cfg := config.NewTestStore(t, &config.Config{
		Options: &config.Options{},
		LSP: config.LSPs{
			"fake": {
				Command:   exe,
				FileTypes: []string{"go"},
				Env:       map[string]string{fakeLSPServerEnv: "1"},
				Timeout:   5,
			},
		},
	}, config.WithWorkingDir(dir))

	mgr := NewManager(cfg)
	server, ok := mgr.manager.GetServer("fake")
	require.True(t, ok)

	const n = 20
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mgr.startServer("fake", path, server)
		}()
	}
	wg.Wait()

	require.Equal(t, 1, mgr.clients.Len(), "concurrent starts must produce exactly one client")

	client, ok := mgr.clients.Get("fake")
	require.True(t, ok)
	t.Cleanup(client.Kill)
	require.Equal(t, StateReady, client.GetServerState())
}
