package app

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/rave-soft/sennit/internal/agent"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/credentials"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/filetracker"
	"github.com/rave-soft/sennit/internal/herdr"
	"github.com/rave-soft/sennit/internal/history"
	"github.com/rave-soft/sennit/internal/latency"
	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/thread"
)

// appServices groups the domain services and workspace-scoped resources
// New wires together: session/message/permission/etc. services, the agent
// coordinator, the LSP/MCP/skills managers, and this workspace's own
// config/credentials state. It is embedded anonymously in App, so every
// field here is promoted onto *App exactly as it was before this type
// existed (app.sessions, app.MCP, app.AgentCoordinator, ...) — App is a
// facade over this plus appEvents and shutdownPhases, not a new API.
type appServices struct {
	// The three service fields below are unexported with accessors on
	// top (see thread_workspace.go) so that *App can satisfy
	// thread.Workspace — Go forbids a method and a field sharing a name,
	// and the domain interface internal/thread drives its delegations
	// through requires method accessors. Read them through the
	// Sessions()/Messages()/Permissions() accessors, not as fields.
	sessions    session.Service
	messages    message.Service
	History     history.Service
	permissions permission.Service
	Questions   question.Service
	FileTracker filetracker.Service
	// Latency records how long steering messages and finished background
	// delegations waited before reaching the model. Read back by
	// `sennit stat --by latency`.
	Latency latency.Recorder

	AgentCoordinator agent.Coordinator

	LSPManager *lsp.Manager

	// lsp holds this workspace's own LSP client states and event broker;
	// see the lspEvents doc in lsp_events.go for why it isn't shared.
	lsp *lspEvents

	// MCP is this workspace's own MCP registry: sessions, per-server
	// state, auth handlers, and the event broker all live here rather
	// than on mcp's process-wide defaultRegistry, so two App instances in
	// one process (the top-level workspace and a spawned thread's
	// workspace) don't clobber each other's MCP servers.
	MCP              *mcp.Registry
	BackgroundShells *shell.BackgroundShellManager

	Skills *skills.Manager

	// queries is the raw generated query set this App's services were
	// built on, kept so read-only reporting that has no service of its
	// own — the usage aggregation behind /stats and `sennit stat` — can
	// run against the same connection instead of opening a second one.
	// Everything that mutates state still goes through a service.
	queries *db.Queries

	// Threads is the thread manager owning this workspace's parallel
	// agent work streams, wired in post-bootstrap by the caller (see
	// internal/app/app.go and internal/app/threadspawn/attach.go) via
	// SetThreads. Declared as the tool-facing interface, not
	// *thread.Manager, because internal/thread imports this package —
	// see internal/agent/tools/thread_manager.go for the seam. Nil for
	// workspaces that don't own a thread manager: non-git workspaces and
	// thread workspaces themselves (nesting is not supported).
	Threads tools.ThreadManager

	// threadManager holds the same thread manager as Threads, but typed
	// *thread.Manager instead of tools.ThreadManager: internal/workspace
	// needs the concrete type (Subscribe, full-arg Merge/Remove, etc.),
	// which is richer than the tools.ThreadManager seam built for the
	// agent-tool wiring. Nil until wired post-bootstrap by the caller (see
	// internal/app/threadspawn/attach.go) via SetThreadManager, independent
	// of SetThreads/Threads — both are set from the same manager, but this
	// field is additive and neither replaces nor is replaced by the other.
	// Held atomically: it is set on the main goroutine post-bootstrap and
	// read from others — SetPermissionsSkip runs on the config-watcher
	// goroutine, and internal/workspace reads it from wherever it is
	// called. A plain field made that a data race.
	threadManager atomic.Pointer[thread.Manager]

	// Tasks is the task delegation manager's tool-facing counterpart to
	// Threads, wired in post-bootstrap alongside it (see attach.go) via
	// SetTasks. Declared as tools.TaskManager for the same import-cycle
	// reason Threads is tools.ThreadManager. Nil for workspaces that
	// don't own a task manager, same as Threads.
	Tasks tools.TaskManager

	// taskManager holds the same task manager as Tasks, but typed
	// *thread.TaskManager instead of tools.TaskManager, for the same reason
	// threadManager is concretely typed relative to Threads. Set via
	// SetTaskManager, independent of SetTasks/Tasks.
	// Held atomically for the same reason as threadManager.
	taskManager atomic.Pointer[thread.TaskManager]

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

	// credentials is the single, process-wide OAuth credentials manager
	// for this store, constructed once in New. Every consumer — the
	// workspace (SignalAuthComplete, RefreshOAuthToken, ImportCopilot)
	// and the agent coordinator (WaitForTokenChange, RefreshOAuthToken)
	// — must go through the same instance, since SignalAuthComplete and
	// WaitForTokenChange communicate through channels private to it. See
	// Credentials and credentials.Manager's doc comment.
	credentials *credentials.Manager

	// herdrClient reports agent state to herdr when running inside
	// a herdr-managed pane. Nil when not in a herdr environment.
	herdrClient *herdr.Client
	// herdrCancel stops herdr.BridgeLocal's forwarding goroutines, which
	// are bound to a context New derives for exactly this purpose rather
	// than to ctx directly: herdrClient.Close() (Shutdown's phase 6, and
	// InitCoderAgent's failure path) only releases the pane and closes
	// the socket, it does not stop those goroutines on its own.
	herdrCancel context.CancelFunc
}

