package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	powernap "github.com/charmbracelet/x/powernap/pkg/lsp"
	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/charmbracelet/x/powernap/pkg/transport"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/home"
)

// DiagnosticCounts holds the count of diagnostics by severity.
type DiagnosticCounts struct {
	Error       int
	Warning     int
	Information int
	Hint        int
}

type clientGeneration struct {
	client *powernap.Client
	ctx    context.Context
	cancel context.CancelFunc
}

type diagnosticEvent struct {
	generation *clientGeneration
	prepare    func() func()
	run        func()
	terminal   bool
}

var errClientShutdown = errors.New("lsp client is shut down")

type Client struct {
	generation  atomic.Pointer[clientGeneration]
	lifecycleMu sync.Mutex
	shutdown    bool
	name        string
	debug       bool

	// Working directory this LSP is scoped to.
	cwd string

	// File types this LSP server handles (e.g., .go, .rs, .py)
	fileTypes []string

	// Configuration for this LSP client
	config config.LSPConfig

	resolver config.VariableResolver

	// Diagnostic change callback
	onDiagnosticsChanged        func(name string, count int)
	diagnosticsCallbackMu       sync.RWMutex
	diagnosticEventsMu          sync.Mutex
	diagnosticEventsCond        *sync.Cond
	diagnosticEvents            []diagnosticEvent
	diagnosticEventsDone        chan struct{}
	diagnosticEventsStop        bool
	diagnosticGeneration        *clientGeneration
	beforeDiagnosticEventCommit func()

	// Diagnostic cache
	diagnosticsMu sync.RWMutex
	diagnostics   *csync.VersionedMap[protocol.DocumentURI, []protocol.Diagnostic]

	// Cached diagnostic counts to avoid map copy on every UI render.
	diagCountsCache   DiagnosticCounts
	diagCountsVersion uint64
	diagCountsMu      sync.Mutex

	// Files are currently opened by the LSP
	openFiles *csync.Map[string, *OpenFileInfo]

	// Server state
	serverState atomic.Value
}

// New creates a new LSP client using the powernap implementation.
func New(
	name string,
	cfg config.LSPConfig,
	resolver config.VariableResolver,
	cwd string,
	debug bool,
) (*Client, error) {
	// Use a long-lived context independent of the caller's request context.
	// The caller's context may be canceled when the tool call completes,
	// but the LSP client must survive across multiple requests and restarts.
	clientCtx, cancelCtx := context.WithCancel(context.Background())
	client := &Client{
		name:        name,
		fileTypes:   cfg.FileTypes,
		diagnostics: csync.NewVersionedMap[protocol.DocumentURI, []protocol.Diagnostic](),
		openFiles:   csync.NewMap[string, *OpenFileInfo](),
		config:      cfg,
		debug:       debug,
		resolver:    resolver,
		cwd:         cwd,
	}
	client.serverState.Store(StateStopped)

	gen, err := client.createGeneration(clientCtx, cancelCtx)
	if err != nil {
		cancelCtx()
		return nil, err
	}
	client.generation.Store(gen)
	client.diagnosticGeneration = gen
	client.startDiagnosticDispatcher()

	return client, nil
}

// Initialize initializes the LSP client and returns the server capabilities.
func (c *Client) Initialize(ctx context.Context, workspaceDir string) (*protocol.InitializeResult, error) {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	gen := c.currentGeneration()
	// Register handlers for requests the server may send during the
	// initialize handshake itself (e.g. typescript-language-server issuing
	// window/workDoneProgress/create while loading the project, before
	// initialize has returned). Registering after client.Initialize() is too
	// late for those — the server treats an unhandled response as fatal.
	c.registerHandlers(gen)

	if err := gen.client.Initialize(ctx, false); err != nil {
		return nil, fmt.Errorf("failed to initialize the lsp client: %w", err)
	}

	// Keep the complete server capability payload, including union providers.
	result := &protocol.InitializeResult{Capabilities: gen.client.GetCapabilities()}

	return result, nil
}

// closeTimeout is the maximum time to wait for a graceful LSP shutdown.
const closeTimeout = 5 * time.Second

