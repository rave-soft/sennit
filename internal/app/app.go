// Package app wires together services, coordinates agents, and manages
// application lifecycle.
package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/clipboard"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/herdr"
	"github.com/rave-soft/sennit/internal/log"
	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/stats"
	"github.com/rave-soft/sennit/internal/stats/gather"
)

// App is the top-level wiring for one workspace: the composition root New
// assembles. It is a thin facade over three groupings, each embedded
// anonymously so their fields/methods promote onto *App unchanged: the
// domain services and managers in appServices (services.go), the event
// fan-in in appEvents (events.go), and the teardown sequence in
// shutdownPhases (shutdown.go). globalCtx and agentDispatcher stay
// directly on App since neither belongs to exactly one of those three.
type App struct {
	// The three groupings are embedded by value, not by pointer, so a
	// zero App is still usable: a promoted field on a nil pointer
	// embedding panics, and external tests legitimately build a bare
	// &app.App{} and fill in only what they need.
	appServices
	appEvents
	shutdownPhases

	// global context every other App-owned context derives from.
	globalCtx context.Context

	// agentDispatcher accepts and dispatches fire-and-forget agent runs
	// on this App's behalf, bound to globalCtx. It resolves
	// AgentCoordinator lazily (see AgentDispatcher's coordinator field
	// doc) so it is safe to build before InitCoderAgent runs and
	// survives a later coordinator reinitialization. Built
	// unconditionally in New/NewForTest, so it is never nil: Shutdown
	// still nil-checks it defensively, for hand-built test doubles that
	// bypass both constructors.
	agentDispatcher *AgentDispatcher

	// workspaceLockEnforced records whether this App's bootstrap holds a
	// workspace lock that actually excludes a second sennit. Read by work
	// that is only safe under mutual exclusion - see
	// WorkspaceLockEnforced.
	workspaceLockEnforced bool
}

// WorkspaceLockEnforced reports whether a second sennit is excluded from
// this workspace. False means either that no lock was requested or that
// SENNIT_SKIP_DATADIR_LOCK turned acquisition into a no-op, and anything
// whose correctness rests on "no other process is running turns against
// these sessions" has to skip. Finalizing interrupted turns is the case
// this exists for: it stamps a canceled finish and error tool results onto
// every unfinished assistant message, which repairs a crashed run and
// corrupts a live one.
func (app *App) WorkspaceLockEnforced() bool {
	return app != nil && app.workspaceLockEnforced
}

// New initializes a new application instance. skillsMgr carries the
// per-workspace skill discovery results computed by the caller; the
// caller is responsible for constructing it (typically via
// skills.NewManager + skills.DiscoverFromConfig).
type Option func(*appOptions)

type appOptions struct {
	herdrClient func() *herdr.Client
}

func WithHerdrClient(client func() *herdr.Client) Option {
	return func(options *appOptions) {
		options.herdrClient = client
	}
}

