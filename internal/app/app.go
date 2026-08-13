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
	"time"

	"github.com/rave-soft/braid/internal/agent"
	"github.com/rave-soft/braid/internal/agent/notify"
	"github.com/rave-soft/braid/internal/agent/tools"
	"github.com/rave-soft/braid/internal/agent/tools/mcp"
	"github.com/rave-soft/braid/internal/clipboard"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/db"
	"github.com/rave-soft/braid/internal/event"
	"github.com/rave-soft/braid/internal/filetracker"
	"github.com/rave-soft/braid/internal/herdr"
	"github.com/rave-soft/braid/internal/history"
	"github.com/rave-soft/braid/internal/log"
	"github.com/rave-soft/braid/internal/lsp"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/permission"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/question"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/shell"
	"github.com/rave-soft/braid/internal/skills"
)

// UpdateAvailableMsg is sent when a new version is available.
type UpdateAvailableMsg struct {
	CurrentVersion string
	LatestVersion  string
	IsDevelopment  bool
}

// coordinatorCloser is implemented by the production agent.Coordinator
// (*agent's unexported coordinator type) to bound its background
// readiness goroutines to the App's lifetime. See its use in Shutdown.
type coordinatorCloser interface {
	Close(ctx context.Context) error
}

type App struct {
	Sessions    session.Service
	Messages    message.Service
	History     history.Service
	Permissions permission.Service
	Questions   question.Service
	FileTracker filetracker.Service

	AgentCoordinator agent.Coordinator

	LSPManager *lsp.Manager

	// lsp holds this workspace's own LSP client states and event broker;
	// see the lspEvents doc in lsp_events.go for why it isn't shared.
	lsp *lspEvents

	// MCP is this workspace's own MCP registry: sessions, per-server
	// state, auth handlers, and the event broker all live here rather
	// than on mcp's process-wide defaultRegistry, so two App instances in
	// one process (multi-client backend mode) don't clobber each other's
	// MCP servers. See ARCHITECTURE_REVIEW.md section 3.1.
	MCP              *mcp.Registry
	BackgroundShells *shell.BackgroundShellManager

	Skills *skills.Manager

	// Threads is the thread manager owning this workspace's parallel
	// agent work streams, wired in post-bootstrap by the caller (see
	// internal/cmd/root.go and internal/backend/backend.go) via
	// SetThreads. Declared as the tool-facing interface, not
	// *thread.Manager, because internal/thread imports this package —
	// see internal/agent/tools/thread_manager.go for the seam. Nil for
	// workspaces that don't own a thread manager: non-git workspaces and
	// thread workspaces themselves (nesting is not supported).
	Threads tools.ThreadManager

	// threadManager holds the same thread manager as Threads, but typed
	// any instead of tools.ThreadManager: internal/workspace and
	// internal/server (added in later steps) need the concrete
	// *thread.Manager (Subscribe, full-arg Merge/Remove, etc.), which is
	// richer than the tools.ThreadManager seam built for the agent-tool
	// wiring. app cannot reference *thread.Manager by name because
	// internal/thread imports internal/app (see the Threads doc above),
	// so this is deliberately untyped; callers type-assert it back to
	// *thread.Manager. Set via SetThreadManager, independent of
	// SetThreads/Threads.
	threadManager any

	// lastConfigBypass is the permissions.bypass value from config as of
	// the last time it was applied to Permissions.SetSkipRequests —
	// either at construction or via a hot-reload. It is compared against
	// the newly reloaded config's value so a reload only touches the
	// live skip state when permissions.bypass itself actually changed;
	// otherwise a user's manual ctrl+y / /yolo toggle (a session-only
	// override on the same service) would get silently overwritten by
	// an unrelated config reload (e.g. a provider edit) that happens to
	// run afterward. Only ever read and written from the config-reload
	// path (New and the OnExternalChange callback in watch.go), which is
	// single-threaded by construction (see ConfigStore.OnExternalChange's
	// doc comment), so no lock is needed.
	lastConfigBypass bool

	config *config.ConfigStore

	serviceEventsWG *sync.WaitGroup
	eventsCtx       context.Context
	// events is typed any rather than bubbletea's tea.Msg: the app
	// package is core (no UI dependency), and any is what tea.Msg
	// aliases down to anyway. Consumers that need a tea.Msg (the TUI)
	// convert at the boundary; see workspace.AppWorkspace.Subscribe.
	events *pubsub.Broker[any]
	tuiWG  *sync.WaitGroup

	// global context and cleanup functions
	globalCtx            context.Context
	preCleanupHooks      []func(context.Context) error // stop and join resources used by ordinary cleanup
	shutdownHooks        []func(context.Context) error // stop resources used by critical cleanup
	cleanupFuncs         []func(context.Context) error
	criticalCleanupFuncs []func(context.Context) error // only after all hooks finish
	mcpInitCancel        context.CancelFunc
	mcpInitWG            sync.WaitGroup
	mcpClose             func(context.Context) error
	mainDBRelease        func(context.Context) error
	agentNotifications   *pubsub.Broker[notify.Notification]
	// runCompletions is the authoritative per-run completion signal,
	// emitted once per top-level agent turn after all message
	// updates have been flushed. Bridged into app.events so SSE
	// subscribers (notably `braid run` in client/server mode) can
	// drive their exit on a deterministic, payload-bearing event
	// instead of guessing from message finish parts.
	runCompletions *pubsub.Broker[notify.RunComplete]

	// herdrClient reports agent state to herdr when running inside
	// a herdr-managed pane. Nil when not in a herdr environment.
	herdrClient *herdr.Client

	// shutdown lifecycle: ensures Shutdown is idempotent and race-safe.
	// mu guards shutdownState, shutdownHooks, and cleanupFuncs.
	shutdownMu sync.Mutex
	// shutdownState tracks lifecycle: 0=idle, 1=shuttingDown, 2=done.
	shutdownState int
	// shutdownDone is closed when Shutdown completes; concurrent callers
	// block on it so repeated/concurrent Shutdown calls wait for one
	// teardown and return together.
	shutdownDone    chan struct{}
	shutdownTimeout time.Duration
}