// Kill kills the client without doing anything else.
func (c *Client) Kill() {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	c.currentGeneration().client.Kill()
}

// Shutdown permanently cancels the client's long-lived context and kills the
// underlying process. Unlike Restart, this is terminal: the client cannot be
// reused after Shutdown.
func (c *Client) Shutdown() {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if c.shutdown {
		return
	}
	c.shutdown = true
	c.publishShutdown()
	gen := c.currentGeneration()
	gen.cancel()
	gen.client.Kill()
}

// GetOffsetEncoding returns the negotiated offset encoding for this client.
func (c *Client) GetOffsetEncoding() powernap.OffsetEncoding {
	return c.currentGeneration().client.GetOffsetEncoding()
}

// Close closes all open files in the client, then shuts down gracefully.
// If shutdown takes longer than closeTimeout, it falls back to Kill().
func (c *Client) Close(ctx context.Context) error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	gen := c.currentGeneration()
	return c.close(ctx, gen)
}

func (c *Client) close(ctx context.Context, gen *clientGeneration) error {
	c.closeAllFiles(ctx, gen)

	// Use a timeout to prevent hanging on unresponsive LSP servers.
	// jsonrpc2's send lock doesn't respect context cancellation, so we
	// need to fall back to Kill() which closes the underlying connection.
	closeCtx, cancel := context.WithTimeout(ctx, closeTimeout)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		if err := gen.client.Shutdown(closeCtx); err != nil {
			slog.Warn("Failed to shutdown LSP client", "error", err)
		}
		done <- gen.client.Exit()
	}()

	select {
	case err := <-done:
		return err
	case <-closeCtx.Done():
		gen.client.Kill()
		return closeCtx.Err()
	}
}

// createGeneration creates an unpublished LSP runtime generation.
func (c *Client) createGeneration(ctx context.Context, cancel context.CancelFunc) (*clientGeneration, error) {
	rootURI := string(protocol.URIFromPath(c.cwd))

	command, err := c.resolver.ResolveValue(c.config.Command)
	if err != nil {
		return nil, fmt.Errorf("invalid lsp command: %w", err)
	}

	args, err := c.config.ResolvedArgs(c.resolver)
	if err != nil {
		return nil, fmt.Errorf("invalid lsp args: %w", err)
	}

	envs, err := c.config.ResolvedEnv(c.resolver)
	if err != nil {
		return nil, fmt.Errorf("invalid lsp env: %w", err)
	}

	clientConfig := powernap.ClientConfig{
		Command:     home.Long(command),
		Args:        args,
		RootURI:     rootURI,
		Environment: envs,
		Settings:    c.config.Options,
		InitOptions: c.config.InitOptions,
		WorkspaceFolders: []protocol.WorkspaceFolder{
			{
				URI:  rootURI,
				Name: filepath.Base(c.cwd),
			},
		},
	}

	powernapClient, err := powernap.NewClient(clientConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create lsp client: %w", err)
	}

	return &clientGeneration{client: powernapClient, ctx: ctx, cancel: cancel}, nil
}

func (c *Client) currentGeneration() *clientGeneration {
	return c.generation.Load()
}

func (c *Client) startDiagnosticDispatcher() {
	c.diagnosticEventsCond = sync.NewCond(&c.diagnosticEventsMu)
	c.diagnosticEventsDone = make(chan struct{})
	go func() {
		defer close(c.diagnosticEventsDone)
		for {
			c.diagnosticEventsMu.Lock()
			for len(c.diagnosticEvents) == 0 {
				c.diagnosticEventsCond.Wait()
			}
			event := c.diagnosticEvents[0]
			c.diagnosticEvents = c.diagnosticEvents[1:]
			hook := c.beforeDiagnosticEventCommit
			c.diagnosticEventsMu.Unlock()
			if hook != nil {
				hook()
			}
			c.diagnosticEventsMu.Lock()
			if event.generation != nil && c.diagnosticGeneration != event.generation {
				c.diagnosticEventsMu.Unlock()
				continue
			}
			run := event.run
			if event.prepare != nil {
				run = event.prepare()
			}
			c.diagnosticEventsMu.Unlock()
			if run != nil {
				run()
			}
			if event.terminal {
				return
			}
		}
	}()
}

