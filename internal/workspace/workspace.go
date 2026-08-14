// Package workspace defines the Workspace interface used by all
// frontends (TUI, CLI) to interact with a running workspace. Two
// implementations exist: one wrapping a local app.App instance and one
// wrapping the HTTP client SDK.
package workspace

import (
	"context"
	"errors"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"

	mcptools "github.com/rave-soft/braid/internal/agent/tools/mcp"
	"github.com/rave-soft/braid/internal/commands"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/git"
	"github.com/rave-soft/braid/internal/history"
	"github.com/rave-soft/braid/internal/lsp"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/oauth"
	"github.com/rave-soft/braid/internal/permission"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/question"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/shell"
	"github.com/rave-soft/braid/internal/skills"
)

// Reasons the coder agent may be unavailable, returned by
// Workspace.AgentReadyErr so callers can tell a genuinely
// uninitialized agent apart from a lost server connection.
var (
	// ErrAgentNotInitialized means the workspace exists but its coder
	// agent has not been configured/initialized (e.g. no model set).
	ErrAgentNotInitialized = errors.New("coder agent is not initialized")
	// ErrServerUnreachable means the client could not reach the server
	// to determine the agent's status (server down, or the workspace was
	// torn down out from under the client).
	ErrServerUnreachable = errors.New("lost connection to the braid server")
	// ErrWorkspaceGone means the server is reachable but no longer knows
	// this client's workspace: it was torn down, or the server was
	// replaced underneath the client. The subscription loop re-registers
	// the workspace in the background when it sees this.
	ErrWorkspaceGone = errors.New("the server reset this workspace; reconnecting")
	// ErrStreamClosed means an established event stream ended.
	// Resubscribing usually succeeds immediately, but events published in
	// the meantime are lost for good, so the client treats it as a
	// degraded link that requires a resync.
	ErrStreamClosed = errors.New("the event stream closed; reconnecting")
	// ErrThreadsNotSupported means this workspace has no thread manager
	// attached (not a git repository, or is itself a thread's own nested
	// workspace — nesting is not supported). Returned by every
	// ThreadController method except SupportsThreads when it reports false.
	ErrThreadsNotSupported = errors.New("workspace does not support threads")
	// ErrTasksNotSupported means this workspace has no task manager
	// attached. Returned by every TaskController method except
	// SupportsTasks when it reports false.
	ErrTasksNotSupported = errors.New("workspace does not support tasks")
)

// ConnectionState describes the health of the client-server link as
// reported by the [ClientWorkspace] subscription loop.
type ConnectionState int

const (
	// ConnectionDegraded means the event stream is down (or the workspace
	// was lost server-side) and the client is retrying or re-registering
	// in the background.
	ConnectionDegraded ConnectionState = iota
	// ConnectionRecovered means the event stream was re-established,
	// possibly against a re-created workspace.
	ConnectionRecovered
)

// ConnectionEvent is delivered to the TUI as a tea.Msg on degraded and
// recovered transitions of the client-server link. Local (in-process)
// workspaces never emit it.
type ConnectionEvent struct {
	State ConnectionState
	// Err is the most recent failure, set when State is
	// ConnectionDegraded.
	Err error
	// Stuck marks a degraded connection that has resisted repeated
	// recovery attempts. The loop keeps retrying regardless; the UI
	// should escalate from a transient notice to a persistent error.
	Stuck bool
}

// LSPClientInfo is the frontend-facing LSP client state.
type LSPClientInfo = lsp.ClientInfo

// LSPEventType represents the type of LSP event.
type LSPEventType string

const (
	LSPEventStateChanged       LSPEventType = "state_changed"
	LSPEventDiagnosticsChanged LSPEventType = "diagnostics_changed"
)

// LSPEvent represents an LSP event forwarded to the TUI.
type LSPEvent struct {
	Type            LSPEventType
	Name            string
	State           lsp.ServerState
	Error           error
	DiagnosticCount int
}

// AgentModel holds the model information exposed to the UI.
type AgentModel struct {
	CatalogCfg catwalk.Model
	ModelCfg   config.SelectedModel
}

