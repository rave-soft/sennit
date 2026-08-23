package app

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rave-soft/sennit/internal/event"
)

// coordinatorCloser is implemented by the production agent.Coordinator
// (*agent's unexported coordinator type) to bound its background
// readiness goroutines to the App's lifetime. See its use in Shutdown.
type coordinatorCloser interface {
	Close(ctx context.Context) error
}

const defaultShutdownTimeout = 5 * time.Second

// shutdownState values.
const (
	shutdownStateIdle = iota
	shutdownStateShuttingDown
	shutdownStateDone
)

// ErrAppShutdownBlocked is returned when a shutdown registration method is
// called after shutdown has already started.
var ErrAppShutdownBlocked = errors.New("app: shutdown already started, cannot register new hooks/cleanups")

// shutdownPhases owns App's teardown: the idempotent state machine, the
// five ordered hook/cleanup queues (AddPreCleanupHook/AddShutdownHook/
// AddCleanup/AddCriticalCleanup/AddFinalCleanup), and the phase sequence
// below that runs them. Embedded anonymously in App, so Shutdown and the
// Add* methods promote onto *App directly — App.Shutdown() is this type's
// Shutdown. Its app back-reference is set once by New/NewForTest, mirroring
// the resource pointer internal/thread/lifecycle.go keeps to the entity it
// drives; shutdownPhases itself still owns every queue, the state machine,
// and the ordering.
//
// # Ordering
//
// Shutdown runs six phases, each documented at its call site below, but in
// short: (0) latch the dispatcher's accept gate, before anything else can
// assume no new run will start; (1) stop pre-cleanup and shutdown hooks —
// subsystems that could otherwise *initiate* MCP/DB/agent work — before
// phases 4-5 tear those resources down; (2) cancel agent work and join the
// dispatcher, before phase 3 flushes messages a live turn might still be
// writing and before phases 4-5 touch MCP/DB a live turn might still use;
// (3) flush messages, while the DB phase 5 releases is still open; (4)
// close MCP, which can still touch the DB, before phase 5 releases it; (5)
// run critical cleanups (only if phase 1's hooks all succeeded) then
// release the main DB last, since every phase above needs it open; (6)
// parallel, independent teardown (AppExited, background shells, LSP,
// herdr), then final cleanups — only once those repository users actually
// stopped, since outliving them is a final cleanup's whole purpose.
type shutdownPhases struct {
	// app is set once, by New/NewForTest, before Shutdown can be called.
	app *App

	// mu guards state, and the five queues below, for their duration as
	// registration targets (AddCleanup et al.) and as the snapshot taken
	// at the top of Shutdown.
	shutdownMu sync.Mutex
	// shutdownState tracks lifecycle: 0=idle, 1=shuttingDown, 2=done.
	shutdownState int
	// shutdownDone is closed when Shutdown completes; concurrent callers
	// block on it so repeated/concurrent Shutdown calls wait for one
	// teardown and return together.
	shutdownDone    chan struct{}
	shutdownTimeout time.Duration

	preCleanupHooks      []func(context.Context) error // stop and join resources used by ordinary cleanup
	shutdownHooks        []func(context.Context) error // stop resources used by critical cleanup
	cleanupFuncs         []func(context.Context) error
	criticalCleanupFuncs []func(context.Context) error // only after all hooks finish
	finalCleanupFuncs    []func(context.Context) error // only after every repo user has stopped

	mcpInitCancel context.CancelFunc
	mcpInitWG     sync.WaitGroup
	mcpClose      func(context.Context) error
	mainDBRelease func(context.Context) error
	// stopBackgroundShells and stopLSP are test seams for the final-resource
	// ordering; production leaves them nil and uses the concrete managers.
	stopBackgroundShells func(context.Context) error
	stopLSP              func(context.Context) error
	// agentWorkStopped is a test seam for shutdown dependency ordering.
	// Production leaves it nil and queries AgentCoordinator after CancelAll.
	agentWorkStopped func() bool
}

