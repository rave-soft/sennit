package lsp

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	powernap "github.com/charmbracelet/x/powernap/pkg/lsp"
	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/charmbracelet/x/powernap/pkg/transport"
	"github.com/rave-soft/sennit/internal/config"
)

var errClientShutdown = errors.New("lsp client is shut down")

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

// closeTimeout is the maximum time to wait for a graceful LSP shutdown.
const closeTimeout = 5 * time.Second

// Client is the façade over one LSP server. It keeps only the wiring that
// glues its components together: the runtime lifecycle (process,
// generation), the diagnostics store, the open-file tracker and the
// request dispatcher, plus the server state and identity that external
// callers observe. Everything else delegates.
type Client struct {
	runtime     *runtime
	diagnostics *diagnosticsStore
	files       *filesync
	requests    *requests
	config      config.LSPConfig
	// serverState is written by the manager, the runtime lifecycle and the
	// façade, and read by the manager and the UI from any goroutine, so it
	// must be atomic; no other lock is taken around it. The zero value
	// (StateUnstarted) is never published by this code, so readers can
	// distinguish "never started" from an actual state by the enum value
	// itself.
	serverState atomic.Int32
	name        string
	fileTypes   []string

	// shutdownOnce starts the one terminal cleanup worker. shutdownDone is
	// closed only after diagnostics quiesce and the runtime process is dead.
	shutdownOnce sync.Once
	shutdownDone chan struct{}
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
	rt := newRuntime(name, cfg, resolver, cwd, debug)

	client := &Client{
		runtime:      rt,
		requests:     newRequests(rt.currentGeneration, nil),
		config:       cfg,
		name:         name,
		fileTypes:    cfg.FileTypes,
		shutdownDone: make(chan struct{}),
	}
	client.files = newFileSync(rt.currentGeneration, cwd, name, cfg.FileTypes, cfg.RootMarkers, debug)
	client.requests.ensureOpen = client.OpenFileOnDemand

	gen, err := rt.createGeneration(clientCtx, cancelCtx)
	if err != nil {
		cancelCtx()
		return nil, err
	}
	client.diagnostics = newDiagnosticsStore(name, nil)
	client.files.diagnostics = client.diagnostics
	rt.closeFiles = client.files.closeAllFiles
	rt.setState = client.SetServerState
	rt.onDiagnosticsPublish = client.diagnostics.publish
	rt.publishInitial(gen, client.diagnostics)

	return client, nil
}

// Initialize initializes the LSP client and returns the server capabilities.
func (c *Client) Initialize(ctx context.Context, workspaceDir string) (*protocol.InitializeResult, error) {
	c.runtime.mu.Lock()
	defer c.runtime.mu.Unlock()
	gen := c.runtime.currentGeneration()
	// Handler registration happens inside runtime.initialize, before it
	// calls gen.client.Initialize — see that method's doc comment.
	return c.runtime.initialize(ctx, gen)
}

// Kill kills the client without doing anything else.
func (c *Client) Kill() {
	c.runtime.Kill()
}

// Shutdown permanently cancels the client's long-lived context and kills the
// underlying process. Unlike Restart, this is terminal: the client cannot be
// reused after Shutdown.
//
// Shutdown is quiescent: when it returns from an external goroutine, no
// diagnostics callback that started before or during the shutdown can still
// run or fire later, and the diagnostics dispatcher has terminated. It waits
// for the diagnostics store to reach that quiescence point and for the
// runtime to have canceled and killed the current process.
//
// Shutdown first closes the lifecycle gate, then drains diagnostics and
// finally kills the process. Closing the lifecycle gate before waiting means
// a callback cannot successfully restart the client once shutdown begins.
//
// Shutdown must not be called by a diagnostics callback: callbacks execute on
// the diagnostics dispatcher, and this method deliberately waits for that
// dispatcher to exit.
func (c *Client) Shutdown() {
	c.startShutdown()
	<-c.shutdownDone
}

func (c *Client) startShutdown() {
	c.runtime.beginShutdown()
	c.shutdownOnce.Do(func() {
		c.diagnostics.requestShutdown()
		go func() {
			<-c.diagnostics.done
			c.runtime.Shutdown()
			close(c.shutdownDone)
		}()
	})
}