// SessionStore covers session CRUD and message reads: everything the
// sessions dialog, chat loading, and history/summarization code need
// without pulling in the rest of Workspace.
type SessionStore interface {
	CreateSession(ctx context.Context, title string) (session.Session, error)
	GetSession(ctx context.Context, sessionID string) (session.Session, error)
	ListSessions(ctx context.Context) ([]session.Session, error)
	SaveSession(ctx context.Context, sess session.Session) (session.Session, error)
	DeleteSession(ctx context.Context, sessionID string) error
	CreateAgentToolSessionID(messageID, toolCallID string) string
	ParseAgentToolSessionID(sessionID string) (messageID string, toolCallID string, ok bool)
	// SetCurrentSession reports the session this client is currently
	// viewing. Empty sessionID clears the entry (e.g. landing screen).
	// In single-client local mode this is a no-op. In client/server
	// mode it informs the server's per-client presence map so other
	// observers can compute attached-client counts per session.
	SetCurrentSession(ctx context.Context, sessionID string) error
	SetCurrentSessionGeneration(ctx context.Context, sessionID string, generation uint64) error

	ListMessages(ctx context.Context, sessionID string) ([]message.Message, error)
	ListMessagesBySessionIDs(ctx context.Context, rootSessionID string, generation uint64, sessionIDs []string) (map[string][]message.Message, error)
	ListUserMessages(ctx context.Context, sessionID string) ([]message.Message, error)
	ListAllUserMessages(ctx context.Context) ([]message.Message, error)
}

// AgentController runs and inspects agent turns: starting/cancelling runs,
// busy/ready probes, the queued-prompt list, and the model the coordinator
// is currently using.
type AgentController interface {
	// AgentRun accepts prompt as a fire-and-forget turn and returns once
	// it is accepted, not once the turn completes. ctx governs only
	// delivery of the request (e.g. the client/server HTTP call), not
	// the lifetime of the dispatched turn: cancelling it does not stop
	// an already-accepted run in either mode. The turn is owned by the
	// workspace/App and is stopped with AgentCancel instead.
	AgentRun(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) error
	AgentRunShellCommand(ctx context.Context, sessionID, command string, termWidth int, onProgress func(string), isFirstMessage bool) (proto.ShellCommandResponse, error)
	AgentCancel(sessionID string)
	AgentIsBusy() bool
	AgentIsSessionBusy(sessionID string) bool
	AgentModel() AgentModel
	AgentIsReady() bool
	// AgentReadyErr reports nil when the coder agent is ready to accept
	// work, or a descriptive error otherwise: ErrAgentNotInitialized
	// when the agent simply isn't set up, or ErrServerUnreachable
	// (wrapped) when the client could not reach the server to find out.
	// It lets the UI show an actionable message instead of collapsing
	// both cases into "agent offline".
	AgentReadyErr() error
	AgentQueuedPrompts(sessionID string) int
	AgentQueuedPromptsList(sessionID string) []string
	AgentClearQueue(sessionID string)
	AgentSummarize(ctx context.Context, sessionID string) error
	UpdateAgentModel(ctx context.Context) error
	InitCoderAgent(ctx context.Context) error
	InitCoderAgentNonInteractive(ctx context.Context) error
	// AgentRunStream sends prompt as a new turn on the already-resolved
	// sessionID (see ResolveSession) and streams the turn to
	// completion. The returned channel is closed after the terminal
	// AgentRunEvent (Done: true) is sent, or immediately alongside a
	// synchronous error if the turn could not be started at all (e.g.
	// agent not ready). The caller owns all presentation concerns
	// (spinner, progress bar) and must either drain the channel to
	// completion or cancel ctx to stop it early; cancelling ctx
	// delivers a terminal event derived from ctx.Err() unless the turn
	// already finished on its own.
	AgentRunStream(ctx context.Context, sessionID, prompt string) (<-chan AgentRunEvent, error)
}

// PermissionResolver resolves or inspects pending tool-permission requests.
//
// PermissionGrant, PermissionGrantPersistent, and PermissionDeny return
// true if the call resolved the pending request and false if it had
// already been resolved by another subscriber (or is no longer pending).
// A false return is not an error; the modal can still close locally
// because the resolution will arrive via the PermissionNotification event
// stream regardless of which client won the race.
type PermissionResolver interface {
	PermissionGrant(perm permission.PermissionRequest) bool
	PermissionGrantPersistent(perm permission.PermissionRequest) bool
	PermissionDeny(perm permission.PermissionRequest) bool
	PermissionSkipRequests() bool
	PermissionSetSkipRequests(skip bool)
}

// QuestionResponder resolves or cancels a pending agent question.
type QuestionResponder interface {
	// QuestionAnswer resolves the pending question with responses.
	QuestionAnswer(responses []question.Answer) bool
	// QuestionCancel cancels the pending question.
	QuestionCancel() bool
}

