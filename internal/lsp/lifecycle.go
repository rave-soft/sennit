package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	powernap "github.com/charmbracelet/x/powernap/pkg/lsp"
	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/home"
)

// runtime owns the LSP process lifecycle: creating, initializing, closing,
// killing and restarting the underlying client. It never touches
// diagnostics or open-file state directly; components that need the
// running process take a generation accessor instead, so a request issued
// while a restart is in flight always reaches the process it was
// dispatched from.
//
// mu is the lifecycle gate: it serializes Initialize, Close, Kill,
// Shutdown and Restart against each other and is held across the entire
// restart, including process close, spawn, Initialize and
// WaitForServerReady. Generation publication is the single step inside
// that gate (publishSwap), so no request or notification can ever observe
// a generation before it is fully initialized and ready.
type runtime struct {
	mu       sync.Mutex
	shutdown bool
	name     string
	debug    bool
	cwd      string
	config   config.LSPConfig
	resolver config.VariableResolver

	// closeFiles closes every tracked file as part of a graceful close or
	// restart. The orchestrator wires it up so the lifecycle never
	// depends on the file-sync component directly.
	closeFiles func(ctx context.Context, gen *clientGeneration)

	// setState reports state transitions to the façade.
	setState func(ServerState)

	// onDiagnosticsPublish is the hook the orchestrator wires up so the
	// lifecycle never depends on the diagnostics component directly.
	onDiagnosticsPublish func(gen *clientGeneration, params json.RawMessage)

	// afterGenerationPublish is a test seam: production code never sets
	// it, but client_test.go assigns it to observe exactly when a new
	// generation becomes current.
	afterGenerationPublish func()

	// genMu guards gen. Readers (requests, notifications, diagnostics
	// handlers) hold a generation value taken under this mutex; a
	// generation's context stays live until the next generation is
	// published or the client is shut down, so a long request that outlives
	// a restart keeps talking to its own process.
	genMu sync.Mutex
	gen   *clientGeneration
}

func newRuntime(name string, cfg config.LSPConfig, resolver config.VariableResolver, cwd string, debug bool) *runtime {
	return &runtime{
		name:     name,
		debug:    debug,
		cwd:      cwd,
		config:   cfg,
		resolver: resolver,
	}
}

// createGeneration builds an unpublished generation: a fresh process plus
// its long-lived context and cancellation.
func (r *runtime) createGeneration(ctx context.Context, cancel context.CancelFunc) (*clientGeneration, error) {
	rootURI := string(protocol.URIFromPath(r.cwd))

	command, err := r.resolver.ResolveValue(r.config.Command)
	if err != nil {
		return nil, fmt.Errorf("invalid lsp command: %w", err)
	}

	args, err := r.config.ResolvedArgs(r.resolver)
	if err != nil {
		return nil, fmt.Errorf("invalid lsp args: %w", err)
	}

	envs, err := r.config.ResolvedEnv(r.resolver)
	if err != nil {
		return nil, fmt.Errorf("invalid lsp env: %w", err)
	}

	clientConfig := powernap.ClientConfig{
		Command:     home.Long(command),
		Args:        args,
		RootURI:     rootURI,
		Environment: envs,
		Settings:    r.config.Options,
		InitOptions: r.config.InitOptions,
		WorkspaceFolders: []protocol.WorkspaceFolder{
			{
				URI:  rootURI,
				Name: filepath.Base(r.cwd),
			},
		},
	}

	client, err := powernap.NewClient(clientConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create lsp client: %w", err)
	}

	return &clientGeneration{client: client, ctx: ctx, cancel: cancel}, nil
}