// runShutdownCallback supplies a bounded context and never synchronously
// waits past its deadline, even if the callback ignores cancellation.
func (p *shutdownPhases) runShutdownCallback(fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), p.shutdownTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// addHook is the shared body behind every Add* registration method below:
// append fn to *queue unless shutdown has already started, in which case
// it returns ErrAppShutdownBlocked and leaves fn unregistered.
func (p *shutdownPhases) addHook(queue *[]func(context.Context) error, fn func(context.Context) error) error {
	p.shutdownMu.Lock()
	defer p.shutdownMu.Unlock()
	if p.shutdownState >= shutdownStateShuttingDown {
		return ErrAppShutdownBlocked
	}
	*queue = append(*queue, fn)
	return nil
}

// AddCleanup registers fn to run, alongside the built-in cleanup tasks,
// when Shutdown is called. Used by callers that attach extra resources to
// an App post-construction — e.g. the thread manager's own database
// connection — that need to be released on the same schedule.
func (p *shutdownPhases) AddCleanup(fn func(context.Context) error) error {
	return p.addHook(&p.cleanupFuncs, fn)
}

// AddPreCleanupHook registers a stop-and-join operation that must finish
// before ordinary DB-dependent resources, such as MCP, can close. It is used
// by the external watchers, which can otherwise initiate MCP work during
// teardown.
func (p *shutdownPhases) AddPreCleanupHook(fn func(context.Context) error) error {
	return p.addHook(&p.preCleanupHooks, fn)
}

// AddCriticalCleanup registers cleanup that depends on every shutdown hook
// completing. It is intended for a thread database release: if its manager
// did not stop, releasing that database could race a live manager. Critical
// cleanup is deliberately separate from the main App database, which is
// released in the final shutdown phase regardless of hook failures.
func (p *shutdownPhases) AddCriticalCleanup(fn func(context.Context) error) error {
	return p.addHook(&p.criticalCleanupFuncs, fn)
}

// AddFinalCleanup registers fn to run only after background shells and LSP
// clients have stopped successfully. It is for resources, such as a workspace
// lock, that must outlive every repository user during shutdown. If a user
// times out or fails to stop, final cleanups are retained rather than released
// unsafely.
func (p *shutdownPhases) AddFinalCleanup(fn func(context.Context) error) error {
	return p.addHook(&p.finalCleanupFuncs, fn)
}

// AddShutdownHook registers fn to run before any cleanup functions when
// Shutdown is called. Use this for subsystems (e.g. thread manager) that
// must block new operations as soon as shutdown begins, before other
// cleanup (DB connections, LSP, MCP) tears down their dependencies.
func (p *shutdownPhases) AddShutdownHook(fn func(context.Context) error) error {
	return p.addHook(&p.shutdownHooks, fn)
}