func (c *Client) dispatchDiagnosticEvent(event diagnosticEvent) bool {
	c.diagnosticEventsMu.Lock()
	defer c.diagnosticEventsMu.Unlock()
	if c.diagnosticEventsStop || event.generation != nil && c.diagnosticGeneration != event.generation {
		return false
	}
	c.diagnosticEvents = append(c.diagnosticEvents, event)
	c.diagnosticEventsCond.Signal()
	return true
}

func (c *Client) waitForDiagnosticEvents() {
	done := make(chan struct{})
	if !c.dispatchDiagnosticEvent(diagnosticEvent{run: func() { close(done) }}) {
		return
	}
	select {
	case <-done:
	case <-c.diagnosticEventsDone:
	}
}

func (c *Client) invokeDiagnosticsCallback(count int) {
	c.diagnosticsCallbackMu.RLock()
	callback := c.onDiagnosticsChanged
	c.diagnosticsCallbackMu.RUnlock()
	if callback != nil {
		callback(c.name, count)
	}
}

// registerHandlers registers the standard LSP notification and request handlers.
func (c *Client) registerHandlers(gen *clientGeneration) {
	gen.client.RegisterHandler("workspace/applyEdit", HandleApplyEdit(gen.client.GetOffsetEncoding))
	gen.client.RegisterHandler("workspace/configuration", HandleWorkspaceConfiguration)
	gen.client.RegisterHandler("client/registerCapability", HandleRegisterCapability)
	gen.client.RegisterHandler("window/workDoneProgress/create", HandleWorkDoneProgressCreate)
	gen.client.RegisterNotificationHandler("window/showMessage", func(ctx context.Context, method string, params json.RawMessage) {
		if c.debug {
			HandleServerMessage(ctx, method, params)
		}
	})
	gen.client.RegisterNotificationHandler("textDocument/publishDiagnostics", func(_ context.Context, _ string, params json.RawMessage) {
		c.handleDiagnostics(gen, params)
	})
}

// Restart closes the current LSP client and creates a new one with the same configuration.
func (c *Client) Restart() error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if c.shutdown {
		return errClientShutdown
	}

	var openFiles []string
	for uri := range c.openFiles.Seq2() {
		openFiles = append(openFiles, uri)
	}

	oldGen := c.currentGeneration()
	oldGen.cancel()

	closeCtx, cancelClose := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelClose()

	if err := c.close(closeCtx, oldGen); err != nil {
		slog.Warn("Error closing client during restart", "name", c.name, "error", err)
	}

	c.SetServerState(StateStopped)

	ctx, cancel := context.WithCancel(context.Background())
	gen, err := c.createGeneration(ctx, cancel)
	if err != nil {
		cancel()
		return err
	}

	c.publishGeneration(gen)

	initCtx, cancelInit := context.WithTimeout(gen.ctx, 30*time.Second)
	defer cancelInit()

	c.SetServerState(StateStarting)

	// Register handlers before Initialize so servers that send
	// requests during the handshake (e.g. window/workDoneProgress/create)
	// don't crash on an unhandled response.
	c.registerHandlers(gen)

	if err := gen.client.Initialize(initCtx, false); err != nil {
		gen.cancel()
		gen.client.Kill()
		c.SetServerState(StateError)
		return fmt.Errorf("failed to initialize lsp client: %w", err)
	}

	if err := c.waitForServerReady(initCtx, gen); err != nil {
		gen.cancel()
		gen.client.Kill()
		slog.Error("Server failed to become ready after restart", "name", c.name, "error", err)
		c.SetServerState(StateError)
		return err
	}

	for _, uri := range openFiles {
		// openFiles was collected from c.openFiles, which is keyed by URI,
		// but OpenFile takes a filesystem path (it checks HandlesFile,
		// which compares against c.cwd). Convert before calling it, or
		// the reopen silently no-ops.
		path, err := protocol.DocumentURI(uri).Path()
		if err != nil {
			slog.Warn("Failed to convert URI to path for reopen", "uri", uri, "error", err)
			continue
		}
		if err := c.OpenFile(initCtx, path); err != nil {
			slog.Warn("Failed to reopen file after restart", "file", path, "error", err)
		}
	}
	return nil
}