// publishSwap makes a new generation visible to every observer as one
// atomic step: the runtime's current generation and the diagnostics
// store's active generation are swapped under the shared publication
// gate, so no request, file notification or diagnostics handler can ever
// observe the new runtime generation while the diagnostics store still
// treats the old one as active (and vice versa). Callers must hold r.mu.
//
// The gate is taken in the order r.mu → genMu → d.mu:
//
//   - r.mu is held by every lifecycle transition (Initialize, Close,
//     Kill, Shutdown, Restart), so generation publication cannot race
//     another publication;
//   - genMu is held for the entire swap, and every currentGeneration()
//     reader (requests, file sync, diagnostics handlers) also takes
//     genMu, so readers are blocked until both the runtime assignment
//     and the diagnostics activation are complete;
//   - d.mu is taken by the diagnostics swap inside that window.
//
// No component ever takes d.mu or genMu before r.mu in any other order,
// so the cycle is impossible.
func (r *runtime) publishSwap(gen *clientGeneration, diags *diagnosticsStore, oldGen *clientGeneration, state ServerState) {
	r.genMu.Lock()
	if diags != nil {
		diags.publishGeneration(oldGen, gen)
	}
	r.gen = gen
	if oldGen != nil && oldGen.cancel != nil {
		oldGen.cancel()
	}
	if r.afterGenerationPublish != nil {
		r.afterGenerationPublish()
	}
	r.reportState(state)
	r.genMu.Unlock()
}

// publishInitial installs the first generation through the same publication
// gate as a restart. New has not returned yet, but keeping this as the sole
// generation-assignment primitive preserves the invariant that runtime and
// diagnostics are always installed as a coherent pair.
func (r *runtime) publishInitial(gen *clientGeneration, diags *diagnosticsStore) {
	r.publishSwap(gen, diags, nil, StateStopped)
}

// reportState forwards a state transition to the façade hook; a nil hook
// (unit-test runtimes that are not wired to a Client) is a no-op.
func (r *runtime) reportState(state ServerState) {
	if r.setState != nil {
		r.setState(state)
	}
}

// currentGeneration returns the generation the caller should talk to.
func (r *runtime) currentGeneration() *clientGeneration {
	r.genMu.Lock()
	gen := r.gen
	r.genMu.Unlock()
	return gen
}

// initialize performs the LSP initialize handshake on the given
// generation. Callers must hold r.mu; the handler registration is part of
// the same critical section as the handshake, and happens before
// gen.client.Initialize() runs, because servers can send requests during
// the handshake itself (e.g. typescript-language-server issuing
// window/workDoneProgress/create while loading the project, before
// Initialize has returned) — registering afterward is too late, since the
// server treats an unhandled request as fatal.
func (r *runtime) initialize(ctx context.Context, gen *clientGeneration) (*protocol.InitializeResult, error) {
	r.registerHandlers(gen)

	if err := gen.client.Initialize(ctx, false); err != nil {
		return nil, fmt.Errorf("failed to initialize the lsp client: %w", err)
	}

	// Keep the complete server capability payload, including union providers.
	result := &protocol.InitializeResult{Capabilities: gen.client.GetCapabilities()}

	return result, nil
}

// waitForServerReady polls until the server on gen reports itself running.
// Callers arrange any bootstrap file synchronization before calling it and
// must hold r.mu.
func (r *runtime) waitForServerReady(ctx context.Context, gen *clientGeneration, publishReady bool) error {
	// Set initial state
	r.reportState(StateStarting)

	// Try to ping the server with a simple request
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	if r.debug {
		slog.Debug("Waiting for LSP server to be ready...")
	}

	for {
		select {
		case <-ctx.Done():
			r.reportState(StateError)
			return fmt.Errorf("timeout waiting for LSP server to be ready")
		case <-ticker.C:
			// Check if client is running
			if !gen.client.IsRunning() {
				if r.debug {
					slog.Debug("LSP server not ready yet", "server", r.name)
				}
				continue
			}

			// Initial startup publishes readiness directly. Restart keeps the
			// candidate unpublished and reports Ready only in publishSwap.
			if publishReady {
				r.reportState(StateReady)
			}
			if r.debug {
				slog.Debug("LSP server is ready")
			}
			return nil
		}
	}
}