// shutdownState values.
const defaultShutdownTimeout = 5 * time.Second

// runShutdownCallback supplies a bounded context and never synchronously
// waits past its deadline, even if the callback ignores cancellation.
func (app *App) runShutdownCallback(fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), app.shutdownTimeout)
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

const (
	shutdownStateIdle = iota
	shutdownStateShuttingDown
	shutdownStateDone
)

// ErrAppShutdownBlocked is returned when a shutdown registration method is
// called after shutdown has already started.
var ErrAppShutdownBlocked = errors.New("app: shutdown already started, cannot register new hooks/cleanups")

// New initializes a new application instance. skillsMgr carries the
// per-workspace skill discovery results computed by the caller; the
// caller is responsible for constructing it (typically via
// skills.NewManager + skills.DiscoverFromConfig).
func New(ctx context.Context, conn *sql.DB, store *config.ConfigStore, skillsMgr *skills.Manager) (*App, error) {
	q := db.New(conn)
	sessions := session.NewService(q, conn, store.WorkingDir())
	messages := message.NewService(q)
	files := history.NewService(q, conn)
	cfg := store.Config()
	skipPermissionsRequests := store.Overrides().SkipPermissionRequests
	var allowedTools []string
	var configBypass bool
	if cfg.Permissions != nil {
		allowedTools = cfg.Permissions.AllowedTools
		configBypass = cfg.Permissions.Bypass
		skipPermissionsRequests = skipPermissionsRequests || configBypass
	}

	app := &App{
		Sessions:         sessions,
		Messages:         messages,
		History:          files,
		Permissions:      permission.NewPermissionService(store.WorkingDir(), skipPermissionsRequests, allowedTools),
		Questions:        question.NewService(),
		FileTracker:      filetracker.NewService(q, store.WorkingDir()),
		LSPManager:       lsp.NewManager(store),
		lsp:              newLSPEvents(),
		MCP:              mcp.NewRegistry(),
		BackgroundShells: shell.NewBackgroundShellManager(),
		Skills:           skillsMgr,

		globalCtx: ctx,

		lastConfigBypass: configBypass,
		config:           store,

		events:             pubsub.NewBroker[any](),
		serviceEventsWG:    &sync.WaitGroup{},
		tuiWG:              &sync.WaitGroup{},
		agentNotifications: pubsub.NewBroker[notify.Notification](),
		runCompletions:     pubsub.NewBroker[notify.RunComplete](),
		shutdownTimeout:    defaultShutdownTimeout,
	}

	app.setupEvents()

	// Initialize clipboard support. This is best-effort; if it fails
	// (e.g., headless environment), clipboard operations will return nil.
	if err := clipboard.Init(); err != nil {
		slog.Warn("Clipboard initialization failed", "error", err)
	}

	// Check for updates in the background.
	// Upstream started a background update check against GitHub here. Braid
	// makes no outbound calls of its own.

	// Arm initialization synchronously before launching it so WaitForInit
	// blocks for the in-flight init instead of racing the goroutine and
	// returning before any MCP tools register.
	app.MCP.ArmInit()
	mcpInitCtx, mcpInitCancel := context.WithCancel(ctx)
	app.mcpInitCancel = mcpInitCancel
	app.mcpInitWG.Go(func() { app.MCP.Initialize(mcpInitCtx, app.Permissions, store) })

	// Start herdr integration when running inside a herdr pane.
	app.herdrClient = herdr.Init()
	herdr.BridgeLocal(ctx, app.herdrClient, herdr.BridgeSources{
		PermRequests:      app.Permissions,
		PermNotifications: app.Permissions,
		RunCompletions:    app.runCompletions,
		Messages:          app.Messages,
	})

	// Keep production resources in explicit dependency phases rather than
	// relying on their registration order among general cleanups. The shared
	// DB is always released last.
	dbDir := config.GlobalDBDir()
	app.mcpClose = func(ctx context.Context) error { return app.MCP.Close(ctx) }
	app.mainDBRelease = func(context.Context) error { return db.Release(dbDir) }

	// Hot-reload config and skills on external edits, in both local mode
	// and client/server mode (see startExternalChangeWatchers' doc).
	// Started regardless of cfg.IsConfigured(): an unconfigured project's
	// config or an empty skills dir can still change externally.
	app.startExternalChangeWatchers(ctx)

	// TODO: remove the concept of agent config, most likely.
	if !cfg.IsConfigured() {
		slog.Warn("No agent configuration found")
		return app, nil
	}
	if err := app.InitCoderAgent(ctx); err != nil {
		return nil, fmt.Errorf("failed to initialize coder agent: %w", err)
	}

	// Set up callback for LSP state updates.
	app.LSPManager.SetCallback(func(name string, client *lsp.Client) {
		if client == nil {
			app.lsp.updateLSPState(name, lsp.StateUnstarted, nil, nil, 0)
			return
		}
		client.SetDiagnosticsCallback(app.lsp.updateLSPDiagnostics)
		app.lsp.updateLSPState(name, client.GetServerState(), nil, client, 0)
	})

	// TrackConfigured must run after SetCallback so the callback is already
	// installed when configured-but-not-yet-started LSPs are announced.
	go app.LSPManager.TrackConfigured(ctx)

	return app, nil
}

