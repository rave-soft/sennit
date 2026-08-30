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
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/stretchr/testify/require"
)

// TestStart_ResolvesSymlinkedWorkingDir guards against comparing paths
// textually instead of canonically. Start rejects any file outside the
// working directory before ever looking at registered servers; if the
// working directory is reached through a symlink (or, on Windows, spelled
// differently than the caller's path), a plain fsext.HasPrefix comparison
// never matches even though the file genuinely is inside it, so the LSP
// silently never starts.
func TestStart_ResolvesSymlinkedWorkingDir(t *testing.T) {
	realDir := t.TempDir()
	filePath := filepath.Join(realDir, "main.go")
	require.NoError(t, os.WriteFile(filePath, []byte("package main\n"), 0o644))

	symDir := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(realDir, symDir))

	exe, err := os.Executable()
	require.NoError(t, err)

	autoLSPOff := false
	cfg := configtest.NewStore(t, &config.Config{
		Options: &config.Options{AutoLSP: &autoLSPOff},
		LSP: map[string]config.LSPConfig{
			"fake": {
				Command:   exe,
				FileTypes: []string{"go"},
				Env:       map[string]string{fakeLSPServerEnv: "1"},
			},
		},
	}, configtest.WithWorkingDir(symDir))

	manager := NewManager(cfg)
	started := make(chan string, 1)
	manager.SetCallback(func(name string, client *Client) {
		select {
		case started <- name:
		default:
		}
	})
	t.Cleanup(func() {
		for _, c := range manager.Clients().Seq2() {
			c.Kill()
		}
	})

	// The working directory is the symlink; the file path is resolved
	// through the real directory, exactly the mismatch a symlinked
	// project root produces in practice.
	manager.Start(t.Context(), filePath)

	select {
	case name := <-started:
		require.Equal(t, "fake", name)
	case <-time.After(5 * time.Second):
		t.Fatal("Start never attempted to start the user-configured LSP for a file inside a symlinked working directory")
	}
}

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

	cfg := configtest.NewStore(t, &config.Config{
		Options: &config.Options{},
		LSP: config.LSPs{
			"fake": {
				Command:   exe,
				FileTypes: []string{"go"},
				Env:       map[string]string{fakeLSPServerEnv: "1"},
				Timeout:   5,
			},
		},
	}, configtest.WithWorkingDir(dir))

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

// TestWorkspaceClients_ScopedToRoot pins the bug where WorkspaceClients
// returned every client the Manager had ever started, ignoring the root
// it was asked about. Two servers are configured, one for Go and one for
// Python files; root only contains a .go file, so only the Go server is
// relevant to it. A client for the Python server is already running
// (started for some other file elsewhere), so it sits in mgr.clients
// alongside the Go client. WorkspaceClients must return only the client
// whose server actually handles something under root.
func TestWorkspaceClients_ScopedToRoot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644))

	cfg := configtest.NewStore(t, &config.Config{
		Options: &config.Options{},
		LSP: config.LSPs{
			"go-lsp": {
				Command:   "go-lsp-binary",
				FileTypes: []string{"go"},
			},
			"py-lsp": {
				Command:   "py-lsp-binary",
				FileTypes: []string{"py"},
			},
		},
	}, configtest.WithWorkingDir(dir))

	mgr := NewManager(cfg)

	// Pre-populate both clients as already running, so startServer's
	// fast path reuses them instead of trying to spawn a real process.
	for _, name := range []string{"go-lsp", "py-lsp"} {
		client := newTestClient()
		client.name = name
		client.SetServerState(StateReady)
		mgr.clients.Set(name, client)
	}

	clients := mgr.WorkspaceClients(t.Context(), dir)

	require.Len(t, clients, 1, "only the server relevant to root should be returned")
	require.Equal(t, "go-lsp", clients[0].GetName())
}

// TestWorkspaceClients_EarlyExitCountsOnlyStartableServers pins that the
// walk's early exit compares found servers against the ones actually
// capable of starting (user-configured, or auto-start candidates whose
// command is on PATH), not against every server in powernap's registry
// (dozens, most not installed). Comparing against the full registry made
// the early exit dead code; miscounting it the other way (e.g. zero) would
// stop the walk before root is ever examined, breaking discovery entirely
// — which is exactly what this test would catch.
func TestWorkspaceClients_EarlyExitCountsOnlyStartableServers(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644))

	cfg := configtest.NewStore(t, &config.Config{
		Options: &config.Options{},
		LSP: config.LSPs{
			"go-lsp": {
				Command:   "go-lsp-binary",
				FileTypes: []string{"go"},
			},
		},
	}, configtest.WithWorkingDir(dir))

	mgr := NewManager(cfg)
	// Isolate from whatever happens to be installed on this machine: no
	// auto-start candidate from the default registry should count as
	// startable, only the user-configured "go-lsp".
	mgr.lookPath = func(string) (string, error) { return "", errors.New("not found") }

	client := newTestClient()
	client.name = "go-lsp"
	client.SetServerState(StateReady)
	mgr.clients.Set("go-lsp", client)

	clients := mgr.WorkspaceClients(t.Context(), dir)

	require.Len(t, clients, 1, "the user-configured server should still be discovered")
	require.Equal(t, "go-lsp", clients[0].GetName())
}