// Kill force-kills the current process and marks that generation dead so
// the lifecycle never re-closes it through the graceful path on a later
// retry; Kill itself is idempotent in the underlying client.
func (r *runtime) Kill() {
	r.mu.Lock()
	defer r.mu.Unlock()
	gen := r.currentGeneration()
	// A runtime straight from newRuntime has no generation until
	// publishInitial installs one. New never hands out a *Client before
	// that, so production cannot reach this with a nil gen — but nothing
	// in the type enforces it, and Shutdown right below already guards
	// the same read. Guarding both keeps them from disagreeing.
	if gen == nil {
		return
	}
	gen.client.Kill()
	gen.markDead()
}

// beginShutdown closes the lifecycle admission gate before diagnostics are
// drained. In particular, a diagnostics callback cannot restart the client
// after another goroutine has begun terminal shutdown.
func (r *runtime) beginShutdown() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.shutdown {
		return
	}
	r.shutdown = true
	r.reportState(StateStopped)
}

// Shutdown retires and kills the current generation. beginShutdown must run
// first, allowing the façade to establish terminal lifecycle state before it
// waits for diagnostics callbacks to quiesce.
func (r *runtime) Shutdown() {
	r.beginShutdown()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.genMu.Lock()
	gen := r.gen
	if gen != nil {
		gen.markRetired()
		gen.cancel()
		gen.client.Kill()
		gen.markDead()
	}
	r.genMu.Unlock()
}

// restart closes the current generation and starts a fresh one with the
// same configuration. It returns the new generation on success.
//
// The old generation is closed but intentionally NOT swapped out until the
// new one is fully up: requests and file notifications resolve
// currentGeneration() at dispatch time, so during the restart they keep
// targeting the old process, and if the new process fails to initialize or
// become ready the old (dead) generation remains current — requests fail
// fast with a defined error, StateError is published, and a later retry
// closes it again and spawns a fresh candidate. A killed or cancelled
// candidate is never published as current.
//
// A dead generation is retired (its context canceled) in the same
// publication step that it stops being current: either the successful
// swap below, or the terminal shutdown. A dead generation that remains
// current after a failed restart therefore has no live context and no
// live process — nothing leaks — and the next retry only re-closes it
// through the idempotent Kill path, never through the graceful shutdown
// against an already-closed process.
func (r *runtime) restart(
	diags *diagnosticsStore,
	prepareSync func() func(ctx context.Context, gen *clientGeneration) (commit func(), err error),
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.shutdown {
		return errClientShutdown
	}

	// The open-files snapshot must be taken under r.mu, not before it:
	// snapshotting outside the gate lets two overlapping restarts
	// interleave so that a file opened between the snapshot and this
	// lock is silently dropped from the candidate's reopen set (it stays
	// in f.files, so IsFileOpen reports true, but the new generation
	// never got its didOpen).
	//
	// This only closes restart-vs-restart interleaving. openFile itself
	// is deliberately not gated by r.mu — it was never meant to block on
	// a restart in flight — so it can still call didOpen on oldGen after
	// this snapshot is taken and after the swap below publishes a new
	// generation, leaving the very same symptom (open per f.files, no
	// didOpen on the generation that matters). filesync.openFile closes
	// that half itself, by checking after its didOpen whether the
	// generation it used is still current and reopening on whichever one
	// is if not — see its comment for why the fix belongs there and not
	// here.
	prepareFiles := prepareSync()

	oldGen := r.currentGeneration()
	r.reportState(StateStopped)

	closeCtx, cancelClose := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelClose()

	if !oldGen.isUsable() {
		// A previous restart already closed this process (it stayed
		// current because that restart failed). Re-closing a closed
		// process would just time out and kill it for no reason; Kill is
		// idempotent and sufficient to make the state definite.
		oldGen.client.Kill()
	} else if err := r.closeProcessLocked(closeCtx, oldGen); err != nil {
		slog.Warn("Error closing client during restart", "name", r.name, "error", err)
	}

	// Retire the old generation now that its process is definitively
	// closed: its context is canceled under the publication gate, which
	// is also where the generation readers (requests, file sync) are
	// blocked, so no new work can start on the dead generation.
	r.genMu.Lock()
	if oldGen == r.gen {
		oldGen.markRetired()
		oldGen.cancel()
	}
	r.genMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	gen, err := r.createGeneration(ctx, cancel)
	if err != nil {
		cancel()
		return err
	}
	// The candidate is dead and was never published: cancel its context so
	// nothing holds the process open.
	defer func() {
		if gen != r.currentGeneration() {
			gen.cancel()
		}
	}()

	initCtx, cancelInit := context.WithTimeout(gen.ctx, 30*time.Second)
	defer cancelInit()

	if _, err := r.initialize(initCtx, gen); err != nil {
		return r.failCandidate(gen, err)
	}

	// Synchronize the unpublished candidate before it is declared ready.
	// prepareFiles keeps its changes in a candidate overlay until publication;
	// a failure rolls back candidate notifications while preserving the current
	// generation's shared user-file snapshot for retry.
	commitFiles, err := prepareFiles(initCtx, gen)
	if err != nil {
		return r.failCandidate(gen, err)
	}
	if err := r.waitForServerReady(initCtx, gen, false); err != nil {
		slog.Error("Server failed to become ready after restart", "name", r.name, "error", err)
		return r.failCandidate(gen, err)
	}

	// The candidate is fully up. Swap it in together with the diagnostics
	// store and the state as one atomic step (under the shared
	// publication gate), and retire the old generation: its context is
	// canceled in the same critical section as the swap, so it is dead
	// exactly when it stops being current, and can never outlive its
	// process or leak a live context.
	r.publishSwap(gen, diags, oldGen, StateReady)
	commitFiles()

	return nil
}