// GetLSPStates returns the current state of this workspace's LSP clients.
func (app *App) GetLSPStates() map[string]LSPClientInfo {
	return app.lsp.GetLSPStates()
}

// GetLSPState returns the state of one of this workspace's LSP clients.
func (app *App) GetLSPState(name string) (LSPClientInfo, bool) {
	return app.lsp.GetLSPState(name)
}

// SubscribeLSPEvents returns a channel for this workspace's LSP events.
func (app *App) SubscribeLSPEvents(ctx context.Context) <-chan pubsub.Event[LSPEvent] {
	return app.lsp.SubscribeLSPEvents(ctx)
}

// Config returns the pure-data configuration.
func (app *App) Config() *config.Config {
	return app.config.Config()
}

// Store returns the config store.
func (app *App) Store() *config.ConfigStore {
	return app.config
}

// SetThreads wires the thread manager owning this workspace's parallel
// agent work streams, forwarding it to the coder agent so the thread_*
// tools become available. Safe to call with nil to clear it. The caller
// (see internal/cmd/root.go and internal/backend/backend.go) is
// responsible for deciding whether this workspace should own one at all.
func (app *App) SetThreads(threads tools.ThreadManager) {
	app.Threads = threads
	if app.AgentCoordinator != nil {
		app.AgentCoordinator.SetThreads(threads)
	}
}