// ServerState represents the state of an LSP server
type ServerState int

const (
	StateUnstarted ServerState = iota
	StateStarting
	StateReady
	StateError
	StateStopped
	StateDisabled
)

// GetServerState returns the current state of the LSP server
func (c *Client) GetServerState() ServerState {
	if val := c.serverState.Load(); val != nil {
		return val.(ServerState)
	}
	return StateStarting
}

// SetServerState sets the current state of the LSP server
func (c *Client) SetServerState(state ServerState) {
	c.serverState.Store(state)
}

// GetName returns the name of the LSP client
func (c *Client) GetName() string {
	return c.name
}

// FileTypes returns the file types this LSP client handles
func (c *Client) FileTypes() []string {
	return slices.Clone(c.fileTypes)
}

// SetDiagnosticsCallback sets the callback function for diagnostic changes
func (c *Client) SetDiagnosticsCallback(callback func(name string, count int)) {
	c.diagnosticsCallbackMu.Lock()
	defer c.diagnosticsCallbackMu.Unlock()
	c.onDiagnosticsChanged = callback
}

// WaitForServerReady waits for the server to be ready
func (c *Client) WaitForServerReady(ctx context.Context) error {
	return c.waitForServerReady(ctx, c.currentGeneration())
}

func (c *Client) waitForServerReady(ctx context.Context, gen *clientGeneration) error {
	// Set initial state
	c.SetServerState(StateStarting)

	// Try to ping the server with a simple request
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	if c.debug {
		slog.Debug("Waiting for LSP server to be ready...")
	}

	c.openKeyConfigFiles(ctx)

	for {
		select {
		case <-ctx.Done():
			c.SetServerState(StateError)
			return fmt.Errorf("timeout waiting for LSP server to be ready")
		case <-ticker.C:
			// Check if client is running
			if !gen.client.IsRunning() {
				if c.debug {
					slog.Debug("LSP server not ready yet", "server", c.name)
				}
				continue
			}

			// Server is ready
			c.SetServerState(StateReady)
			if c.debug {
				slog.Debug("LSP server is ready")
			}
			return nil
		}
	}
}

// OpenFileInfo contains information about an open file. Version is an
// atomic.Int32, not a plain int32: the *OpenFileInfo pointer returned by
// openFiles.Get is shared, and NotifyChange and RefreshOpenFiles can both
// bump it for the same file from different goroutines (e.g. a debounced
// edit notification racing a workspace-wide refresh).
type OpenFileInfo struct {
	Version atomic.Int32
	URI     protocol.DocumentURI
}

// HandlesFile checks if this LSP client handles the given file based on its
// extension and whether it's within the working directory.
func (c *Client) HandlesFile(path string) bool {
	if c == nil {
		return false
	}
	if !fsext.HasPrefix(path, c.cwd) {
		slog.Debug("File outside workspace", "name", c.name, "file", path, "workDir", c.cwd)
		return false
	}
	return handlesFiletype(c.name, c.fileTypes, path)
}

// OpenFile opens a file in the LSP server.
func (c *Client) OpenFile(ctx context.Context, filepath string) error {
	if !c.HandlesFile(filepath) {
		return nil
	}

	uri := string(protocol.URIFromPath(filepath))

	if _, exists := c.openFiles.Get(uri); exists {
		return nil // Already open
	}

	// Skip files that do not exist or cannot be read
	content, err := os.ReadFile(filepath)
	if err != nil {
		return fmt.Errorf("error reading file: %w", err)
	}

	// Notify the server about the opened document
	if err = c.currentGeneration().client.NotifyDidOpenTextDocument(ctx, uri, string(powernap.DetectLanguage(filepath)), 1, string(content)); err != nil {
		return err
	}

	info := &OpenFileInfo{URI: protocol.DocumentURI(uri)}
	info.Version.Store(1)
	c.openFiles.Set(uri, info)

	return nil
}

