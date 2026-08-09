// Package app wires together services, coordinates agents, and manages
// application lifecycle.
package app

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/braid/internal/agent"
	"github.com/rave-soft/braid/internal/agent/notify"
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
	MCP *mcp.Registry

	Skills *skills.Manager

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
	globalCtx          context.Context
	cleanupFuncs       []func(context.Context) error
	agentNotifications *pubsub.Broker[notify.Notification]
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
}

// New initializes a new application instance. skillsMgr carries the
// per-workspace skill discovery results computed by the caller; the
// caller is responsible for constructing it (typically via
// skills.NewManager + skills.DiscoverFromConfig).
func New(ctx context.Context, conn *sql.DB, store *config.ConfigStore, skillsMgr *skills.Manager) (*App, error) {
	q := db.New(conn)
	sessions := session.NewService(q, conn)
	messages := message.NewService(q)
	files := history.NewService(q, conn)
	cfg := store.Config()
	skipPermissionsRequests := store.Overrides().SkipPermissionRequests
	var allowedTools []string
	if cfg.Permissions != nil && cfg.Permissions.AllowedTools != nil {
		allowedTools = cfg.Permissions.AllowedTools
	}

	app := &App{
		Sessions:    sessions,
		Messages:    messages,
		History:     files,
		Permissions: permission.NewPermissionService(store.WorkingDir(), skipPermissionsRequests, allowedTools),
		Questions:   question.NewService(),
		FileTracker: filetracker.NewService(q, store.WorkingDir()),
		LSPManager:  lsp.NewManager(store),
		lsp:         newLSPEvents(),
		MCP:         mcp.NewRegistry(),
		Skills:      skillsMgr,

		globalCtx: ctx,

		config: store,

		events:             pubsub.NewBroker[any](),
		serviceEventsWG:    &sync.WaitGroup{},
		tuiWG:              &sync.WaitGroup{},
		agentNotifications: pubsub.NewBroker[notify.Notification](),
		runCompletions:     pubsub.NewBroker[notify.RunComplete](),
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
	go app.MCP.Initialize(ctx, app.Permissions, store)

	// Start herdr integration when running inside a herdr pane.
	app.herdrClient = herdr.Init()
	herdr.BridgeLocal(ctx, app.herdrClient, herdr.BridgeSources{
		PermRequests:      app.Permissions,
		PermNotifications: app.Permissions,
		RunCompletions:    app.runCompletions,
		Messages:          app.Messages,
	})

	// Release the shared database connection on shutdown. The pool
	// closes the underlying *sql.DB when the last reference is released.
	dataDir := cfg.Options.DataDirectory
	app.cleanupFuncs = append(
		app.cleanupFuncs,
		func(context.Context) error { return db.Release(dataDir) },
		func(ctx context.Context) error { return app.MCP.Close(ctx) },
	)

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

// GetDefaultSmallModel returns the default small model for the given
// provider. Falls back to the large model if no default is found.
func (app *App) GetDefaultSmallModel(providerID string) config.SelectedModel {
	cfg := app.config.Config()
	largeModelCfg := cfg.Models[config.SelectedModelTypeLarge]

	// Find the provider in the known providers list to get its default small model.
	knownProviders := app.config.KnownProviders()
	var knownProvider *catwalk.Provider
	for _, p := range knownProviders {
		if string(p.ID) == providerID {
			knownProvider = &p
			break
		}
	}

	// For unknown/local providers, use the large model as small.
	if knownProvider == nil {
		slog.Warn("Using large model as small model for unknown provider", "provider", providerID, "model", largeModelCfg.Model)
		return largeModelCfg
	}

	defaultSmallModelID := knownProvider.DefaultSmallModelID
	model := cfg.GetModel(providerID, defaultSmallModelID)
	if model == nil {
		slog.Warn("Default small model not found, using large model", "provider", providerID, "model", largeModelCfg.Model)
		return largeModelCfg
	}

	slog.Info("Using provider default small model", "provider", providerID, "model", defaultSmallModelID)
	return config.SelectedModel{
		Provider:        providerID,
		Model:           defaultSmallModelID,
		MaxTokens:       model.DefaultMaxTokens,
		ReasoningEffort: model.DefaultReasoningEffort,
	}
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
	app.cleanupFuncs = append(app.cleanupFuncs, cleanupFunc)
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
		Config:      app.config,
		Sessions:    app.Sessions,
		Messages:    app.Messages,
		Permissions: app.Permissions,
		Questions:   app.Questions,
		History:     app.History,
		FileTracker: app.FileTracker,
		LSPManager:  app.LSPManager,
		Notify:      app.agentNotifications,
		RunComplete: app.runCompletions,
		Skills:      app.Skills,
		Interactive: interactive,
		MCP:         app.MCP,
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
	app.cleanupFuncs = append(app.cleanupFuncs, func(context.Context) error {
		slog.Debug("Cancelling TUI message handler")
		tuiCancel()
		app.tuiWG.Wait()
		return nil
	})
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

// Shutdown performs a graceful shutdown of the application.
func (app *App) Shutdown() {
	start := time.Now()
	defer func() { slog.Debug("Shutdown took " + time.Since(start).String()) }()

	// First, cancel all agents and wait for them to finish. This must complete
	// before closing the DB so agents can finish writing their state.
	if app.AgentCoordinator != nil {
		app.AgentCoordinator.CancelAll()
	}

	// Shared shutdown context for all timeout-bounded cleanup.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Drain any debounced message updates before the DB-close cleanup
	// runs in the parallel block below. message.Service buffers
	// streaming deltas (see internal/message/message.go) and we must
	// land them while the connection is still open.
	if app.Messages != nil {
		if err := app.Messages.FlushAll(shutdownCtx); err != nil {
			slog.Error("Failed to flush pending message updates on shutdown", "error", err)
		}
	}

	// Now run remaining cleanup tasks in parallel.
	var wg sync.WaitGroup

	// Send exit event
	wg.Go(func() {
		event.AppExited()
	})

	// Kill all background shells.
	wg.Go(func() {
		shell.GetBackgroundShellManager().KillAll(shutdownCtx)
	})

	// Close herdr client to stop its background writer.
	app.herdrClient.Close()

	// Shutdown all LSP clients.
	wg.Go(func() {
		app.LSPManager.KillAll(shutdownCtx)
	})

	// Call all cleanup functions.
	for _, cleanup := range app.cleanupFuncs {
		if cleanup != nil {
			wg.Go(func() {
				if err := cleanup(shutdownCtx); err != nil {
					slog.Error("Failed to cleanup app properly on shutdown", "error", err)
				}
			})
		}
	}
	wg.Wait()
}