// GetOffsetEncoding returns the negotiated offset encoding for this client.
func (c *Client) GetOffsetEncoding() powernap.OffsetEncoding {
	return c.runtime.currentGeneration().client.GetOffsetEncoding()
}

// Close closes all open files in the client, then shuts down gracefully.
// If shutdown takes longer than closeTimeout, it falls back to Kill().
func (c *Client) Close(ctx context.Context) error {
	gen := c.runtime.currentGeneration()
	return c.runtime.close(ctx, gen)
}

// Restart closes the current LSP client and creates a new one with the same
// configuration, then reopens every file that was open before the restart.
//
// Publication is single-step and happens inside the runtime lifecycle gate
// (r.mu): the new generation is swapped into the runtime, the diagnostics
// store and the server state as one orchestrated step, only after
// Initialize+WaitForServerReady have succeeded on the new process. Until
// that swap, requests and file notifications keep talking to the old
// generation — which is dead but coherent — so a failed restart can never
// leave a killed candidate published as current.
func (c *Client) Restart() error {
	return c.runtime.restart(c.diagnostics, c.files.prepareSync)
}

// GetServerState returns the current state of the LSP server
func (c *Client) GetServerState() ServerState {
	if c == nil {
		return StateStarting
	}
	return ServerState(c.serverState.Load())
}

// SetServerState sets the current state of the LSP server
func (c *Client) SetServerState(state ServerState) {
	if c == nil {
		return
	}
	c.serverState.Store(int32(state))
}

// GetName returns the name of the LSP client
func (c *Client) GetName() string {
	if c == nil {
		return ""
	}
	return c.name
}

// FileTypes returns the file types this LSP client handles
func (c *Client) FileTypes() []string {
	return slices.Clone(c.fileTypes)
}

// SetDiagnosticsCallback sets the callback function for diagnostic changes
func (c *Client) SetDiagnosticsCallback(callback func(name string, count int)) {
	c.diagnostics.SetDiagnosticsCallback(callback)
}

// WaitForServerReady waits for the server to be ready. It takes the
// lifecycle gate so it cannot race a restart: the generation it polls is
// the one that is current for the whole wait, and the state it reports
// belongs to that generation.
func (c *Client) WaitForServerReady(ctx context.Context) error {
	c.runtime.mu.Lock()
	defer c.runtime.mu.Unlock()
	gen := c.runtime.currentGeneration()
	prepare := c.files.prepareSync()
	commit, err := prepare(ctx, gen)
	if err != nil {
		return err
	}
	if err := c.runtime.waitForServerReady(ctx, gen, true); err != nil {
		return err
	}
	commit()
	return nil
}

// HandlesFile checks if this LSP client handles the given file based on its
// extension and whether it's within the working directory.
func (c *Client) HandlesFile(path string) bool {
	if c == nil {
		return false
	}
	return c.files.handlesFile(path)
}

// OpenFile opens a file in the LSP server. handlesFile is checked inside
// files.openFile, so it is not duplicated here.
func (c *Client) OpenFile(ctx context.Context, filepath string) error {
	return c.files.openFile(ctx, filepath)
}

// NotifyChange notifies the server about a file change.
func (c *Client) NotifyChange(ctx context.Context, filepath string) error {
	if c == nil {
		return nil
	}
	return c.files.notifyChange(ctx, filepath)
}

// IsFileOpen checks if a file is currently open.
func (c *Client) IsFileOpen(filepath string) bool {
	return c.files.isFileOpen(filepath)
}

// CloseAllFiles closes all currently open files.
func (c *Client) CloseAllFiles(ctx context.Context) {
	c.files.closeAllFiles(ctx, c.runtime.currentGeneration())
}

// GetFileDiagnostics returns diagnostics for a specific file.
func (c *Client) GetFileDiagnostics(uri protocol.DocumentURI) []protocol.Diagnostic {
	return c.diagnostics.getFileDiagnostics(uri)
}

// GetDiagnostics returns all diagnostics for all files.
func (c *Client) GetDiagnostics() map[protocol.DocumentURI][]protocol.Diagnostic {
	if c == nil {
		return nil
	}
	return c.diagnostics.getDiagnostics()
}

// GetDiagnosticCounts returns cached diagnostic counts by severity.
func (c *Client) GetDiagnosticCounts() DiagnosticCounts {
	if c == nil {
		return DiagnosticCounts{}
	}
	return c.diagnostics.getDiagnosticCounts()
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
	c.runtime.currentGeneration().client.RegisterNotificationHandler(method, handler)
}