// SetThreadManager wires the concrete thread manager for callers (see
// internal/workspace and internal/server) that need more than the
// tools.ThreadManager seam exposes. Kept separate from SetThreads/Threads,
// which exist purely for the agent-tool wiring; both are set from the
// same manager by the caller (see internal/cmd/threads.go,
// internal/backend/threads.go), but this accessor is additive and neither
// replaces nor is replaced by the other.
func (app *App) SetThreadManager(m any) {
	app.threadManager = m
}

// ThreadManager returns the value passed to SetThreadManager, or nil if
// unset. Typed any for the same import-cycle reason documented on the
// threadManager field; callers type-assert it to *thread.Manager.
func (app *App) ThreadManager() any {
	return app.threadManager
}

// AddCleanup registers fn to run, alongside the built-in cleanup tasks,
// when Shutdown is called. Used by callers that attach extra resources to
// an App post-construction — e.g. the thread manager's own database
// connection — that need to be released on the same schedule.
//
// If called after shutdown has started, returns ErrAppShutdownBlocked
// and the function is not registered.
func (app *App) AddCleanup(fn func(context.Context) error) error {
	app.shutdownMu.Lock()
	defer app.shutdownMu.Unlock()
	if app.shutdownState >= shutdownStateShuttingDown {
		return ErrAppShutdownBlocked
	}
	app.cleanupFuncs = append(app.cleanupFuncs, fn)
	return nil
}

// AddPreCleanupHook registers a stop-and-join operation that must finish
// before ordinary DB-dependent resources, such as MCP, can close. It is used
// by the external watchers, which can otherwise initiate MCP work during
// teardown.
func (app *App) AddPreCleanupHook(fn func(context.Context) error) error {
	app.shutdownMu.Lock()
	defer app.shutdownMu.Unlock()
	if app.shutdownState >= shutdownStateShuttingDown {
		return ErrAppShutdownBlocked
	}
	app.preCleanupHooks = append(app.preCleanupHooks, fn)
	return nil
}

// AddCriticalCleanup registers cleanup that depends on every shutdown hook
// completing. It is intended for a thread database release: if its manager
// did not stop, releasing that database could race a live manager. Critical
// cleanup is deliberately separate from the main App database, which is
// released in the final shutdown phase regardless of hook failures.
func (app *App) AddCriticalCleanup(fn func(context.Context) error) error {
	app.shutdownMu.Lock()
	defer app.shutdownMu.Unlock()
	if app.shutdownState >= shutdownStateShuttingDown {
		return ErrAppShutdownBlocked
	}
	app.criticalCleanupFuncs = append(app.criticalCleanupFuncs, fn)
	return nil
}

