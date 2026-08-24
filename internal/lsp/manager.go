// Package lsp provides a manager for Language Server Protocol (LSP) clients.
package lsp

import (
	"cmp"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	powernapconfig "github.com/charmbracelet/x/powernap/pkg/config"
	powernap "github.com/charmbracelet/x/powernap/pkg/lsp"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/sourcegraph/jsonrpc2"
)

const unavailableRetryDelay = 30 * time.Second

// Manager handles lazy initialization of LSP clients based on file types.
type Manager struct {
	clients     *csync.Map[string, *Client]
	unavailable *csync.Map[string, time.Time]
	// startMu holds one mutex per server name, taken across the
	// check-then-create-then-store sequence in startServer so concurrent
	// starts for the same server never spawn two LSP processes.
	startMu  *csync.Map[string, *sync.Mutex]
	cfg      *config.ConfigStore
	manager  *powernapconfig.Manager
	callback func(name string, client *Client)
	now      func() time.Time
	lookPath func(string) (string, error)
}

// NewManager creates a new LSP manager service.
func NewManager(cfg *config.ConfigStore) *Manager {
	manager := powernapconfig.NewManager()
	if err := manager.LoadDefaults(); err != nil {
		slog.Warn("Failed to load default LSP server configs", "error", err)
	}

	// Merge user-configured LSPs into the manager.
	for name, clientConfig := range cfg.Config().LSP {
		if clientConfig.Disabled {
			slog.Debug("LSP disabled by user config", "name", name)
			manager.RemoveServer(name)
			continue
		}

		// HACK: the user might have the command name in their config instead
		// of the actual name. Find and use the correct name.
		actualName := resolveServerName(manager, name)
		manager.AddServer(actualName, &powernapconfig.ServerConfig{
			Command:     clientConfig.Command,
			Args:        clientConfig.Args,
			Environment: clientConfig.Env,
			FileTypes:   clientConfig.FileTypes,
			RootMarkers: clientConfig.RootMarkers,
			InitOptions: clientConfig.InitOptions,
			Settings:    clientConfig.Options,
		})
	}

	return &Manager{
		clients:     csync.NewMap[string, *Client](),
		unavailable: csync.NewMap[string, time.Time](),
		startMu:     csync.NewMap[string, *sync.Mutex](),
		cfg:         cfg,
		manager:     manager,
		callback:    func(string, *Client) {}, // default no-op callback
		now:         time.Now,
		lookPath:    exec.LookPath,
	}
}

// Clients returns the map of LSP clients.
func (s *Manager) Clients() *csync.Map[string, *Client] {
	return s.clients
}

// SetCallback sets a callback that is invoked when a new LSP
// client is successfully started. This allows the coordinator to add LSP tools.
func (s *Manager) SetCallback(cb func(name string, client *Client)) {
	s.callback = cb
}

// TrackConfigured notifies the UI about user-configured LSP servers without
// starting them. Servers start on demand via Start(). The callback is a
// synchronous, in-memory notification (see app.go's SetCallback), so this
// just calls it directly instead of fanning out across goroutines.
func (s *Manager) TrackConfigured(ctx context.Context) {
	for name := range s.manager.GetServers() {
		if !s.isUserConfigured(name) {
			continue
		}
		s.callback(name, nil)
	}
}

// Start starts an LSP server that can handle the given file path.
// If an appropriate LSP is already running, this is a no-op.
func (s *Manager) Start(ctx context.Context, path string) {
	// Canonical, not just Abs: a symlinked cwd (or a differently-spelled
	// Windows path) makes a textual HasPrefix comparison fail even though
	// path is genuinely inside the working directory, silently skipping
	// LSP start.
	path = fsext.Canonical(path)
	if !fsext.HasPrefix(path, fsext.Canonical(s.cfg.WorkingDir())) {
		return
	}

	var wg sync.WaitGroup
	for name, server := range s.manager.GetServers() {
		wg.Go(func() {
			s.startServer(name, path, server)
		})
	}
	wg.Wait()
}

// WorkspaceClients starts configured servers for representative files below root
// and returns the clients scoped to that workspace.  It deliberately limits the
// walk: server selection only needs one supported file, not an index of the tree.
func (s *Manager) WorkspaceClients(ctx context.Context, root string) []*Client {
	root = fsext.Canonical(root)
	if !fsext.HasPrefix(root, fsext.Canonical(s.cfg.WorkingDir())) {
		return nil
	}
	servers := s.manager.GetServers()
	found := make(map[string]bool)
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || len(found) == len(servers) {
			return filepath.SkipDir
		}
		if d.IsDir() {
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		for name, server := range servers {
			if !found[name] && handles(server, path, root) {
				found[name] = true
				s.startServer(name, path, server)
			}
		}
		return nil
	})
	clients := make([]*Client, 0)
	for _, client := range s.clients.Copy() {
		clients = append(clients, client)
	}
	return clients
}