// Shutdown performs a graceful, race-safe shutdown of the application.
// It is idempotent: repeated or concurrent callers wait for the same
// teardown and return together. See the shutdownPhases doc comment above
// for the six-phase ordering this method implements.
func (p *shutdownPhases) Shutdown() {
	p.shutdownMu.Lock()
	// If already done, return immediately.
	if p.shutdownState == shutdownStateDone {
		p.shutdownMu.Unlock()
		return
	}
	// If already shutting down, wait for completion.
	if p.shutdownState == shutdownStateShuttingDown {
		done := p.shutdownDone
		p.shutdownMu.Unlock()
		<-done
		return
	}
	// Enter shutting down state, create done channel.
	p.shutdownState = shutdownStateShuttingDown
	p.shutdownDone = make(chan struct{})

	// Capture phased hooks and cleanups under the lock so no new ones arrive
	// mid-shutdown.
	preCleanupHooks := make([]func(context.Context) error, len(p.preCleanupHooks))
	copy(preCleanupHooks, p.preCleanupHooks)
	p.preCleanupHooks = nil
	hooks := make([]func(context.Context) error, len(p.shutdownHooks))
	copy(hooks, p.shutdownHooks)
	p.shutdownHooks = nil // prevent concurrent AddShutdownHook from appending
	cleanups := make([]func(context.Context) error, len(p.cleanupFuncs))
	copy(cleanups, p.cleanupFuncs)
	p.cleanupFuncs = nil // prevent concurrent AddCleanup from appending
	criticalCleanups := make([]func(context.Context) error, len(p.criticalCleanupFuncs))
	copy(criticalCleanups, p.criticalCleanupFuncs)
	p.criticalCleanupFuncs = nil
	finalCleanups := make([]func(context.Context) error, len(p.finalCleanupFuncs))
	copy(finalCleanups, p.finalCleanupFuncs)
	p.finalCleanupFuncs = nil
	p.shutdownMu.Unlock()

	app := p.app

	// 0. Latch the dispatcher's accept gate before any teardown phase
	// runs: a Send that lands after this point must be refused rather
	// than racing the teardown below. Nil only for a hand-built App
	// that skipped both New and NewForTest.
	if app.agentDispatcher != nil {
		app.agentDispatcher.MarkClosing()
	}

	start := time.Now()
	defer func() { slog.Debug("Shutdown took " + time.Since(start).String()) }()
	defer close(p.shutdownDone)

	// 1. Stop and join watchers before MCP closes. A failed watcher hook means
	// it may still initiate MCP work, so MCP cannot safely be closed.
	preCleanupHooksSucceeded := true
	for _, hook := range preCleanupHooks {
		if hook != nil {
			if err := p.runShutdownCallback(hook); err != nil {
				preCleanupHooksSucceeded = false
				slog.Error("Failed to run pre-cleanup hook", "error", err)
			}
		}
	}

	// Stop dependent subsystems before cancelling or releasing shared resources.
	// A hook that does not finish is treated as a failure: its dependent
	// cleanup (notably the thread DB release) must not run while it can still
	// access that resource.
	hooksSucceeded := true
	for _, hook := range hooks {
		if hook != nil {
			if err := p.runShutdownCallback(hook); err != nil {
				hooksSucceeded = false
				slog.Error("Failed to run shutdown hook", "error", err)
			}
		}
	}

	// 2. Stop agent-owned work before closing MCP or database resources it may
	// still use. CancelAll bounds its own wait; retain dependencies when active
	// work or coordinator readiness work does not stop.
	agentWorkStopped := true
	if p.agentWorkStopped != nil {
		agentWorkStopped = p.agentWorkStopped()
		if !agentWorkStopped {
			slog.Error("Agent work did not stop before shutdown deadline")
		}
	} else if app.AgentCoordinator != nil {
		app.AgentCoordinator.CancelAll()
		if app.AgentCoordinator.IsBusy() {
			agentWorkStopped = false
			slog.Error("Agent work did not stop before shutdown deadline")
		}
		if closer, ok := app.AgentCoordinator.(coordinatorCloser); ok {
			if err := p.runShutdownCallback(closer.Close); err != nil {
				agentWorkStopped = false
				slog.Error("Failed to close agent coordinator readiness work", "error", err)
			}
		}
	}

	// Join every dispatched run before anything below touches shared
	// resources. CancelAll above already poisons/cancels each run
	// through the coordinator's own accept/cancel protocol regardless of
	// which branch above ran it (including the agentWorkStopped test
	// seam), so Wait is unconditional here rather than nested in the
	// CancelAll branch. Bounded by shutdownTimeout like every other
	// phase-2 wait: an unbounded wait would let one hung run stall
	// Shutdown forever, whereas a bounded one degrades to the same
	// "retain dependencies and log" contract agentWorkStopped already
	// has, keeping MCP and the DB from closing under a run that is
	// still using them.
	if app.agentDispatcher != nil {
		if err := p.runShutdownCallback(func(context.Context) error {
			app.agentDispatcher.Wait()
			return nil
		}); err != nil {
			agentWorkStopped = false
			slog.Error("Agent dispatcher runs did not join before shutdown deadline", "error", err)
		}
	}

	// Initial MCP setup is independent of watcher-triggered reinitialization.
	// Cancel and join it before Registry.Close so it cannot publish a session
	// after the registry has closed.
	mcpInitStopped := true
	if p.mcpInitCancel != nil {
		if err := p.runShutdownCallback(func(context.Context) error {
			p.mcpInitCancel()
			p.mcpInitWG.Wait()
			return nil
		}); err != nil {
			mcpInitStopped = false
			slog.Error("Failed to stop MCP initialization", "error", err)
		}
	}

	// 3. Flush any debounced message updates before the DB-close cleanup
	// runs. message.Service buffers streaming deltas and we must land
	// them while the connection is still open.
	//
	// Close rather than FlushAll: a drain alone leaves the service free
	// to arm another debounce timer, and one that fires after this point
	// writes to a database that is closing underneath it. Close stops the
	// timers first and drains second, so nothing is left to fire.
	if app.messages != nil {
		ctx, cancel := context.WithTimeout(context.Background(), p.shutdownTimeout)
		if err := app.messages.Close(ctx); err != nil {
			slog.Error("Failed to flush pending message updates on shutdown", "error", err)
		}
		cancel()
	}

	dependenciesStopped := preCleanupHooksSucceeded && agentWorkStopped && mcpInitStopped

	// 4. MCP can use the App database during close, so close it after all
	// stop-and-join hooks and before the database's final release.
	if dependenciesStopped && p.mcpClose != nil {
		if err := p.runShutdownCallback(p.mcpClose); err != nil {
			dependenciesStopped = false
			slog.Error("Failed to close MCP on shutdown", "error", err)
		}
	}

	// General cleanups do not determine the ordering of production resources.
	for _, cleanup := range cleanups {
		if cleanup != nil {
			if err := p.runShutdownCallback(cleanup); err != nil {
				slog.Error("Failed to cleanup app properly on shutdown", "error", err)
			}
		}
	}

	// 5. Never release a thread DB used by a hook that failed to finish.
	if hooksSucceeded {
		for _, cleanup := range criticalCleanups {
			if cleanup != nil {
				if err := p.runShutdownCallback(cleanup); err != nil {
					slog.Error("Failed to run critical app cleanup on shutdown", "error", err)
				}
			}
		}
	}
	if dependenciesStopped && p.mainDBRelease != nil {
		if err := p.runShutdownCallback(p.mainDBRelease); err != nil {
			slog.Error("Failed to release main database on shutdown", "error", err)
		}
	}

	// 6. Parallel independent teardown.
	var wg sync.WaitGroup
	var repoUsersStopped atomic.Bool
	repoUsersStopped.Store(true)

	wg.Go(func() {
		event.AppExited()
	})

	wg.Go(func() {
		stop := p.stopBackgroundShells
		if stop == nil && app.BackgroundShells != nil {
			stop = app.BackgroundShells.Shutdown
		}
		if stop != nil {
			if err := p.runShutdownCallback(stop); err != nil {
				repoUsersStopped.Store(false)
				slog.Error("Failed to stop background shells", "error", err)
			}
		}
	})

	if app.herdrClient != nil {
		app.herdrClient.Close()
	}
	if app.herdrCancel != nil {
		app.herdrCancel()
	}

	wg.Go(func() {
		stop := p.stopLSP
		if stop == nil && app.LSPManager != nil {
			stop = func(ctx context.Context) error {
				app.LSPManager.KillAll(ctx)
				return nil
			}
		}
		if stop != nil {
			if err := p.runShutdownCallback(stop); err != nil {
				repoUsersStopped.Store(false)
				slog.Error("Failed to stop LSP clients", "error", err)
			}
		}
	})

	wg.Wait()
	if hooksSucceeded && dependenciesStopped && repoUsersStopped.Load() {
		for _, cleanup := range finalCleanups {
			if cleanup != nil {
				if err := p.runShutdownCallback(cleanup); err != nil {
					slog.Error("Failed to run final app cleanup on shutdown", "error", err)
				}
			}
		}
	} else if len(finalCleanups) > 0 {
		slog.Error("Retaining final resources because repository-dependent subsystems did not stop", "hooksSucceeded", hooksSucceeded, "dependenciesStopped", dependenciesStopped, "repoUsersStopped", repoUsersStopped.Load())
	}

	p.shutdownMu.Lock()
	p.shutdownState = shutdownStateDone
	p.shutdownMu.Unlock()
}