// newAppServices builds the appServices grouping for New: the session/
// message/permission/etc. services, this workspace's own LSP/MCP/skills
// managers, and its config/credentials state.
func newAppServices(q *db.Queries, conn *sql.DB, store *config.ConfigStore, skillsMgr *skills.Manager) *appServices {
	cfg := store.Config()
	skipPermissionsRequests := store.Overrides().SkipPermissionRequests
	var allowedTools []string
	var configBypass bool
	if cfg.Permissions != nil {
		allowedTools = cfg.Permissions.AllowedTools
		configBypass = cfg.Permissions.Bypass
		skipPermissionsRequests = skipPermissionsRequests || configBypass
	}
	return &appServices{
		sessions:         session.NewService(q, conn, store.WorkingDir()),
		messages:         message.NewService(q),
		queries:          q,
		History:          history.NewService(q, conn),
		permissions:      permission.NewPermissionService(store.WorkingDir(), skipPermissionsRequests, allowedTools),
		Questions:        question.NewService(),
		FileTracker:      filetracker.NewService(q, store.WorkingDir()),
		Latency:          latency.NewService(q),
		LSPManager:       lsp.NewManager(store),
		lsp:              newLSPEvents(),
		MCP:              mcp.NewRegistry(),
		BackgroundShells: shell.NewBackgroundShellManager(),
		Skills:           skillsMgr,
		lastConfigBypass: configBypass,
		config:           store,
		credentials:      credentials.New(store),
	}
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

// Credentials returns this App's single OAuth credentials manager. See
// the credentials field doc for why callers must not construct their own.
func (app *App) Credentials() *credentials.Manager {
	return app.credentials
}

// SetThreads wires the thread manager owning this workspace's parallel
// agent work streams, forwarding it to the coder agent so the thread_*
// tools become available. Safe to call with nil to clear it. The caller
// (see internal/app/threadspawn/attach.go) is responsible for deciding
// whether this workspace should own one at all.
func (app *App) SetThreads(threads tools.ThreadManager) {
	app.Threads = threads
	if app.AgentCoordinator != nil {
		app.AgentCoordinator.SetThreads(threads)
	}
}

// SetThreadManager wires the concrete thread manager for callers (see
// internal/workspace) that need more than the tools.ThreadManager seam
// exposes. Kept separate from SetThreads/Threads, which exist purely for
// the agent-tool wiring; both are set from the same manager by the caller
// (see internal/app/threadspawn/attach.go), but this accessor is
// additive and neither replaces nor is replaced by the other.
func (app *App) SetThreadManager(m *thread.Manager) {
	app.threadManager.Store(m)
}

// ThreadManager returns the value passed to SetThreadManager, or nil if
// unset.
func (app *App) ThreadManager() *thread.Manager {
	return app.threadManager.Load()
}

// PermissionsSkipFunc returns an accessor for this workspace's live
// permission-bypass ("yolo") state, for the thread spawners that must hand
// it to a delegation workspace at spawn time (see internal/cmd/root.go).
//
// It exists so callers cannot disagree about where the answer comes from.
// They used to each read Store().Overrides().SkipPermissionRequests
// - the --yolo flag as it was at bootstrap - which meant a thread created
// after a ctrl+y or /yolo toggle inherited the state the process started
// in, not the state the user was actually in.
func (app *App) PermissionsSkipFunc() func() bool {
	return func() bool { return app.Permissions().SkipRequests() }
}

// SetPermissionsSkip sets this workspace's permission-bypass ("yolo")
// state and propagates it to every delegation workspace live under it.
//
// This is the single entry point for changing bypass state: the TUI's
// ctrl+y and /yolo, and a permissions.bypass config reload all funnel
// through here. Calling
// Permissions.SetSkipRequests directly sets only this app's own flag and
// leaves running threads - which have permission services of their own -
// on whatever state they were spawned with.
func (app *App) SetPermissionsSkip(skip bool) {
	app.Permissions().SetSkipRequests(skip)
	if mgr := app.threadManager.Load(); mgr != nil {
		mgr.SetPermissionsSkip(skip)
	}
}

// SetTasks wires the task delegation manager, forwarding it to the coder
// agent so the built-in agent tool's background mode becomes available.
// Mirrors SetThreads; safe to call with nil to clear it.
func (app *App) SetTasks(tasks tools.TaskManager) {
	app.Tasks = tasks
	if app.AgentCoordinator != nil {
		app.AgentCoordinator.SetTasks(tasks)
	}
}

// SetTaskManager wires the concrete task manager for callers that need it,
// mirroring SetThreadManager.
func (app *App) SetTaskManager(m *thread.TaskManager) {
	app.taskManager.Store(m)
}

// TaskManager returns the value passed to SetTaskManager, or nil if unset.
func (app *App) TaskManager() *thread.TaskManager {
	return app.taskManager.Load()
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
		Credentials:      app.credentials,
		Sessions:         app.sessions,
		Messages:         app.messages,
		Permissions:      app.permissions,
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
		Latency:          app.Latency,
	})
	if err != nil {
		slog.Error("Failed to create coder agent", "err", err)
		return err
	}
	return nil
}