// skipAutoStartCommands contains commands that are too generic or ambiguous to
// auto-start without explicit user configuration.
var skipAutoStartCommands = map[string]bool{
	"buck2":   true,
	"buf":     true,
	"cue":     true,
	"dart":    true,
	"deno":    true,
	"dotnet":  true,
	"dprint":  true,
	"gleam":   true,
	"java":    true,
	"julia":   true,
	"koka":    true,
	"node":    true,
	"npx":     true,
	"perl":    true,
	"plz":     true,
	"python":  true,
	"python3": true,
	"R":       true,
	"racket":  true,
	"rome":    true,
	"rubocop": true,
	"ruff":    true,
	"scarb":   true,
	"solc":    true,
	"stylua":  true,
	"swipl":   true,
	"tflint":  true,
}

func (s *Manager) startServer(name, filepath string, server *powernapconfig.ServerConfig) {
	var (
		isUserConfigured = s.isUserConfigured(name)
		autoLSP          = s.cfg.Config().Options.AutoLSP
	)
	if !isUserConfigured && autoLSP != nil && !*autoLSP {
		slog.Debug("Auto-start LSP disabled", "name", name)
		return
	}

	cfg := s.buildConfig(name, server)
	if cfg.Disabled {
		return
	}

	// Fast path: reuse an already-usable client without taking the
	// per-name lock.
	if client, ok := s.clients.Get(name); ok {
		switch client.GetServerState() {
		case StateReady, StateStarting, StateDisabled:
			s.callback(name, client)
			// already done, return
			return
		}
	}

	// handles/canAutoStart don't touch shared mutable state, so run them
	// before taking the lock; they can filter out most calls cheaply.
	if isUserConfigured {
		if !handles(server, filepath, s.cfg.WorkingDir()) {
			return
		}
	} else if !s.canAutoStart(name, filepath, s.cfg.WorkingDir(), server) {
		return
	}

	// Serialize the check-then-create-then-store sequence per server
	// name: two concurrent Start calls for the same server must never
	// both pass the checks below and spawn two LSP processes. The loser
	// shuts its client down instead of leaking it.
	mu := s.startMu.GetOrSet(name, func() *sync.Mutex { return &sync.Mutex{} })
	mu.Lock()
	defer mu.Unlock()

	if client, ok := s.clients.Get(name); ok {
		switch client.GetServerState() {
		case StateReady, StateStarting, StateDisabled:
			s.callback(name, client)
			return
		}
		// Anything else (StateError, StateStopped) is about to be
		// replaced by the fresh client built below. Shut the old one
		// down first: an errored client can still own a live process,
		// and overwriting the map entry orphaned it — KillAll walks the
		// map, so nothing would ever reap it.
		client.Shutdown()
	}

	client, err := New(
		name,
		cfg,
		s.cfg.Resolver(),
		s.cfg.WorkingDir(),
		s.cfg.Config().Options.DebugLSP,
	)
	if err != nil {
		slog.Error("Failed to create LSP client", "name", name, "error", err)
		return
	}
	s.clients.Set(name, client)
	defer func() {
		s.callback(name, client)
	}()

	client.serverState.Store(StateStarting)

	// Use an independent context for initialization so that the LSP server
	// startup is not tied to the caller's request context. The caller's
	// context may have a short timeout or be canceled when the tool call
	// completes, but LSP initialization can take several seconds and the
	// server must persist beyond any single request.
	initCtx, cancel := context.WithTimeout(context.Background(), time.Duration(cmp.Or(cfg.Timeout, 30))*time.Second)
	defer cancel()

	if _, err := client.Initialize(initCtx, s.cfg.WorkingDir()); err != nil {
		slog.Error("LSP client initialization failed", "name", name, "error", err)
		// Before the deferred callback fires, or it publishes this
		// client as still starting — and since the entry is dropped on
		// the next line, nothing ever corrects that: the UI showed a
		// server stuck at "starting" for the rest of the session.
		client.SetServerState(StateError)
		client.Shutdown()
		s.clients.Del(name)
		return
	}

	if err := client.WaitForServerReady(initCtx); err != nil {
		slog.Warn("LSP server not fully ready, continuing anyway", "name", name, "error", err)
		client.SetServerState(StateError)
	} else {
		client.SetServerState(StateReady)
	}

	slog.Debug("LSP client started", "name", name)
}

func (s *Manager) canAutoStart(
	name, filePath, workDir string,
	server *powernapconfig.ServerConfig,
) bool {
	if skipAutoStartCommands[server.Command] {
		slog.Debug("LSP command too generic for auto-start, skipping", "name", name, "command", server.Command)
		return false
	}

	// Filtering by file type is cheap and usually rejects a server before the
	// root marker check. Do both before searching PATH, which can require a stat
	// for every directory in PATH for every bundled server.
	if !handles(server, filePath, workDir) {
		return false
	}

	if s.recentlyUnavailable(name) {
		return false
	}
	if _, err := s.lookPath(server.Command); err != nil {
		slog.Debug("LSP server not installed, skipping", "name", name, "command", server.Command)
		s.markUnavailable(name)
		return false
	}
	s.clearUnavailable(name)
	return true
}