// AddShutdownHook registers fn to run before any cleanup functions when
// Shutdown is called. Use this for subsystems (e.g. thread manager) that
// must block new operations as soon as shutdown begins, before other
// cleanup (DB connections, LSP, MCP) tears down their dependencies.
//
// If called after shutdown has started, returns ErrAppShutdownBlocked
// and the function is not registered.
func (app *App) AddShutdownHook(fn func(context.Context) error) error {
	app.shutdownMu.Lock()
	defer app.shutdownMu.Unlock()
	if app.shutdownState >= shutdownStateShuttingDown {
		return ErrAppShutdownBlocked
	}
	app.shutdownHooks = append(app.shutdownHooks, fn)
	return nil
}

// Events returns a per-caller subscription channel for application events.
// Each caller receives its own channel; all callers receive every event.
func (app *App) Events(ctx context.Context) <-chan pubsub.Event[any] {
	return app.events.Subscribe(ctx)
}

// SendEvent publishes a message to all event subscribers.
func (app *App) SendEvent(msg any) {
	app.events.Publish(pubsub.UpdatedEvent, msg)
}

// AgentNotifications returns the broker for agent notification events.
func (app *App) AgentNotifications() *pubsub.Broker[notify.Notification] {
	return app.agentNotifications
}

// RunCompletions returns the broker for the authoritative per-run
// terminal RunComplete events. The dispatcher (backend.runAgent) uses
// it to emit a reliable terminal event when a run fails before the
// coordinator could publish one of its own.
func (app *App) RunCompletions() *pubsub.Broker[notify.RunComplete] {
	return app.runCompletions
}

// ReportCurrentSession tells herdr which session the user is now
// viewing so it can persist a resumable reference for the pane. Safe
// to call when not running inside a herdr pane; the underlying client
// is nil-safe. Call this whenever the active session changes (load,
// new, or select).
func (app *App) ReportCurrentSession(sessionID string) {
	app.herdrClient.SetSessionID(sessionID)
}

func (app *App) UpdateAgentModel(ctx context.Context) error {
	if app.AgentCoordinator == nil {
		return fmt.Errorf("agent configuration is missing")
	}
	return app.AgentCoordinator.UpdateModels(ctx)
}

func (app *App) setupEvents() {
	ctx, cancel := context.WithCancel(app.globalCtx)
	app.eventsCtx = ctx
	setupSubscriber(ctx, app.serviceEventsWG, "sessions", app.Sessions.Subscribe, app.events)
	setupSubscriber(ctx, app.serviceEventsWG, "messages", app.Messages.Subscribe, app.events)
	setupSubscriberMustDeliver(ctx, app.serviceEventsWG, "permissions", app.Permissions.Subscribe, app.events)
	setupSubscriberMustDeliver(ctx, app.serviceEventsWG, "permissions-notifications", app.Permissions.SubscribeNotifications, app.events)
	setupSubscriberMustDeliver(ctx, app.serviceEventsWG, "question-batches", app.Questions.Subscribe, app.events)
	setupSubscriberMustDeliver(ctx, app.serviceEventsWG, "question-notifications", app.Questions.SubscribeNotifications, app.events)
	setupSubscriber(ctx, app.serviceEventsWG, "history", app.History.Subscribe, app.events)
	setupSubscriber(ctx, app.serviceEventsWG, "agent-notifications", app.agentNotifications.Subscribe, app.events)
	setupSubscriberMustDeliver(ctx, app.serviceEventsWG, "run-completions", app.runCompletions.Subscribe, app.events)
	setupSubscriber(ctx, app.serviceEventsWG, "mcp", app.MCP.SubscribeEvents, app.events)
	setupSubscriber(ctx, app.serviceEventsWG, "lsp", app.lsp.SubscribeLSPEvents, app.events)
	if app.Skills != nil {
		setupSubscriber(ctx, app.serviceEventsWG, "skills", app.Skills.SubscribeEvents, app.events)
	}
	cleanupFunc := func(context.Context) error {
		cancel()
		app.serviceEventsWG.Wait()
		app.events.Shutdown()
		return nil
	}
	if err := app.AddCleanup(cleanupFunc); err != nil {
		slog.Error("Failed to register event cleanup", "error", err)
		_ = cleanupFunc(context.Background())
	}
}