// NotifyChange notifies the server about a file change.
func (c *Client) NotifyChange(ctx context.Context, filepath string) error {
	if c == nil {
		return nil
	}
	uri := string(protocol.URIFromPath(filepath))

	content, err := os.ReadFile(filepath)
	if err != nil {
		return fmt.Errorf("error reading file: %w", err)
	}

	fileInfo, isOpen := c.openFiles.Get(uri)
	if !isOpen {
		return fmt.Errorf("cannot notify change for unopened file: %s", filepath)
	}

	// Increment version. Atomic because the *OpenFileInfo pointer is
	// shared and RefreshOpenFiles can bump the same file's version
	// concurrently.
	newVersion := fileInfo.Version.Add(1)

	// Create change event
	changes := []protocol.TextDocumentContentChangeEvent{
		{
			Value: protocol.TextDocumentContentChangeWholeDocument{
				Text: string(content),
			},
		},
	}

	return c.currentGeneration().client.NotifyDidChangeTextDocument(ctx, uri, int(newVersion), changes)
}

// IsFileOpen checks if a file is currently open.
func (c *Client) IsFileOpen(filepath string) bool {
	uri := string(protocol.URIFromPath(filepath))
	_, exists := c.openFiles.Get(uri)
	return exists
}

// CloseAllFiles closes all currently open files.
//
// The bookkeeping is dropped whether or not the notification lands. It
// used to be kept on failure, which is exactly backwards: the case where
// didClose fails is a server that is already dead, and the caller is
// Close — after which this client's idea of what is open describes a
// server that no longer exists. Restart then reopened nothing, because
// OpenFile treats a URI still in the map as already open, and the fresh
// server received not one didOpen.
func (c *Client) CloseAllFiles(ctx context.Context) {
	c.closeAllFiles(ctx, c.currentGeneration())
}

func (c *Client) closeAllFiles(ctx context.Context, gen *clientGeneration) {
	for uri := range c.openFiles.Seq2() {
		if c.debug {
			slog.Debug("Closing file", "file", uri)
		}
		if err := gen.client.NotifyDidCloseTextDocument(ctx, uri); err != nil {
			slog.Warn("Error closing file", "uri", uri, "error", err)
		}
		c.openFiles.Del(uri)
	}
}

// GetFileDiagnostics returns diagnostics for a specific file.
func (c *Client) GetFileDiagnostics(uri protocol.DocumentURI) []protocol.Diagnostic {
	c.diagnosticsMu.RLock()
	defer c.diagnosticsMu.RUnlock()
	diags, _ := c.diagnostics.Get(uri)
	return diags
}

// GetDiagnostics returns all diagnostics for all files.
func (c *Client) GetDiagnostics() map[protocol.DocumentURI][]protocol.Diagnostic {
	if c == nil {
		return nil
	}
	c.diagnosticsMu.RLock()
	defer c.diagnosticsMu.RUnlock()
	return c.diagnostics.Copy()
}

// GetDiagnosticCounts returns cached diagnostic counts by severity.
// Uses the VersionedMap version to avoid recomputing on every call.
func (c *Client) resetDiagnostics() {
	c.diagnosticEventsMu.Lock()
	c.diagnosticsMu.Lock()
	c.resetDiagnosticsLocked()
	c.diagnosticsMu.Unlock()
	c.diagnosticEventsMu.Unlock()
}

func (c *Client) publishGeneration(gen *clientGeneration) {
	c.diagnosticEventsMu.Lock()
	c.diagnosticsMu.Lock()
	hadDiagnostics := c.diagnostics.Version() != 0
	c.resetDiagnosticsLocked()
	c.generation.Store(gen)
	c.diagnosticGeneration = gen
	c.diagnosticsMu.Unlock()
	c.purgeDiagnosticEventsLocked()
	if hadDiagnostics {
		c.diagnosticEvents = append(c.diagnosticEvents, diagnosticEvent{run: func() {
			c.invokeDiagnosticsCallback(0)
		}})
		c.diagnosticEventsCond.Signal()
	}
	c.diagnosticEventsMu.Unlock()
}