// TestWorkspaceClients_SkipsNonStartableServerFiletypeMatch pins the bug
// where a file matching a non-startable server's filetype could exhaust the
// walk's early exit before the startable server it was looking for was ever
// found. The registry here is trimmed to exactly two servers via
// powernapconfig.Manager's own AddServer (bypassing LoadDefaults, so the
// dozens of stock entries — several of which match any file because they
// carry no FileTypes filter — can't blur the count): "fake-ls", whose
// filetype matches "app.txt" (walked first, since it sorts before
// "main.go") but whose command is never on PATH, and "go-lsp", the
// user-configured server this test wants found. Before the fix, the walk
// marked fake-ls found (and called startServer for it) purely because its
// filetype matched — regardless of it being startable — which made
// len(found) reach the startable count (1, for go-lsp alone) right after
// app.txt, so the walk hit SkipDir at the root and never reached main.go.
func TestWorkspaceClients_SkipsNonStartableServerFiletypeMatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "app.txt"), []byte("noise\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644))

	cfg := configtest.NewStore(t, &config.Config{
		Options: &config.Options{},
		LSP: config.LSPs{
			"go-lsp": {
				Command:   "go-lsp-binary",
				FileTypes: []string{"go"},
			},
		},
	}, configtest.WithWorkingDir(dir))

	registry := powernapconfig.NewManager()
	registry.AddServer("go-lsp", &powernapconfig.ServerConfig{
		Command:   "go-lsp-binary",
		FileTypes: []string{"go"},
	})
	registry.AddServer("fake-ls", &powernapconfig.ServerConfig{
		Command:   "fake-ls-binary",
		FileTypes: []string{"txt"},
	})

	mgr := &Manager{
		clients:     csync.NewMap[string, *Client](),
		unavailable: csync.NewMap[string, time.Time](),
		startMu:     csync.NewMap[string, *sync.Mutex](),
		cfg:         cfg,
		manager:     registry,
		callback:    func(string, *Client) {},
		now:         time.Now,
		// Neither fake-ls-binary nor go-lsp-binary is ever on PATH; go-lsp
		// counts as startable solely because it is user-configured.
		lookPath: func(string) (string, error) { return "", errors.New("not found") },
	}

	client := newTestClient()
	client.name = "go-lsp"
	client.SetServerState(StateReady)
	mgr.clients.Set("go-lsp", client)

	clients := mgr.WorkspaceClients(t.Context(), dir)

	require.Len(t, clients, 1, "go-lsp must still be discovered even though a file matching a non-startable server's filetype is walked first")
	require.Equal(t, "go-lsp", clients[0].GetName())
}

// TestStartServer_StateRaceSafeUnderConcurrentUIReads pins the state
// race: startServer writes the server state (Starting during Initialize,
// Ready/Error afterwards) while a UI goroutine polls GetServerState. A
// plain field would data-race here; the state must be atomic. Run with
// -race.
func TestStartServer_StateRaceSafeUnderConcurrentUIReads(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)

	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(path, []byte("package main\n"), 0o644))

	autoLSPOff := false
	cfg := configtest.NewStore(t, &config.Config{
		Options: &config.Options{AutoLSP: &autoLSPOff},
		LSP: config.LSPs{
			"fake": {
				Command:   exe,
				FileTypes: []string{"go"},
				Env:       map[string]string{fakeLSPServerEnv: "1"},
				Timeout:   30,
			},
		},
	}, configtest.WithWorkingDir(dir))

	mgr := NewManager(cfg)
	server, ok := mgr.manager.GetServer("fake")
	require.True(t, ok)

	stopReading := make(chan struct{})
	doneReading := make(chan struct{})
	go func() {
		defer close(doneReading)
		for {
			select {
			case <-stopReading:
				return
			default:
			}
			if c, ok := mgr.clients.Get("fake"); ok {
				if s := c.GetServerState(); s < StateUnstarted || s > StateDisabled {
					t.Errorf("GetServerState returned an invalid state %d", s)
					return
				}
			}
		}
	}()

	mgr.startServer("fake", path, server)

	close(stopReading)
	<-doneReading

	client, ok := mgr.clients.Get("fake")
	require.True(t, ok)
	t.Cleanup(client.Kill)
	require.Equal(t, StateReady, client.GetServerState())
}