func New(ctx context.Context, conn *sql.DB, store *config.ConfigStore, skillsMgr *skills.Manager, options ...Option) (*App, error) {
	appOpts := appOptions{herdrClient: func() *herdr.Client { return nil }}
	for _, option := range options {
		option(&appOpts)
	}
	q := db.New(conn)
	cfg := store.Config()

	app := &App{
		appServices: *newAppServices(q, conn, store, skillsMgr),
		appEvents: appEvents{
			events:             pubsub.NewBroker[any](),
			serviceEventsWG:    &sync.WaitGroup{},
			tuiWG:              &sync.WaitGroup{},
			agentNotifications: pubsub.NewBroker[notify.Notification](),
			runCompletions:     pubsub.NewBroker[notify.RunComplete](),
		},
		shutdownPhases: shutdownPhases{
			shutdownTimeout: defaultShutdownTimeout,
		},

		globalCtx: ctx,
	}
	app.app = app

	app.setupEvents()

	// Initialize clipboard support. This is best-effort; if it fails
	// (e.g., headless environment), clipboard operations will return nil.
	if err := clipboard.Init(); err != nil {
		slog.Warn("Clipboard initialization failed", "error", err)
	}

	// Arm initialization synchronously before launching it so WaitForInit
	// blocks for the in-flight init instead of racing the goroutine and
	// returning before any MCP tools register.
	app.MCP.ArmInit()
	mcpInitCtx, mcpInitCancel := context.WithCancel(ctx)
	app.mcpInitCancel = mcpInitCancel
	app.mcpInitWG.Go(func() { app.MCP.Initialize(mcpInitCtx, app.Permissions(), store) })

	// Start herdr integration when running inside a herdr pane. Bound to a
	// context derived from ctx (not ctx itself), stopped via app.herdrCancel
	// (see its doc) rather than left to outlive ctx unconditionally.
	var herdrCtx context.Context
	herdrCtx, app.herdrCancel = context.WithCancel(ctx)
	app.herdrClient = appOpts.herdrClient()
	herdr.BridgeLocal(herdrCtx, app.herdrClient, herdr.BridgeSources{
		PermRequests:      app.Permissions(),
		PermNotifications: app.Permissions(),
		RunCompletions:    app.runCompletions,
		Messages:          app.Messages(),
	})

	// Keep production resources in explicit dependency phases rather than
	// relying on their registration order among general cleanups. The shared
	// DB is always released last.
	dbDir := config.GlobalDBDir()
	app.mcpClose = func(ctx context.Context) error { return app.MCP.Close(ctx) }
	app.mainDBRelease = func(context.Context) error { return db.Release(dbDir) }

	// Hot-reload config and skills on external edits (see
	// startExternalChangeWatchers' doc). Started regardless of
	// cfg.IsConfigured(): an unconfigured project's
	// config or an empty skills dir can still change externally.
	app.startExternalChangeWatchers(ctx)

	// Built unconditionally, before the IsConfigured early return below:
	// an unconfigured project still needs a non-nil dispatcher (its
	// coordinator getter yields nil, so Send answers
	// ErrCoordinatorNotInitialized instead of every caller having to
	// nil-check app.AgentDispatcher() first). Bound to globalCtx, not a
	// context this constructor derives and could cancel itself — the
	// dispatcher's lifetime tracks its owning App rather than any single
	// request.
	app.agentDispatcher = NewAgentDispatcher(app.globalCtx, func() AcceptedRunner { return app.Coordinator() }, app.agentNotifications, app.runCompletions)

	// Set up callback for LSP state updates.
	app.LSPManager.SetCallback(func(name string, client *lsp.Client) {
		if client == nil {
			app.lsp.updateLSPState(name, lsp.StateUnstarted, nil, nil)
			return
		}
		client.SetDiagnosticsCallback(app.lsp.updateLSPDiagnostics)
		app.lsp.updateLSPState(name, client.GetServerState(), nil, client)
	})

	// TrackConfigured must run after SetCallback so the callback is already
	// installed when configured-but-not-yet-started LSPs are announced.
	go app.LSPManager.TrackConfigured(ctx)

	// Keep the application available until a provider is configured:
	// configuration and skill watchers can still observe a later setup, while
	// dispatch reports that no coordinator has been initialized.
	if !cfg.IsConfigured() {
		slog.Warn("No runtime provider configured")
		return app, nil
	}
	if err := app.InitCoderAgent(ctx); err != nil {
		// Roll back everything started above before failing New: MCP
		// initialization, the config/skills watchers, the setupEvents
		// fan-in, and the herdr bridge are all live goroutines at this
		// point, and the caller only gets an error back, not an *App it
		// could Shutdown itself. app.Shutdown() already knows how to tear
		// all of that down (the watchers registered their own
		// pre-cleanup hook, mcpClose is already wired, and it cancels
		// app.herdrCancel itself), so reuse it instead of re-deriving the
		// same teardown here.
		//
		// mainDBRelease is deliberately cleared first: the main DB
		// connection is owned by New's caller (see Bootstrap's own
		// dbConnected release), not by this constructor, and
		// app.Shutdown() would otherwise release it out from under that
		// caller.
		app.mainDBRelease = nil
		app.Shutdown()
		return nil, fmt.Errorf("failed to initialize coder agent: %w", err)
	}

	return app, nil
}

// AgentDispatcher returns this App's fire-and-forget agent-run
// dispatcher. It is always non-nil for an App built by New or
// NewForTest. internal/workspace uses it to send prompts: dispatch
// and return, without waiting for the LLM turn to complete.
func (app *App) AgentDispatcher() *AgentDispatcher {
	return app.agentDispatcher
}

// Subscribe streams application events to send until ctx (app.globalCtx
// scoped internally) is torn down. It is decoupled from bubbletea's
// concrete *tea.Program so this core package doesn't need to import
// it; onPanic is invoked instead of a hardcoded program.Quit() if the
// delivery loop panics. Callers that feed a *tea.Program (see
// appws.AppWorkspace.Subscribe) pass program.Send and program.Quit.
func (app *App) Subscribe(send func(any), onPanic func()) {
	defer log.RecoverPanic("app.Subscribe", func() {
		slog.Info("TUI subscription panic: attempting graceful shutdown")
		onPanic()
	})

	app.tuiWG.Add(1)
	tuiCtx, tuiCancel := context.WithCancel(app.globalCtx)
	if err := app.AddCleanup(func(context.Context) error {
		slog.Debug("Cancelling TUI message handler")
		tuiCancel()
		app.tuiWG.Wait()
		return nil
	}); err != nil {
		tuiCancel()
	}
	defer app.tuiWG.Done()

	events := app.events.Subscribe(tuiCtx)
	for {
		select {
		case <-tuiCtx.Done():
			slog.Debug("TUI message handler shutting down")
			return
		case ev, ok := <-events:
			if !ok {
				slog.Debug("TUI message channel closed")
				return
			}
			send(ev.Payload)
		}
	}
}

// Stats aggregates recorded usage for the requested scope. It is the only
// read path that reaches the generated queries directly rather than a
// service (see App.queries): the breakdown spans sessions, messages, and
// delegations at once, and no single service owns that join.
//
// The heavy lifting lives in internal/stats, shared with `sennit stat`,
// so the TUI screen and the command can never report different numbers
// for the same data.
func (app *App) Stats(ctx context.Context, req stats.Request) (stats.Snapshot, error) {
	if app.queries == nil {
		return stats.Snapshot{}, errors.New("app: stats unavailable: no database queries wired")
	}
	return gather.Gather(ctx, app.queries, req)
}