// RegisterServerRequestHandler handles server requests.
func (c *Client) RegisterServerRequestHandler(method string, handler transport.Handler) {
	c.runtime.currentGeneration().client.RegisterHandler(method, handler)
}

// NotifyWorkspaceChange sends a workspace-level file change notification to
// trigger re-analysis of all files. This is useful when the overall project
// state may have changed (e.g., after a project-wide refactoring) and
// diagnostics for files not currently being edited may be stale.
func (c *Client) NotifyWorkspaceChange(ctx context.Context) error {
	if c == nil {
		return nil
	}
	return c.files.notifyWorkspaceChange(ctx)
}

// RefreshOpenFiles re-notifies the LSP server about all currently open files,
// which triggers re-analysis and fresh diagnostics for the entire project.
func (c *Client) RefreshOpenFiles(ctx context.Context) {
	if c == nil {
		return
	}
	c.files.refreshOpenFiles(ctx)
}

// WaitForDiagnostics waits until diagnostics stop changing for a settling
// period, indicating the LSP server has finished processing. If no
// diagnostics change within firstChangeDuration, it returns early since the
// server likely isn't going to republish.
func (c *Client) WaitForDiagnostics(ctx context.Context, timeout time.Duration) {
	if c == nil {
		return
	}
	c.diagnostics.waitForDiagnostics(ctx, timeout, time.Second, 300*time.Millisecond, 100*time.Millisecond)
}

// FindReferences finds all references to the symbol at the given position.
func (c *Client) FindReferences(ctx context.Context, filepath string, line, character int, includeDeclaration bool) ([]protocol.Location, error) {
	return c.requests.FindReferences(ctx, filepath, line, character, includeDeclaration)
}

// Rename renames the symbol at the given position across all files.
func (c *Client) Rename(ctx context.Context, filepath string, line, character int, newName string) (*protocol.WorkspaceEdit, error) {
	return c.requests.Rename(ctx, filepath, line, character, newName)
}

// Hover returns hover information at a file position.
func (c *Client) Hover(ctx context.Context, filepath string, line, character int) (*protocol.Hover, error) {
	return c.requests.Hover(ctx, filepath, line, character)
}

// WorkspaceSymbolResults normalizes the legacy SymbolInformation and modern
// WorkspaceSymbol workspace/symbol result variants.
func (c *Client) WorkspaceSymbolResults(ctx context.Context, query string) ([]WorkspaceSymbol, error) {
	return c.requests.WorkspaceSymbolResults(ctx, query)
}

// SupportsWorkspaceSymbols reports whether the initialized server advertises workspace symbols.
func (c *Client) SupportsWorkspaceSymbols() bool {
	return c.requests.SupportsWorkspaceSymbols()
}

// SupportsHover reports whether the initialized server advertises hover support.
func (c *Client) SupportsHover() bool {
	return c.requests.SupportsHover()
}

// DocumentSymbols returns the document symbols for the given file.
func (c *Client) DocumentSymbols(ctx context.Context, filepath string) ([]protocol.DocumentSymbolResult, error) {
	return c.requests.DocumentSymbols(ctx, filepath)
}

// Definition finds the definition of the symbol at the given position.
func (c *Client) Definition(ctx context.Context, filepath string, line, character int) ([]protocol.Location, error) {
	return c.requests.Definition(ctx, filepath, line, character)
}

// PrepareCallHierarchy prepares a call hierarchy item at the given position.
func (c *Client) PrepareCallHierarchy(ctx context.Context, filepath string, line, character int) ([]protocol.CallHierarchyItem, error) {
	return c.requests.PrepareCallHierarchy(ctx, filepath, line, character)
}

// IncomingCalls returns all callers of the given call hierarchy item.
func (c *Client) IncomingCalls(ctx context.Context, item protocol.CallHierarchyItem) ([]protocol.CallHierarchyIncomingCall, error) {
	return c.requests.IncomingCalls(ctx, item)
}

// OutgoingCalls returns all callees of the given call hierarchy item.
func (c *Client) OutgoingCalls(ctx context.Context, item protocol.CallHierarchyItem) ([]protocol.CallHierarchyOutgoingCall, error) {
	return c.requests.OutgoingCalls(ctx, item)
}