func (c *Client) publishShutdown() {
	c.diagnosticEventsMu.Lock()
	c.diagnosticsMu.Lock()
	hadDiagnostics := c.diagnostics.Version() != 0
	c.resetDiagnosticsLocked()
	c.diagnosticsMu.Unlock()
	c.diagnosticGeneration = nil
	c.diagnosticEventsStop = true
	c.purgeDiagnosticEventsLocked()
	c.diagnosticEvents = append(c.diagnosticEvents, diagnosticEvent{terminal: true, run: func() {
		if hadDiagnostics {
			c.invokeDiagnosticsCallback(0)
		}
	}})
	c.diagnosticEventsCond.Signal()
	c.diagnosticEventsMu.Unlock()
}

func (c *Client) purgeDiagnosticEventsLocked() {
	kept := c.diagnosticEvents[:0]
	for _, event := range c.diagnosticEvents {
		if event.generation == nil {
			kept = append(kept, event)
		}
	}
	c.diagnosticEvents = kept
}

func (c *Client) resetDiagnosticsLocked() {
	c.diagCountsMu.Lock()
	defer c.diagCountsMu.Unlock()
	c.diagnostics = csync.NewVersionedMap[protocol.DocumentURI, []protocol.Diagnostic]()
	c.diagCountsCache = DiagnosticCounts{}
	c.diagCountsVersion = 0
}

func (c *Client) GetDiagnosticCounts() DiagnosticCounts {
	if c == nil {
		return DiagnosticCounts{}
	}
	c.diagnosticsMu.RLock()
	defer c.diagnosticsMu.RUnlock()
	currentVersion := c.diagnostics.Version()

	c.diagCountsMu.Lock()
	defer c.diagCountsMu.Unlock()

	if currentVersion == c.diagCountsVersion {
		return c.diagCountsCache
	}

	// Recompute counts.
	counts := DiagnosticCounts{}
	for _, diags := range c.diagnostics.Seq2() {
		for _, diag := range diags {
			switch diag.Severity {
			case protocol.SeverityError:
				counts.Error++
			case protocol.SeverityWarning:
				counts.Warning++
			case protocol.SeverityInformation:
				counts.Information++
			case protocol.SeverityHint:
				counts.Hint++
			}
		}
	}

	c.diagCountsCache = counts
	c.diagCountsVersion = currentVersion
	return counts
}

// OpenFileOnDemand opens a file only if it's not already open.
func (c *Client) OpenFileOnDemand(ctx context.Context, filepath string) error {
	if c == nil {
		return nil
	}
	// Check if the file is already open
	if c.IsFileOpen(filepath) {
		return nil
	}

	// Open the file
	return c.OpenFile(ctx, filepath)
}

// RegisterNotificationHandler registers a notification handler.
func (c *Client) RegisterNotificationHandler(method string, handler transport.NotificationHandler) {
	c.currentGeneration().client.RegisterNotificationHandler(method, handler)
}

// RegisterServerRequestHandler handles server requests.
func (c *Client) RegisterServerRequestHandler(method string, handler transport.Handler) {
	c.currentGeneration().client.RegisterHandler(method, handler)
}

// openKeyConfigFiles opens important configuration files that help initialize the server.
func (c *Client) openKeyConfigFiles(ctx context.Context) {
	// Try to open each file, ignoring errors if they don't exist
	for _, file := range c.config.RootMarkers {
		file = filepath.Join(c.cwd, file)
		if _, err := os.Stat(file); err == nil {
			// File exists, try to open it
			if err := c.OpenFile(ctx, file); err != nil {
				slog.Error("Failed to open key config file", "file", file, "error", err)
			} else {
				slog.Debug("Opened key config file for initialization", "file", file)
			}
		}
	}
}

// NotifyWorkspaceChange sends a workspace-level file change notification to
// trigger re-analysis of all files. This is useful when the overall project
// state may have changed (e.g., after a project-wide refactoring) and
// diagnostics for files not currently being edited may be stale.
func (c *Client) NotifyWorkspaceChange(ctx context.Context) error {
	if c == nil {
		return nil
	}
	return c.currentGeneration().client.NotifyDidChangeWatchedFiles(ctx, []protocol.FileEvent{
		{URI: protocol.URIFromPath(c.cwd), Type: protocol.Changed},
	})
}