func setupSubscriber[T any](
	ctx context.Context,
	wg *sync.WaitGroup,
	name string,
	subscriber func(context.Context) <-chan pubsub.Event[T],
	broker *pubsub.Broker[any],
) {
	wg.Go(func() {
		subCh := subscriber(ctx)
		for {
			select {
			case event, ok := <-subCh:
				if !ok {
					slog.Debug("Subscription channel closed", "name", name)
					return
				}
				broker.Publish(pubsub.UpdatedEvent, any(event))
			case <-ctx.Done():
				slog.Debug("Subscription cancelled", "name", name)
				return
			}
		}
	})
}

// ForwardEvents subscribes to an event source attached to app after
// construction (e.g. the thread manager, wired in post-bootstrap by
// SetThreadManager — see internal/cmd/threads.go and
// internal/backend/threads.go) and forwards its events into app's own
// event fan-in (app.events), so both local-mode (app.Subscribe) and
// client/server-mode (app.Events/SSE) consumers see them exactly like
// every source wired in at New() time by setupEvents.
//
// A free function rather than a method because Go has no generic
// methods; T is inferred from subscribe at the call site. Exported
// because the caller lives in a different package (internal/thread, via
// internal/cmd or internal/backend). Callers must invoke this before
// app.Shutdown tears down app.eventsCtx/app.events — true in practice,
// since thread managers are attached once, early in a workspace's life.
func ForwardEvents[T any](app *App, name string, subscribe func(context.Context) <-chan pubsub.Event[T]) {
	setupSubscriber(app.eventsCtx, app.serviceEventsWG, name, subscribe, app.events)
}

// setupSubscriberMustDeliver is the bounded-blocking fan-in variant of
// setupSubscriber: it re-publishes upstream events onto the shared
// app.events broker using PublishMustDeliver instead of Publish. Use
// this for terminal events that subscribers cannot tolerate losing —
// notably RunComplete, which is the authoritative end-of-run signal
// for `braid run`. A lossy fan-in here can drop the only terminal
// event and hang non-interactive clients waiting on it.
func setupSubscriberMustDeliver[T any](
	ctx context.Context,
	wg *sync.WaitGroup,
	name string,
	subscriber func(context.Context) <-chan pubsub.Event[T],
	broker *pubsub.Broker[any],
) {
	wg.Go(func() {
		subCh := subscriber(ctx)
		for {
			select {
			case event, ok := <-subCh:
				if !ok {
					slog.Debug("Subscription channel closed", "name", name)
					return
				}
				broker.PublishMustDeliver(ctx, pubsub.UpdatedEvent, any(event))
			case <-ctx.Done():
				slog.Debug("Subscription cancelled", "name", name)
				return
			}
		}
	})
}

func (app *App) InitCoderAgent(ctx context.Context) error {
	return app.initCoderAgent(ctx, true)
}

// InitCoderAgentNonInteractive initializes the coder agent without
// interactive-only tools (e.g. question).
func (app *App) InitCoderAgentNonInteractive(ctx context.Context) error {
	return app.initCoderAgent(ctx, false)
}

func (app *App) initCoderAgent(ctx context.Context, interactive bool) error {
	coderAgentCfg := app.config.Config().Agents[config.AgentCoder]
	if coderAgentCfg.ID == "" {
		return fmt.Errorf("coder agent configuration is missing")
	}
	var err error
	app.AgentCoordinator, err = agent.NewCoordinator(ctx, agent.CoordinatorOptions{
		Config:           app.config,
		Sessions:         app.Sessions,
		Messages:         app.Messages,
		Permissions:      app.Permissions,
		Questions:        app.Questions,
		History:          app.History,
		FileTracker:      app.FileTracker,
		LSPManager:       app.LSPManager,
		Notify:           app.agentNotifications,
		RunComplete:      app.runCompletions,
		Skills:           app.Skills,
		Interactive:      interactive,
		MCP:              app.MCP,
		BackgroundShells: app.BackgroundShells,
	})
	if err != nil {
		slog.Error("Failed to create coder agent", "err", err)
		return err
	}
	return nil
}