func (s *Manager) isUserConfigured(name string) bool {
	cfg, ok := s.cfg.Config().LSP[name]
	return ok && !cfg.Disabled
}

func (s *Manager) recentlyUnavailable(name string) bool {
	lastUnavailableAt, exists := s.unavailable.Get(name)
	if !exists {
		return false
	}
	if s.now().Sub(lastUnavailableAt) < unavailableRetryDelay {
		return true
	}
	s.unavailable.Del(name)
	return false
}

func (s *Manager) markUnavailable(name string) {
	s.unavailable.Set(name, s.now())
}

func (s *Manager) clearUnavailable(name string) {
	s.unavailable.Del(name)
}

func (s *Manager) buildConfig(name string, server *powernapconfig.ServerConfig) config.LSPConfig {
	cfg := config.LSPConfig{
		Command:     server.Command,
		Args:        server.Args,
		Env:         server.Environment,
		FileTypes:   server.FileTypes,
		RootMarkers: server.RootMarkers,
		InitOptions: server.InitOptions,
		Options:     server.Settings,
	}
	if userCfg, ok := s.cfg.Config().LSP[name]; ok {
		cfg.Timeout = userCfg.Timeout
	}
	return cfg
}

func resolveServerName(manager *powernapconfig.Manager, name string) string {
	if _, ok := manager.GetServer(name); ok {
		return name
	}
	for sname, server := range manager.GetServers() {
		if server.Command == name {
			return sname
		}
	}
	return name
}

func handlesFiletype(sname string, fileTypes []string, filePath string) bool {
	if len(fileTypes) == 0 {
		return true
	}

	kind := powernap.DetectLanguage(filePath)
	name := strings.ToLower(filepath.Base(filePath))
	for _, filetype := range fileTypes {
		suffix := strings.ToLower(filetype)
		if !strings.HasPrefix(suffix, ".") {
			suffix = "." + suffix
		}
		if strings.HasSuffix(name, suffix) || filetype == string(kind) {
			slog.Debug("Handles file", "name", sname, "file", name, "filetype", filetype, "kind", kind)
			return true
		}
	}

	slog.Debug("Doesn't handle file", "name", sname, "file", name)
	return false
}

func hasRootMarkers(dir string, markers []string) bool {
	if len(markers) == 0 {
		return true
	}
	for _, pattern := range markers {
		// Use filepath.Glob for a non-recursive check in the root
		// directory. This avoids walking the entire tree (which is
		// catastrophic in large monorepos with node_modules, etc.).
		matches, err := filepath.Glob(filepath.Join(dir, pattern))
		if err == nil && len(matches) > 0 {
			return true
		}
	}
	return false
}

func handles(server *powernapconfig.ServerConfig, filePath, workDir string) bool {
	return handlesFiletype(server.Command, server.FileTypes, filePath) &&
		hasRootMarkers(workDir, server.RootMarkers)
}

// KillAll force-kills all the LSP clients.
//
// This is generally faster than [Manager.StopAll] because it doesn't wait for
// the server to exit gracefully, but it can lead to data loss if the server is
// in the middle of writing something.
// Generally it doesn't matter when shutting down Sennit, though.
func (s *Manager) KillAll(context.Context) {
	var wg sync.WaitGroup
	for name, client := range s.clients.Seq2() {
		wg.Go(func() {
			defer func() { s.callback(name, client) }()
			client.Shutdown()
			client.SetServerState(StateStopped)
			s.clients.Del(name)
			slog.Debug("Killed LSP client", "name", name)
		})
	}
	wg.Wait()
}

// StopAll stops all running LSP clients and clears the client map.
func (s *Manager) StopAll(ctx context.Context) {
	var wg sync.WaitGroup
	for name, client := range s.clients.Seq2() {
		wg.Go(func() {
			defer func() { s.callback(name, client) }()
			if err := client.Close(ctx); err != nil &&
				!errors.Is(err, io.EOF) &&
				!errors.Is(err, context.Canceled) &&
				!errors.Is(err, jsonrpc2.ErrClosed) &&
				!processKilledBySignal(err) {
				slog.Warn("Failed to stop LSP client", "name", name, "error", err)
			}
			client.cancelCtx()
			client.SetServerState(StateStopped)
			s.clients.Del(name)
			slog.Debug("Stopped LSP client", "name", name)
		})
	}
	wg.Wait()
}

// processKilledBySignal reports whether err is the *exec.ExitError produced
// when the LSP server's process was terminated by a signal rather than
// exiting on its own (Close's closeCtx timeout falls back to Kill(), and
// exec.CommandContext also kills the process on cancellation) — an expected
// outcome, not a failure worth warning about.
func processKilledBySignal(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ProcessState != nil && !exitErr.Exited()
}
