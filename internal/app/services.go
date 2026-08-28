package app

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/rave-soft/sennit/internal/agent"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/credentials"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/event"
	"github.com/rave-soft/sennit/internal/filetracker"
	"github.com/rave-soft/sennit/internal/herdr"
	historystore "github.com/rave-soft/sennit/internal/history/store"
	"github.com/rave-soft/sennit/internal/latency"
	"github.com/rave-soft/sennit/internal/lsp"
	messagestore "github.com/rave-soft/sennit/internal/message/store"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/question"
	sessionstore "github.com/rave-soft/sennit/internal/session/store"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/thread"
)

// delegationManagerSnapshot is the single ownership representation of this
// workspace's delegation managers: the concrete thread manager (nil for
// non-git workspaces), the concrete task manager, and the tool-facing
// adapters Attach derives from that pair. It is published atomically as
// one value, so a reader never observes a mix of a manager and an adapter
// from different attach generations.
//
// The adapters live here — not only handed to the agent coordinator once,
// at attachment time, the way this used to work — because a coordinator
// built or rebuilt after Attach ran (initCoderAgent's non-interactive
// path, in particular) has no other way to recover them; see
// initCoderAgent's use of delegationToolAdapters.
type delegationManagerSnapshot struct {
	thread      *thread.Manager
	task        *thread.TaskManager
	threadTools tools.ThreadManager
	taskTools   tools.TaskManager
}