// Subscribe streams application events to send until ctx (app.globalCtx
// scoped internally) is torn down. It is decoupled from bubbletea's
// concrete *tea.Program so this core package doesn't need to import
// it; onPanic is invoked instead of a hardcoded program.Quit() if the
// delivery loop panics. Callers that feed a *tea.Program (see
// workspace.AppWorkspace.Subscribe) pass program.Send and program.Quit.
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

// Shutdown performs a graceful, race-safe shutdown of the application.
// It is idempotent: repeated or concurrent callers wait for the same
// teardown and return together. The shutdown order is:
//
//  1. Run shutdown hooks first (e.g. thread manager admission block),
//     before anything else, so subsystems that depend on other resources
//     (DB, MCP, LSP) are blocked before those resources are torn down.
//  2. Cancel all agents and wait for them to finish.
//  3. Flush pending message updates.
//  4. Close MCP and other ordinary resources sequentially. Watchers have
//     already stopped and joined, so none can begin MCP work while it closes.
//  5. Release critical dependent resources only after their hooks succeeded,
//     then release the main DB last.
//  6. Kill background shells, stop LSP, close coordinator readiness
//     work, signal herdr, emit AppExited — in parallel where safe.
func (app *App) Shutdown() {
	app.shutdownMu.Lock()
	// If already done, return immediately.
	if app.shutdownState == shutdownStateDone {
		app.shutdownMu.Unlock()
		return
	}
	// If already shutting down, wait for completion.
	if app.shutdownState == shutdownStateShuttingDown {
		done := app.shutdownDone
		app.shutdownMu.Unlock()
		<-done
		return
	}
	// Enter shutting down state, create done channel.
	app.shutdownState = shutdownStateShuttingDown
	app.shutdownDone = make(chan struct{})

	// Capture phased hooks and cleanups under the lock so no new ones arrive
	// mid-shutdown.
	preCleanupHooks := make([]func(context.Context) error, len(app.preCleanupHooks))
	copy(preCleanupHooks, app.preCleanupHooks)
	app.preCleanupHooks = nil
	hooks := make([]func(context.Context) error, len(app.shutdownHooks))
	copy(hooks, app.shutdownHooks)
	app.shutdownHooks = nil // prevent concurrent AddShutdownHook from appending
	cleanups := make([]func(context.Context) error, len(app.cleanupFuncs))
	copy(cleanups, app.cleanupFuncs)
	app.cleanupFuncs = nil // prevent concurrent AddCleanup from appending
	criticalCleanups := make([]func(context.Context) error, len(app.criticalCleanupFuncs))
	copy(criticalCleanups, app.criticalCleanupFuncs)
	app.criticalCleanupFuncs = nil
	app.shutdownMu.Unlock()

	start := time.Now()
	defer func() { slog.Debug("Shutdown took " + time.Since(start).String()) }()
	defer close(app.shutdownDone)

	// 1. Stop and join watchers before MCP closes. A failed watcher hook means
	// it may still initiate MCP work, so MCP cannot safely be closed.
	preCleanupHooksSucceeded := true
	for _, hook := range preCleanupHooks {
		if hook != nil {
			if err := app.runShutdownCallback(hook); err != nil {
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
			if err := app.runShutdownCallback(hook); err != nil {
				hooksSucceeded = false
				slog.Error("Failed to run shutdown hook", "error", err)
			}
		}
	}

	// 2. Stop agent-owned work before closing MCP or database resources it may
	// still use. CancelAll bounds its own wait; retain dependencies when active
	// work or coordinator readiness work does not stop.
	agentWorkStopped := true
	if app.AgentCoordinator != nil {
		app.AgentCoordinator.CancelAll()
		if app.AgentCoordinator.IsBusy() {
			agentWorkStopped = false
			slog.Error("Agent work did not stop before shutdown deadline")
		}
		if closer, ok := app.AgentCoordinator.(coordinatorCloser); ok {
			if err := app.runShutdownCallback(closer.Close); err != nil {
				agentWorkStopped = false
				slog.Error("Failed to close agent coordinator readiness work", "error", err)
			}
		}
	}

	// Initial MCP setup is independent of watcher-triggered reinitialization.
	// Cancel and join it before Registry.Close so it cannot publish a session
	// after the registry has closed.
	mcpInitStopped := true
	if app.mcpInitCancel != nil {
		if err := app.runShutdownCallback(func(context.Context) error {
			app.mcpInitCancel()
			app.mcpInitWG.Wait()
			return nil
		}); err != nil {
			mcpInitStopped = false
			slog.Error("Failed to stop MCP initialization", "error", err)
		}
	}

	// 3. Flush any debounced message updates before the DB-close cleanup
	// runs. message.Service buffers streaming deltas and we must land
	// them while the connection is still open.
	if app.Messages != nil {
		ctx, cancel := context.WithTimeout(context.Background(), app.shutdownTimeout)
		if err := app.Messages.FlushAll(ctx); err != nil {
			slog.Error("Failed to flush pending message updates on shutdown", "error", err)
		}
		cancel()
	}

	dependenciesStopped := preCleanupHooksSucceeded && agentWorkStopped && mcpInitStopped

	// 4. MCP can use the App database during close, so close it after all
	// stop-and-join hooks and before the database's final release.
	if dependenciesStopped && app.mcpClose != nil {
		if err := app.runShutdownCallback(app.mcpClose); err != nil {
			dependenciesStopped = false
			slog.Error("Failed to close MCP on shutdown", "error", err)
		}
	}

	// General cleanups do not determine the ordering of production resources.
	for _, cleanup := range cleanups {
		if cleanup != nil {
			if err := app.runShutdownCallback(cleanup); err != nil {
				slog.Error("Failed to cleanup app properly on shutdown", "error", err)
			}
		}
	}

	// 5. Never release a thread DB used by a hook that failed to finish.
	if hooksSucceeded {
		for _, cleanup := range criticalCleanups {
			if cleanup != nil {
				if err := app.runShutdownCallback(cleanup); err != nil {
					slog.Error("Failed to run critical app cleanup on shutdown", "error", err)
				}
			}
		}
	}
	if dependenciesStopped && app.mainDBRelease != nil {
		if err := app.runShutdownCallback(app.mainDBRelease); err != nil {
			slog.Error("Failed to release main database on shutdown", "error", err)
		}
	}

	// 6. Parallel independent teardown.
	var wg sync.WaitGroup

	wg.Go(func() {
		event.AppExited()
	})

	wg.Go(func() {
		if app.BackgroundShells == nil {
			return
		}
		if err := app.runShutdownCallback(app.BackgroundShells.Shutdown); err != nil {
			slog.Error("Failed to stop background shells", "error", err)
		}
	})

	if app.herdrClient != nil {
		app.herdrClient.Close()
	}

	wg.Go(func() {
		if app.LSPManager != nil {
			if err := app.runShutdownCallback(func(ctx context.Context) error {
				app.LSPManager.KillAll(ctx)
				return nil
			}); err != nil {
				slog.Error("Failed to stop LSP clients", "error", err)
			}
		}
	})

	wg.Wait()

	app.shutdownMu.Lock()
	app.shutdownState = shutdownStateDone
	app.shutdownMu.Unlock()
}