// FileServices covers per-session file tracking (what's been read, when)
// and the on-disk edit history used to render diffs.
type FileServices interface {
	UncommittedFiles(ctx context.Context) ([]git.FileChange, error)

	FileTrackerRecordRead(ctx context.Context, sessionID, path string)
	FileTrackerLastReadTime(ctx context.Context, sessionID, path string) time.Time
	FileTrackerListReadFiles(ctx context.Context, sessionID string) ([]string, error)

	ListSessionHistory(ctx context.Context, sessionID string) ([]history.File, error)
}

// LSPController starts/stops LSP servers and reports their state and
// diagnostic counts.
type LSPController interface {
	LSPStart(ctx context.Context, path string)
	LSPStopAll(ctx context.Context)
	LSPGetStates() map[string]LSPClientInfo
	LSPGetDiagnosticCounts(name string) lsp.DiagnosticCounts
}

// ConfigAccessor reads the resolved configuration and applies mutations to
// it (proxied to the server in client mode).
type ConfigAccessor interface {
	Config() *config.Config
	WorkingDir() string
	Resolver() config.VariableResolver

	UpdatePreferredModel(scope config.Scope, model config.SelectedModel) error
	// OverridePreferredModel applies a preferred-model override for the
	// current process, for callers (namely `braid run -m/--model`) that
	// must not surprise the user with a config-file write from a
	// single invocation. In local mode this is purely in-memory (see
	// config.ConfigStore.OverridePreferredModel). Client/server mode has
	// no equivalent ephemeral primitive on the server, so it falls back
	// to a persisted UpdatePreferredModel at ScopeWorkspace — matching
	// braid run -m's pre-existing behavior in that mode.
	OverridePreferredModel(model config.SelectedModel) error
	SetCompactMode(scope config.Scope, enabled bool) error
	SetProviderAPIKey(scope config.Scope, providerID string, apiKey any) error
	SetConfigField(scope config.Scope, key string, value any) error
	RemoveConfigField(scope config.Scope, key string) error
	ImportCopilot() (*oauth.Token, bool)
	RefreshOAuthToken(ctx context.Context, scope config.Scope, providerID string) error
}

// ProjectLifecycle covers first-run project initialization and skill
// discovery/reads.
type ProjectLifecycle interface {
	ProjectNeedsInitialization() (bool, error)
	MarkProjectInitialized() error
	InitializePrompt() (string, error)
	ListSkills(ctx context.Context) ([]skills.CatalogEntry, error)
	ReadSkill(ctx context.Context, skillID string) ([]byte, skills.SkillReadResult, error)
}

// MCPController manages MCP server connections and their tools, prompts,
// and resources (server-side in client mode).
type MCPController interface {
	// WaitForMCPInit blocks until this workspace's MCP servers have
	// finished their initial connection attempt (or ctx is done). Used by
	// the UI to avoid reporting "no pending auth" before slow servers have
	// had a chance to reach StateNeedsAuth.
	WaitForMCPInit(ctx context.Context) error
	MCPGetStates() map[string]mcptools.ClientInfo
	// MCPResources returns the cached resource catalog across all
	// connected MCP servers, e.g. for completion popups.
	MCPResources() []MCPResourceInfo
	MCPRefreshPrompts(ctx context.Context, name string)
	MCPRefreshResources(ctx context.Context, name string)
	RefreshMCPTools(ctx context.Context, name string)
	ReadMCPResource(ctx context.Context, name, uri string) ([]MCPResourceContents, error)
	ListMCPPrompts(ctx context.Context) ([]commands.MCPPrompt, error)
	GetMCPPrompt(clientID, promptID string, args map[string]string) (string, error)
	EnableDockerMCP(ctx context.Context) error
	DisableDockerMCP() error
	MCPAuthenticate(ctx context.Context, name string) error
	MCPPendingAuth() []mcptools.PendingAuthServer
	MCPAuthURL(name string) string
}

// ThreadController manages a workspace's threads: parallel agent work
// streams, each in its own git worktree/branch (see internal/thread).
// SupportsThreads reports whether the workspace owns a thread manager at
// all (false for non-git workspaces and thread workspaces themselves —
// nesting is not supported); every other method's behavior when it's
// false is implementation-defined (both implementations return a clear
// error rather than panicking — see each method's doc below).
// ThreadCapabilityInitializer is an optional extension for frontends that
// need a definitive answer before checking SupportsThreads. Implementations
// may resolve an older server's unknown capability snapshot using ctx.
type ThreadCapabilityInitializer interface {
	InitializeThreadsCapability(ctx context.Context) error
}

// InitializeThreadsCapability resolves an optional workspace capability when
// supported. Existing Workspace implementations and test stubs remain valid.
func InitializeThreadsCapability(ctx context.Context, ws ThreadController) error {
	if initializer, ok := ws.(ThreadCapabilityInitializer); ok {
		return initializer.InitializeThreadsCapability(ctx)
	}
	return nil
}