// appServices groups the domain services and workspace-scoped resources
// New wires together: session/message/permission/etc. services, the agent
// coordinator, the LSP/MCP/skills managers, and this workspace's own
// config/credentials state. It is embedded anonymously in App, so every
// field here is promoted onto *App exactly as it was before this type
// existed (app.sessions, app.MCP, app.Coordinator(), ...) — App is a
// facade over this plus appEvents and shutdownPhases, not a new API.
type appServices struct {
	// The three service fields below are unexported with accessors on
	// top (see thread_workspace.go) so that *App can satisfy
	// thread.Workspace — Go forbids a method and a field sharing a name,
	// and the domain interface internal/thread drives its delegations
	// through requires method accessors. Read them through the
	// Sessions()/Messages()/Permissions() accessors, not as fields.
	sessions    sessionstore.Service
	messages    messagestore.Service
	History     historystore.Service
	permissions permission.Service
	Questions   question.Service
	FileTracker filetracker.Service
	// Latency records how long steering messages and finished background
	// delegations waited before reaching the model. Read back by
	// `sennit stat --by latency`.
	Latency latency.Recorder

	// agentCoordinatorMu guards agentCoordinator: initCoderAgent (on the
	// main goroutine, typically) and SetDelegationManagers's republish can
	// swap it while workspace.AppWorkspace's many methods read it from
	// request-handling goroutines. Read/write only through Coordinator()
	// and setCoordinator (see thread_workspace.go) — never the field
	// directly.
	agentCoordinatorMu sync.RWMutex
	agentCoordinator   agent.Coordinator

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

	// delegationManagers is the single atomic ownership snapshot of this
	// workspace's delegation managers (concrete thread and task pair).
	// Nil until wired post-bootstrap by the caller (see
	// internal/app/threadspawn/attach.go) via SetDelegationManagers. The
	// thread manager is nil for workspaces that don't own one: non-git
	// workspaces and thread workspaces themselves (nesting is not
	// supported). Set on the main goroutine and read from others (see
	// SetPermissionsSkip), hence atomic.
	delegationManagers atomic.Pointer[delegationManagerSnapshot]

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
		sessions:         sessionstore.NewService(q, conn, store.WorkingDir(), sessionstore.WithTelemetry(event.NewSessionTelemetry())),
		messages:         messagestore.NewService(q),
		queries:          q,
		History:          historystore.NewService(q, conn),
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

// SetDelegationManagers publishes one consistent delegation snapshot: the
// concrete thread/task manager pair together with the tool adapters Attach
// derived from them.
//
// Earlier, the adapters were deliberately not accepted or stored here —
// Attach derived them from this same pair and published them to the
// coordinator separately, on the theory that App never needed them again.
// That fell apart the moment a coordinator could be built or rebuilt after
// Attach ran: initCoderAgent's non-interactive path had no way to recover
// the adapters it had already lost, so `sennit run` silently built a
// coordinator with no thread_*/task tools. Storing the adapters here lets
// initCoderAgent (via delegationToolAdapters) hand them to a freshly built
// coordinator up front instead.
//
// If a coordinator already exists, this also republishes the adapters to
// it directly, so callers like Attach no longer need to reach into
// app.Coordinator() themselves.
func (app *App) SetDelegationManagers(threadMgr *thread.Manager, taskMgr *thread.TaskManager, threadTools tools.ThreadManager, taskTools tools.TaskManager) {
	app.delegationManagers.Store(&delegationManagerSnapshot{
		thread:      threadMgr,
		task:        taskMgr,
		threadTools: threadTools,
		taskTools:   taskTools,
	})
	if coord := app.Coordinator(); coord != nil {
		coord.SetDelegationTools(threadTools, taskTools)
	}
}

// delegationToolAdapters returns the tool adapters from the current
// delegation snapshot, for handing to a coordinator at construction time
// (see initCoderAgent). Both are nil-safe on the CoordinatorOptions side,
// so a workspace with no delegation managers wired yet (or none at all,
// e.g. a non-git workspace) simply builds a coordinator with no
// thread_*/task tools, same as before this existed.
func (app *App) delegationToolAdapters() (tools.ThreadManager, tools.TaskManager) {
	s := app.delegationManagers.Load()
	if s == nil {
		return nil, nil
	}
	return s.threadTools, s.taskTools
}

// ThreadManager returns this workspace's concrete thread manager from the
// delegation snapshot, or nil if unset or if this workspace owns no
// thread manager (non-git, nested thread workspace).
func (app *App) ThreadManager() *thread.Manager {
	s := app.delegationManagers.Load()
	if s == nil {
		return nil
	}
	return s.thread
}

// TaskManager returns this workspace's concrete task manager from the
// delegation snapshot, or nil if unset.
func (app *App) TaskManager() *thread.TaskManager {
	s := app.delegationManagers.Load()
	if s == nil {
		return nil
	}
	return s.task
}

// delegationManagersForTest returns the current snapshot as a value, for
// consistency tests that must observe one attach generation atomically
// rather than as two separate accessor reads.
func (app *App) delegationManagersForTest() delegationManagerSnapshot {
	if s := app.delegationManagers.Load(); s != nil {
		return *s
	}
	return delegationManagerSnapshot{}
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
	if mgr := app.ThreadManager(); mgr != nil {
		mgr.SetPermissionsSkip(skip)
	}
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
	coord := app.Coordinator()
	if coord == nil {
		return fmt.Errorf("agent configuration is missing")
	}
	return coord.UpdateModels(ctx)
}

func (app *App) InitCoderAgent(ctx context.Context) error {
	return app.initCoderAgent(ctx, true)
}

// InitCoderAgentNonInteractive initializes the coder agent without
// interactive-only tools (e.g. question).
func (app *App) InitCoderAgentNonInteractive(ctx context.Context) error {
	return app.initCoderAgent(ctx, false)
}

// newCoordinator builds the agent coordinator. It is a variable — rather
// than initCoderAgent calling agent.NewCoordinator directly — so tests can
// substitute a fake constructor and verify initCoderAgent's option wiring
// and its replace/close ordering without booting a real coordinator's
// readiness work; see internal/app/threadspawn's attachDeps for the same
// pattern.
var newCoordinator = agent.NewCoordinator

// initCoderAgent (re)builds the coder agent coordinator. It re-applies
// whatever delegation tool adapters are already published (see
// delegationToolAdapters) so that a rebuild — InitCoderAgentNonInteractive
// running after InitCoderAgent already ran and Attach already published
// the thread/task tools, as `sennit run` does — never leaves the new
// coordinator without them.
//
// The old coordinator, if any, is only replaced and closed once the new
// one is built successfully: a failed NewCoordinator must leave the
// existing coordinator in place and working, not overwrite the field with
// the error's nil.
func (app *App) initCoderAgent(ctx context.Context, interactive bool) error {
	coderAgentCfg := app.config.Config().Agents[config.AgentCoder]
	if coderAgentCfg.ID == "" {
		return fmt.Errorf("coder agent configuration is missing")
	}
	threadTools, taskTools := app.delegationToolAdapters()
	newCoord, err := newCoordinator(ctx, agent.CoordinatorOptions{
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
		Threads:          threadTools,
		Tasks:            taskTools,
	})
	if err != nil {
		slog.Error("Failed to create coder agent", "err", err)
		return err
	}

	old := app.Coordinator()
	app.setCoordinator(newCoord)
	if old != nil {
		if closer, ok := old.(coordinatorCloser); ok {
			if err := closer.Close(ctx); err != nil {
				slog.Error("Failed to close previous agent coordinator", "err", err)
			}
		}
	}
	return nil
}