// failCandidate marks a restart candidate generation dead, kills its
// process, and reports StateError, returning err unchanged so call sites
// can `return r.failCandidate(gen, err)`. It is the shared failure arm for
// restart's three candidate-setup steps (initialize, prepareFiles,
// waitForServerReady): each fails the same way once the candidate itself
// is bad, differing only in whether they log first.
func (r *runtime) failCandidate(gen *clientGeneration, err error) error {
	gen.client.Kill()
	gen.markDead()
	r.reportState(StateError)
	return err
}

// close closes all tracked files (via closeFiles, if set) and performs the
// graceful LSP shutdown/exit sequence, falling back to Kill() if the
// server does not answer in time.
func (r *runtime) close(ctx context.Context, gen *clientGeneration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closeLocked(ctx, gen)
}

func (r *runtime) closeLocked(ctx context.Context, gen *clientGeneration) error {
	if r.closeFiles != nil {
		r.closeFiles(ctx, gen)
	}
	return r.closeProcessLocked(ctx, gen)
}

// closeProcessLocked stops a server without changing the user-file snapshot.
// Restart needs that snapshot intact until its candidate is published.
func (r *runtime) closeProcessLocked(ctx context.Context, gen *clientGeneration) error {
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
		// The process is definitively closed: mark the generation dead
		// so the context is retired by the shared publication step and a
		// later restart retry does not run the graceful shutdown against
		// it again (a second shutdown against a closed process is a
		// timeout-then-kill for no reason).
		gen.markDead()
		return err
	case <-closeCtx.Done():
		gen.client.Kill()
		gen.markDead()
		return closeCtx.Err()
	}
}

// registerHandlers registers the standard LSP notification and request
// handlers on the given generation.
func (r *runtime) registerHandlers(gen *clientGeneration) {
	gen.client.RegisterHandler("workspace/applyEdit", HandleApplyEdit(gen.client.GetOffsetEncoding))
	gen.client.RegisterHandler("workspace/configuration", HandleWorkspaceConfiguration)
	gen.client.RegisterHandler("client/registerCapability", HandleRegisterCapability)
	gen.client.RegisterHandler("window/workDoneProgress/create", HandleWorkDoneProgressCreate)
	gen.client.RegisterNotificationHandler("window/showMessage", func(ctx context.Context, method string, params json.RawMessage) {
		if r.debug {
			HandleServerMessage(ctx, method, params)
		}
	})
	gen.client.RegisterNotificationHandler("textDocument/publishDiagnostics", func(_ context.Context, _ string, params json.RawMessage) {
		if r.onDiagnosticsPublish != nil {
			r.onDiagnosticsPublish(gen, params)
		}
	})
}