// RefreshOpenFiles re-notifies the LSP server about all currently open files,
// which triggers re-analysis and fresh diagnostics for the entire project.
func (c *Client) RefreshOpenFiles(ctx context.Context) {
	if c == nil {
		return
	}
	for uri, info := range c.openFiles.Seq2() {
		path, err := protocol.DocumentURI(uri).Path()
		if err != nil {
			slog.Warn("Failed to convert URI to path", "uri", uri, "error", err)
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			slog.Warn("Failed to read file for refresh", "path", path, "error", err)
			continue
		}
		newVersion := info.Version.Add(1)
		changes := []protocol.TextDocumentContentChangeEvent{
			{
				Value: protocol.TextDocumentContentChangeWholeDocument{
					Text: string(content),
				},
			},
		}
		if err := c.currentGeneration().client.NotifyDidChangeTextDocument(ctx, uri, int(newVersion), changes); err != nil {
			slog.Warn("Failed to notify file change", "uri", uri, "error", err)
		}
	}
}

// WaitForDiagnostics waits until diagnostics stop changing for a settling
// period, indicating the LSP server has finished processing. If no
// diagnostics change within firstChangeDuration, it returns early since the
// server likely isn't going to republish.
func (c *Client) WaitForDiagnostics(ctx context.Context, timeout time.Duration) {
	c.waitForDiagnostics(ctx, timeout, time.Second, 300*time.Millisecond, 100*time.Millisecond)
}

func (c *Client) diagnosticVersion() uint64 {
	c.diagnosticsMu.RLock()
	defer c.diagnosticsMu.RUnlock()
	return c.diagnostics.Version()
}

func (c *Client) waitForDiagnostics(
	ctx context.Context,
	timeout, firstChangeDuration, settleDuration, pollInterval time.Duration,
) {
	if c == nil {
		return
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	firstChangeTimer := time.NewTimer(min(timeout, firstChangeDuration))
	defer firstChangeTimer.Stop()
	previousVersion := c.diagnosticVersion()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-firstChangeTimer.C:
			// No change arrived quickly — server isn't republishing.
			return
		case <-ticker.C:
			currentVersion := c.diagnosticVersion()
			if currentVersion != previousVersion {
				// Diagnostics changed — now wait for them to settle.
				c.waitForDiagnosticsToSettle(ctx, deadline.C, settleDuration, pollInterval/2)
				return
			}
		}
	}
}

// waitForDiagnosticsToSettle waits until diagnostics version stays the same
// for settleDuration, indicating the LSP server has finished publishing.
func (c *Client) waitForDiagnosticsToSettle(
	ctx context.Context,
	deadline <-chan time.Time,
	settleDuration, pollInterval time.Duration,
) {
	lastVersion := c.diagnosticVersion()
	settleTicker := time.NewTicker(pollInterval)
	defer settleTicker.Stop()

	// Track how long the version has been stable.
	stableStart := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			return
		case <-settleTicker.C:
			currentVersion := c.diagnosticVersion()
			if currentVersion != lastVersion {
				// New change detected — reset the stable timer.
				lastVersion = currentVersion
				stableStart = time.Now()
			} else if time.Since(stableStart) >= settleDuration {
				// Diagnostics have been stable for the settle duration.
				return
			}
		}
	}
}

// FindReferences finds all references to the symbol at the given position.
func (c *Client) FindReferences(ctx context.Context, filepath string, line, character int, includeDeclaration bool) ([]protocol.Location, error) {
	if err := c.OpenFileOnDemand(ctx, filepath); err != nil {
		return nil, err
	}

	// Add timeout to prevent hanging on slow LSP servers.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// NOTE: line and character should be 0-based.
	// See: https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification/#position
	return c.currentGeneration().client.FindReferences(ctx, filepath, line-1, character-1, includeDeclaration)
}

// Rename renames the symbol at the given position across all files.
func (c *Client) Rename(ctx context.Context, filepath string, line, character int, newName string) (*protocol.WorkspaceEdit, error) {
	if err := c.OpenFileOnDemand(ctx, filepath); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	return c.currentGeneration().client.RequestRename(ctx, filepath, line-1, character-1, newName) //nolint:wrapcheck
}