type ThreadController interface {
	SupportsThreads() bool
	ListThreads(ctx context.Context) ([]proto.Thread, error)
	GetThread(ctx context.Context, id string) (proto.Thread, error)
	CreateThread(ctx context.Context, req proto.CreateThreadRequest) (proto.Thread, error)
	SendThread(ctx context.Context, id, message string) error
	// ActivateThread respawns id's isolated workspace without dispatching
	// an agent run, so a thread whose run has finished can be attached to
	// and worked in by hand instead of only viewed read-only.
	ActivateThread(ctx context.Context, id string) (proto.Thread, error)
	MergeThread(ctx context.Context, id string) (proto.Thread, error)
	// CancelThread stops id's in-flight run and rests it at
	// StatusCancelled, leaving its worktree and branch on disk — unlike
	// RemoveThread, which tears everything down. Mirrors TaskController's
	// CancelTask; see internal/thread.Manager.Cancel for the refusal a
	// thread mid-merge gets that a task never can.
	CancelThread(ctx context.Context, id, reason string) error
	RemoveThread(ctx context.Context, id string, opts proto.RemoveThreadOptions) error
	// AttachThread connects to id's own spawned workspace and returns a
	// Workspace bound to it, plus a detach func to release that
	// connection (NOT the thread itself — the thread keeps running
	// regardless of whether anything is attached to view it). Callers
	// must call detach exactly once when done.
	AttachThread(ctx context.Context, id string) (Workspace, func(), error)
}

// TaskController manages a workspace's tasks: the worktree-less background
// delegation kind, sibling to threads (see internal/thread.TaskManager).
// Tasks share threads' wire representation (proto.Thread, discriminated by
// Kind) but have no workspace of their own — proto.Thread.WorkspaceID is
// always "" for a task, since a task runs inside its parent workspace's own
// App rather than spawning an isolated one. SupportsTasks reports whether
// the workspace owns a task manager at all; every other method's behavior
// when it's false is implementation-defined (both implementations return
// ErrTasksNotSupported rather than panicking).
type TaskController interface {
	SupportsTasks() bool
	ListTasks(ctx context.Context) ([]proto.Thread, error)
	CancelTask(ctx context.Context, id, reason string) error
}

// EventSubscriber wires a frontend into the workspace's event stream and
// tears it down.
type BackgroundJobs interface {
	BackgroundJobCounts() shell.BackgroundJobCounts
}

type EventSubscriber interface {
	Subscribe(program *tea.Program)
	Shutdown()
}

// Workspace is the main abstraction consumed by the TUI and CLI. It
// groups every operation a frontend needs to perform against a running
// workspace, regardless of whether the workspace is in-process or
// remote. It is a composition of narrower role interfaces (SessionStore,
// AgentController, ...) so that consumers who only need one slice of it —
// test stubs, most dialogs — can depend on the narrow interface instead of
// all ~65 methods. Implementations are unaffected: this is purely a
// grouping of the same method set.
type Workspace interface {
	SessionStore
	AgentController
	PermissionResolver
	QuestionResponder
	FileServices
	LSPController
	ConfigAccessor
	ProjectLifecycle
	MCPController
	ThreadController
	TaskController
	BackgroundJobs
	EventSubscriber
}

// AgentRunEvent is one increment of a non-interactive agent turn
// driven through Workspace.AgentRunStream. TextDelta carries new,
// already de-duplicated/reconciled assistant text to append verbatim
// to stdout — callers must not track byte offsets themselves. Done is
// set on the terminal event (success, cancellation, or failure); Err
// is non-nil only on failure. A clean cancellation reports Done=true,
// Err=nil when the turn itself reports a clean cancellation (e.g. the
// server-side run was cancelled); a caller-driven ctx cancellation
// still surfaces ctx.Err(), matching the pre-refactor behavior of
// `braid run`'s select-on-ctx.Done() branch.
type AgentRunEvent struct {
	TextDelta string
	Done      bool
	Err       error
}

// MCPResourceContents holds the contents of an MCP resource.
type MCPResourceContents struct {
	URI      string `json:"uri"`
	MIMEType string `json:"mime_type,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     []byte `json:"blob,omitempty"`
}

// MCPResourceInfo describes one resource advertised by an MCP server, as
// returned by MCPResources' catalog listing.
type MCPResourceInfo struct {
	MCPName  string
	URI      string
	Title    string
	MIMEType string
}