// Hover returns hover information at a file position.
func (c *Client) Hover(ctx context.Context, filepath string, line, character int) (*protocol.Hover, error) {
	gen := c.currentGeneration()
	return gen.client.RequestHover(ctx, filepath, protocol.Position{Line: uint32(line), Character: uint32(character)}) //nolint:wrapcheck
}

type WorkspaceSymbol struct {
	Name            string
	Kind            protocol.SymbolKind
	Path            string
	Line, Character int
	Container       string
}

// WorkspaceSymbolResults normalizes the legacy SymbolInformation and modern
// WorkspaceSymbol workspace/symbol result variants.
func (c *Client) WorkspaceSymbolResults(ctx context.Context, query string) ([]WorkspaceSymbol, error) {
	raw, err := c.currentGeneration().client.RequestWorkspaceSymbols(ctx, query)
	if err != nil {
		return nil, err
	}
	return normalizeWorkspaceSymbolResults(raw)
}

func normalizeWorkspaceSymbolResults(raw protocol.Or_Result_workspace_symbol) ([]WorkspaceSymbol, error) {
	results, err := raw.Results()
	if err != nil {
		return nil, err
	}
	out := make([]WorkspaceSymbol, 0, len(results))
	for _, result := range results {
		loc := result.GetLocation()
		path, err := loc.URI.Path()
		if err != nil || path == "" {
			continue
		}
		item := WorkspaceSymbol{Name: result.GetName(), Path: path, Line: int(loc.Range.Start.Line) + 1, Character: int(loc.Range.Start.Character) + 1}
		switch v := result.(type) {
		case *protocol.WorkspaceSymbol:
			item.Kind, item.Container = v.Kind, v.ContainerName
		case *protocol.SymbolInformation:
			item.Kind, item.Container = v.Kind, v.ContainerName
		}
		out = append(out, item)
	}
	return out, nil
}

// SupportsWorkspaceSymbols reports whether the initialized server advertises workspace symbols.
func (c *Client) SupportsWorkspaceSymbols() bool {
	return c.currentGeneration().client.GetCapabilities().WorkspaceSymbolProvider != nil
}

// SupportsHover reports whether the initialized server advertises hover support.
func (c *Client) SupportsHover() bool {
	return c.currentGeneration().client.GetCapabilities().HoverProvider != nil
}

// DocumentSymbols returns the document symbols for the given file.
func (c *Client) DocumentSymbols(ctx context.Context, filepath string) ([]protocol.DocumentSymbolResult, error) {
	if err := c.OpenFileOnDemand(ctx, filepath); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	return c.currentGeneration().client.RequestDocumentSymbols(ctx, filepath) //nolint:wrapcheck
}

// Definition finds the definition of the symbol at the given position.
func (c *Client) Definition(ctx context.Context, filepath string, line, character int) ([]protocol.Location, error) {
	if err := c.OpenFileOnDemand(ctx, filepath); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	return c.currentGeneration().client.RequestDefinition(ctx, filepath, line-1, character-1) //nolint:wrapcheck
}

// PrepareCallHierarchy prepares a call hierarchy item at the given position.
func (c *Client) PrepareCallHierarchy(ctx context.Context, filepath string, line, character int) ([]protocol.CallHierarchyItem, error) {
	if err := c.OpenFileOnDemand(ctx, filepath); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	return c.currentGeneration().client.PrepareCallHierarchy(ctx, filepath, line-1, character-1) //nolint:wrapcheck
}

// IncomingCalls returns all callers of the given call hierarchy item.
func (c *Client) IncomingCalls(ctx context.Context, item protocol.CallHierarchyItem) ([]protocol.CallHierarchyIncomingCall, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	return c.currentGeneration().client.IncomingCalls(ctx, item) //nolint:wrapcheck
}

// OutgoingCalls returns all callees of the given call hierarchy item.
func (c *Client) OutgoingCalls(ctx context.Context, item protocol.CallHierarchyItem) ([]protocol.CallHierarchyOutgoingCall, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	return c.currentGeneration().client.OutgoingCalls(ctx, item) //nolint:wrapcheck
}
