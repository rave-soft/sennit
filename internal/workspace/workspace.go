// Package workspace defines the Workspace interface and DTOs used by all
// frontends (TUI, CLI) to interact with a running workspace. This is the
// contract only: it must never import internal/db, internal/app,
// internal/agent, or internal/thread (see dependency_guard_test.go),
// because internal/ui imports this package for the interface and would
// otherwise drag in the whole backend runtime transitively. The concrete
// implementation wrapping an in-process app.App lives in
// internal/workspace/appws ([appws.AppWorkspace]); [readOnlyWorkspace],
// which wraps another Workspace to restrict it to read-only operations,
// stays here since it depends on nothing but this package's own contract.
package workspace

import (
	"context"
	"errors"
	"time"

	"charm.land/catwalk/pkg/catwalk"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/git"
	"github.com/rave-soft/sennit/internal/history"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/stats"
)

// Reasons the coder agent may be unavailable, returned by
// Workspace.AgentReadyErr so callers can tell a genuinely
// uninitialized agent apart from a lost server connection.
type ProviderQuotaInfo struct {
	Model       string
	SettingsURL string
}

// providerQuotaError is the shape [agent.ProviderQuotaError] exposes via
// its QuotaInfo method. Matching on this interface (errors.As accepts a
// pointer to an interface type, not just a concrete type) lets
// GetProviderQuotaInfo recognize the error without importing
// internal/agent for its concrete type.
type providerQuotaError interface {
	error
	QuotaInfo() (model, settingsURL string)
}

func GetProviderQuotaInfo(err error) (ProviderQuotaInfo, bool) {
	var quotaErr providerQuotaError
	if !errors.As(err, &quotaErr) {
		return ProviderQuotaInfo{}, false
	}
	model, settingsURL := quotaErr.QuotaInfo()
	return ProviderQuotaInfo{Model: model, SettingsURL: settingsURL}, true
}

var (
	// ErrAgentNotInitialized means the workspace exists but its coder
	// agent has not been configured/initialized (e.g. no model set).
	ErrAgentNotInitialized = errors.New("coder agent is not initialized")
	// ErrServerUnreachable means the client could not reach the server
	// to determine the agent's status (server down, or the workspace was
	// torn down out from under the client).
	ErrServerUnreachable = errors.New("lost connection to the sennit server")
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

type AgentNotificationType string

const (
	AgentNotificationFinished       AgentNotificationType = "agent_finished"
	AgentNotificationReAuthenticate AgentNotificationType = "re_authenticate"
	AgentNotificationError          AgentNotificationType = "error"
	AgentNotificationAWSSSOAuth     AgentNotificationType = "aws_sso_auth"
	AgentNotificationAWSSSOResult   AgentNotificationType = "aws_sso_auth_result"
	AgentNotificationQueueChanged   AgentNotificationType = "queue_changed"
	// AgentNotificationAccountRotated and AgentNotificationAccountRotationExhausted
	// mirror notify.TypeAccountRotated / notify.TypeAccountRotationExhausted
	// (internal/agent/notify) - see those for what triggers each.
	AgentNotificationAccountRotated           AgentNotificationType = "account_rotated"
	AgentNotificationAccountRotationExhausted AgentNotificationType = "account_rotation_exhausted"
)

type AgentNotification struct {
	SessionID    string
	SessionTitle string
	Type         AgentNotificationType
	ProviderID   string
	RunID        string
	Message      string
	AWSSOCommand string
	AWSSOURL     string
}

// LSPClientInfo is the frontend-facing LSP client state.
//
// It used to alias internal/lsp.ClientInfo, which carries a live *Client:
// every consumer of this contract was handed the running LSP client along
// with its name and state. The transport shape carries no handle.
type LSPClientInfo = proto.LSPClientInfo

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
	State           proto.LSPState
	Error           error
	DiagnosticCount int
}

// AgentCatalog contains the catalog properties the UI renders.
type AgentCatalog struct {
	ID              string
	Name            string
	CanReason       bool
	ReasoningLevels []string
	ContextWindow   int64
}

// AgentSelection contains the configured properties the UI renders.
type AgentSelection struct {
	Provider        string
	Model           string
	Think           bool
	ReasoningEffort string
}

// AgentModel holds the model information exposed to the UI.
type AgentModel struct {
	CatalogCfg AgentCatalog
	ModelCfg   AgentSelection
}

// SessionStore covers session CRUD and message reads: everything the
// sessions dialog, chat loading, and history/summarization code need
// without pulling in the rest of Workspace.
type SessionStore interface {
	CreateSession(ctx context.Context, title string) (session.Session, error)
	GetSession(ctx context.Context, sessionID string) (session.Session, error)
	ListSessions(ctx context.Context) ([]session.Session, error)
	// GetLastSession returns the most recently updated top-level session
	// for this workspace's project, scoped the same way ListSessions is
	// (no child or agent-tool sessions). It reports an error when there
	// is none — see [ResolveSession]'s useLast branch, its only caller.
	GetLastSession(ctx context.Context) (session.Session, error)
	// RenameSession writes only sessionID's title. There is deliberately
	// no whole-row counterpart on this contract: a caller that only wants
	// to change the title (e.g. the sessions dialog, which never
	// subscribes to session updates) would otherwise write back its whole
	// stale snapshot, clobbering cost, todos and summary_message_id that
	// other writers (usage saves, the todo tool, auto-summarization)
	// changed while the snapshot was sitting in the UI. Whoever needs to
	// write another field adds a narrow method for that field rather than
	// bringing the row write back.
	RenameSession(ctx context.Context, sessionID string, title string) error
	DeleteSession(ctx context.Context, sessionID string) error
	// SetCurrentSession reports the session this client is currently
	// viewing. Empty sessionID clears the entry (e.g. landing screen).
	// For AppWorkspace this is a no-op.
	SetCurrentSession(ctx context.Context, sessionID string) error
	SetCurrentSessionGeneration(ctx context.Context, sessionID string, generation uint64) error
	// SessionDescendantCost sums cost over every session nested under
	// sessionID, at any depth, excluding sessionID's own row. A session
	// with no delegations legitimately sums to 0.
	SessionDescendantCost(ctx context.Context, sessionID string) (float64, error)

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
	// delivery of the request, not the lifetime of the dispatched turn:
	// cancelling it does not stop an already-accepted run. The turn is
	// owned by the workspace/App and is stopped with AgentCancel instead.
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
	AgentQueuedPromptsList(sessionID string) []string
	AgentClearQueue(sessionID string)
	AgentSummarize(ctx context.Context, sessionID string) error
	UpdateAgentModel(ctx context.Context) error
	// ApplySessionModel switches this instance onto the model sessionID is
	// pinned to (see session.Session.Model), so opening an older session
	// resumes it on the model it was working with instead of whatever is
	// selected now. It reports whether it actually switched.
	//
	// The switch is in-memory only: it never writes the user's config
	// file. Opening a session is not the same act as choosing a default,
	// and several instances share that file — see
	// config.ConfigStore.OverridePreferredModel.
	//
	// Not switching is an ordinary outcome, reported as (false, nil): the
	// session may have no pinned model (it never ran, or predates the
	// pin), may already be on it, or may name a model this instance
	// cannot reach — a provider since removed, or one that was never
	// configured here. The last case is the reason this cannot simply
	// trust the stored value: the pin outlives the configuration that
	// made it valid, and a session must still open on a machine that has
	// since dropped the provider.
	ApplySessionModel(ctx context.Context, sessionID string) (bool, error)
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
	//
	// If opts.AutoApprovePermissions is set, every permission request the
	// turn raises on sessionID is granted without asking, for the rest of
	// the session's lifetime (see permission.AutoApproveSession) — there
	// is no way to later require prompting again on that session. A
	// delegation started from sessionID (the agent tool, a named agent,
	// agentic fetch, or a chain of those nested any number of levels
	// deep) inherits the same grant on its own child session, since the
	// child's requests would otherwise block forever with nothing able to
	// answer them (see thread.TaskManager.Create). This exists for
	// headless callers (see cmd/run.go) that have no UI to answer a
	// prompt with; anything that can show one should leave it false and
	// let permission requests surface normally.
	AgentRunStream(ctx context.Context, sessionID, prompt string, opts AgentRunOptions) (<-chan AgentRunEvent, error)
	// ResetAgentToolCache clears process-wide caches the agent's built-in
	// tools keep (e.g. compiled grep/glob regexes), so a fresh session
	// does not inherit state left over from a previous one. It is a
	// method on the interface, rather than a free function reaching into
	// internal/agent/tools directly, so the contract package never needs
	// to import that package (see app_workspace_agent.go's
	// implementation in internal/workspace/appws).
	ResetAgentToolCache()
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
	QuestionAnswer(batchID string, responses []question.Answer) bool
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
	LSPGetDiagnosticCounts(name string) proto.LSPDiagnosticCounts
}

type ConfigReader interface {
	Config() *config.Config
}

// WorkingDirectory reports the workspace path used to scope operations to the
// current project.
type WorkingDirectory interface {
	WorkingDir() string
}

type ConfigFieldEditor interface {
	SetConfigField(scope config.Scope, key string, value any) error
	RemoveConfigField(scope config.Scope, key string) error
	// SetCompactMode sets whether compact mode is enabled at scope.
	SetCompactMode(scope config.Scope, enabled bool) error
}

type AccountRecorder interface {
	RecordAccount(scope config.Scope, providerID string, cred accounts.LegacyCredential) (accounts.Account, error)
}

type AccountLister interface {
	ListAccounts(providerID string) ([]accounts.Account, error)
}

type AccountActivator interface {
	ActivateAccount(scope config.Scope, providerID, accountID string) error
}

type AccountUpdater interface {
	UpdateAccount(providerID string, account accounts.Account) error
}

type AccountRemover interface {
	RemoveAccount(scope config.Scope, providerID, accountID string) error
}

type ProviderProxySetter interface {
	SetProviderProxy(providerID, proxy string) error
}

type AccountsPurger interface {
	PurgeAccounts(scope config.Scope, providerID string) error
}

// AccountUsage reports on the accounts that already exist, as opposed to
// the AccountRecorder/AccountLister/... roles above, which manage which
// accounts exist in the first place.
type AccountUsage interface {
	// RefreshAccountLimits fetches a fresh rate-limit snapshot for every
	// OAuth account of providerID that reports usage
	// (accounts.CapabilitiesOf(providerID).Usage) and persists what was
	// learned, returning the provider's accounts. A single account's
	// fetch failing does not fail the call — see config.RefreshAccountLimits
	// for the full contract.
	RefreshAccountLimits(ctx context.Context, providerID string) ([]accounts.Account, error)
	// CurrentPlanUsage reports the rate-limit snapshot the provider quoted
	// on its most recent response, and whether there is one. It is not the
	// stored per-account snapshot RefreshAccountLimits persists: this one
	// is whatever the last request came back with, which is what the
	// sidebar shows while a turn is running.
	//
	// Empty for every provider that does not quote usage, and empty for
	// one that does until it has answered once. It is on the facade
	// because the numbers live in the vendor package that also carries
	// that vendor's sign-in flow, and the UI has no business importing
	// that to read a percentage.
	CurrentPlanUsage(providerID string) (accounts.Usage, bool)
	// AccountCapabilities reports what the provider's accounts support —
	// whether it quotes a usage snapshot, and when it rotates — so the
	// settings dialog can render only the fields that apply.
	AccountCapabilities(providerID string) AccountCapabilities
}

type ConfigResolver interface {
	Resolver() config.VariableResolver
}

type PreferredModelUpdater interface {
	UpdatePreferredModel(scope config.Scope, model config.SelectedModel) error
	// OverridePreferredModel applies a preferred-model override for the
	// current process, for callers (namely `sennit run -m/--model`) that
	// must not surprise the user with a config-file write from a single
	// invocation. In local mode this is purely in-memory (see
	// config.ConfigStore.OverridePreferredModel).
	OverridePreferredModel(model config.SelectedModel) error
}

type ProviderAPIKeySetter interface {
	SetProviderAPIKey(scope config.Scope, providerID string, apiKey any) error
}

// ProviderCatalog answers what providers this workspace knows about and
// whether a given API key is good for one, independent of whether an
// account is actually recorded for it.
type ProviderCatalog interface {
	// VerifyProviderAPIKey tests apiKey against providerID by building the
	// same kind of runtime provider the agent itself would use for that
	// provider (proxy, extra headers, account rotation, and a base URL
	// resolved from the known-providers catalog when the provider isn't
	// configured yet) and probing it, rather than a caller-assembled
	// stand-in. It returns nil when the key checks out and a descriptive
	// error otherwise; it does not persist apiKey — SetProviderAPIKey does
	// that once the caller is satisfied.
	VerifyProviderAPIKey(ctx context.Context, providerID, apiKey string) error
	// KnownProviders is the provider catalog this workspace was built
	// with: the embedded list plus Codex, or nothing at all when
	// disable_default_providers is set.
	//
	// It is the store's cached copy, which is the same list model
	// resolution and credential setup use. The UI used to recompute it
	// from the config on every call — seven call sites, some on a render
	// path — which rebuilt the embedded catalog each time and, worse, was
	// a second answer to a question the store already answers. A reload
	// recomputes the store's copy; a recomputation here could disagree
	// with the one the agent is actually using.
	KnownProviders() []catwalk.Provider
	// CustomProviderTypes lists the provider types a custom provider may
	// declare beyond catwalk's own catalog — the ones this build has a
	// discovery enricher registered for.
	//
	// It is on the facade because the list is derived from that registry,
	// not written down: a hardcoded copy in the form would drift the
	// moment an enricher is added or removed, and importing the discovery
	// engine into a dialog to read five strings is the trade this
	// boundary exists to refuse.
	CustomProviderTypes() []string
}

// OAuthStartResult is what a caller must show the user to complete a
// provider's sign-in, or the token already won without needing to.
type OAuthStartResult struct {
	// AuthorizationURL is set for a redirect flow (e.g. Codex): the sole
	// URL to open. Empty for a device flow.
	AuthorizationURL string

	// DeviceCode, UserCode, VerificationURL, Interval are set for a device
	// flow (e.g. Copilot); DeviceCode is empty for a redirect flow.
	DeviceCode      string
	UserCode        string
	VerificationURL string
	Interval        int

	// ExpiresIn bounds how long the above stays valid, in seconds.
	ExpiresIn int

	// Token is set when sign-in already completed with nothing to show —
	// Codex found a Codex CLI login on disk it could reuse or refresh.
	// Every field above is zero when this is set, and the accompanying
	// OAuthFlow is nil.
	Token *oauth.Token

	// ReusedExistingLogin/RefreshedExistingLogin/ExistingLoginFailure
	// narrate how Token came to be set, or why a login found on disk was
	// abandoned in favor of an interactive flow — purely for a CLI's
	// console narration (see internal/cmd/login_codex.go's existing
	// printfs); a UI ignores them.
	ReusedExistingLogin    bool
	RefreshedExistingLogin bool
	ExistingLoginFailure   string
}

// OAuthFlow is a started sign-in awaiting completion.
type OAuthFlow interface {
	// Wait blocks until the provider completes the flow or ctx is done.
	Wait(ctx context.Context) (*oauth.Token, error)
	// Cancel releases whatever resource the flow holds (a loopback
	// listener, an in-flight poll). Must be called exactly once when the
	// flow is no longer needed, whether or not Wait was called.
	Cancel()
}

// OAuthCompletion is what CompleteOAuth returns once the credential and
// any provider-specific follow-up have been persisted.
type OAuthCompletion struct {
	Account accounts.Account
	// ModelsFetched is the number of models fetched for a provider whose
	// catalog is per-account (Codex); -1 for a provider with nothing to
	// fetch.
	ModelsFetched int
	// ModelsError is set when fetching/saving the model list failed. The
	// credential is already saved when this is set — callers decide for
	// themselves whether that makes the overall sign-in a failure (the
	// TUI dialog does; the CLI does not, see loginCodex's existing
	// behavior on model-fetch failure).
	ModelsError error
	// ProxyError is set when the proxy this sign-in used could not be
	// persisted as the provider's default. The credential is already
	// saved when this is set, and the model fetch (if any) still runs
	// with the proxy that was actually used, not the one that failed to
	// save — only the provider's stored default is missing. As with
	// ModelsError, callers decide whether that makes the sign-in a
	// failure; unlike ModelsError, this is never set for a sign-in whose
	// proxy already matched what was configured, since nothing is
	// written in that case.
	ProxyError error
}

// OAuthController starts a provider's OAuth sign-in flow and finishes it
// once a token is won, doing whatever post-save work that provider needs
// (recording the model list, persisting the proxy that was used) on the
// backend side instead of in a frontend.
type OAuthController interface {
	// StartOAuth begins providerID's sign-in flow using proxyURL ("" for
	// none).
	StartOAuth(ctx context.Context, providerID, proxyURL string) (OAuthStartResult, OAuthFlow, error)
	// CompleteOAuth persists token as a new/updated account of providerID
	// (scope is always global, matching RecordAccount) and performs
	// whatever the provider needs done afterward.
	CompleteOAuth(ctx context.Context, providerID, proxyURL string, token *oauth.Token, forceNewAccount bool) (OAuthCompletion, error)
	// OAuthConfiguredProxy is the proxy providerID already uses: whatever
	// Sennit has configured for it, falling back to a sibling CLI's own
	// on-disk config for a provider that has one (Codex).
	OAuthConfiguredProxy(providerID string) string
	// OAuthValidateProxy checks proxyURL is well-formed for providerID.
	OAuthValidateProxy(providerID, proxyURL string) error
	// ImportCopilot imports the credentials of an existing GitHub Copilot
	// CLI login, if one is present on this machine, for use as this
	// workspace's Copilot provider token.
	ImportCopilot() (*oauth.Token, bool)
	// RefreshOAuthToken refreshes providerID's stored OAuth token at scope.
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
	// ConfigProblems runs the config diagnostic and returns what it found.
	// It answers from the config alone — see DoctorProblems below for the
	// merged list that also covers the machine and discovery.
	ConfigProblems() []config.Problem
	// SkillStates is the outcome of the last skill discovery: what loaded
	// and what failed to.
	SkillStates() []*skills.SkillState
	// BuiltinSkills are the skills shipped with the binary.
	BuiltinSkills() []*skills.Skill
	// DoctorProblems is the full config.Problem list the /doctor dialog
	// shows: ConfigProblems' static findings, an environment probe (e.g.
	// missing clipboard helpers — machine state, not config), SkillStates
	// run through internal/doctor's validation, and any MCP server stuck
	// in an error/needs-auth state. The environment probe shells out and
	// walks PATH, so this call can block; callers on the UI thread must
	// run it from a tea.Cmd rather than a dialog constructor. Mirrors
	// sennit_info's own [problems] section (the agent-tool side of the
	// same merge).
	DoctorProblems() []config.Problem
	// ListCustomCommands returns everything the command palette offers
	// under "user commands": the markdown commands found on disk, plus
	// the user-invocable skills, already merged.
	//
	// The merge is here rather than in the caller because both halves are
	// discovery — walking the config directories, then the skill catalog —
	// and the palette's job is to render a list, not to know that a
	// command can come from two places or how either is found.
	ListCustomCommands(ctx context.Context) ([]CustomCommand, error)
}

// MCPController manages MCP server connections and their tools, prompts,
// and resources (server-side in client mode).
type MCPEventType string

const (
	MCPEventStateChanged         MCPEventType = "state_changed"
	MCPEventToolsListChanged     MCPEventType = "tools_list_changed"
	MCPEventPromptsListChanged   MCPEventType = "prompts_list_changed"
	MCPEventResourcesListChanged MCPEventType = "resources_list_changed"
)

type MCPEvent struct {
	Type MCPEventType
	Name string
}

type MCPState = proto.MCPState

const (
	MCPStateDisabled  = proto.MCPStateDisabled
	MCPStateStarting  = proto.MCPStateStarting
	MCPStateConnected = proto.MCPStateConnected
	MCPStateError     = proto.MCPStateError
	MCPStateNeedsAuth = proto.MCPStateNeedsAuth
)

type MCPCounts struct {
	Tools     int
	Prompts   int
	Resources int
}

type MCPClientInfo struct {
	Name        string
	State       MCPState
	Error       error
	Counts      MCPCounts
	ConnectedAt time.Time
}

type MCPPendingAuthServer struct {
	Name string
	URL  string
}

type MCPController interface {
	// WaitForMCPInit blocks until this workspace's MCP servers have
	// finished their initial connection attempt (or ctx is done). Used by
	// the UI to avoid reporting "no pending auth" before slow servers have
	// had a chance to reach StateNeedsAuth.
	WaitForMCPInit(ctx context.Context) error
	MCPGetStates() map[string]MCPClientInfo
	// MCPResources returns the cached resource catalog across all
	// connected MCP servers, e.g. for completion popups.
	MCPResources() []MCPResourceInfo
	MCPRefreshPrompts(ctx context.Context, name string)
	MCPRefreshResources(ctx context.Context, name string)
	RefreshMCPTools(ctx context.Context, name string)
	ReadMCPResource(ctx context.Context, name, uri string) ([]MCPResourceContents, error)
	ListMCPPrompts(ctx context.Context) ([]MCPPrompt, error)
	GetMCPPrompt(clientID, promptID string, args map[string]string) (string, error)
	EnableDockerMCP(ctx context.Context) error
	DisableDockerMCP() error
	// DockerMCPAvailable reports the cached answer to "is the Docker MCP
	// gateway usable here", and whether that answer is still fresh.
	DockerMCPAvailable() (available, known bool)
	// RefreshDockerMCPAvailability re-answers it and caches the result.
	//
	// Answering means running `docker mcp version` with a timeout, which
	// is why it is on the facade: the command palette used to spawn that
	// process itself, from a tea.Cmd, to decide whether to offer a menu
	// entry.
	RefreshDockerMCPAvailability() bool
	MCPAuthenticate(ctx context.Context, name string) error
	MCPPendingAuth() []MCPPendingAuthServer
	MCPAuthURL(name string) string
}

// ThreadController manages a workspace's threads: parallel agent work
// streams, each in its own git worktree/branch (see internal/thread).
// SupportsThreads reports whether the workspace owns a thread manager at
// all (false for non-git workspaces and thread workspaces themselves —
// nesting is not supported); every other method returns
// ErrThreadsNotSupported when it's false, rather than panicking — see each
// method's doc below. A read-only workspace refuses the mutating methods
// with its own error before capability is consulted at all.
type ThreadController interface {
	SupportsThreads() bool
	ListThreads(ctx context.Context) ([]proto.Thread, error)
	GetThread(ctx context.Context, id string) (proto.Thread, error)
	CreateThread(ctx context.Context, req proto.CreateThreadRequest) (proto.Thread, error)
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
	BackgroundJobCounts() BackgroundJobCounts
}

type EventSubscriber interface {
	Subscribe(send func(any))
	Shutdown()
}

// Workspace is the main abstraction consumed by the TUI and CLI. It
// groups every operation a frontend needs to perform against a running
// workspace, regardless of whether the workspace is in-process or
// remote. It is a composition of narrower role interfaces (SessionStore,
// AgentController, ...) so that consumers who only need one slice of it —
// test stubs, most dialogs — can depend on the narrow interface instead of
// all 110 methods. Implementations are unaffected: this is purely a
// grouping of the same method set.
//
// A hundred and ten is a lot, and the role interfaces are what keep that
// from being the number every consumer depends on. A new method belongs on
// the role it serves, not here — including a method that is, for now, the
// only one its role has: AccountRecorder, AccountLister, ProviderProxySetter
// and their one-method siblings below look collapsible, but each is already
// depended on individually (see internal/cmd/login.go, accounts.go,
// logout.go) by code that wants exactly that one capability and nothing
// else the account-management surface offers. Folding them back into one
// interface would force those call sites wider for no benefit; a one-method
// role earns its keep the moment something names it.
type FrontendWorkspace interface {
	SessionStore
	AgentController
	UsageReporter
	PermissionResolver
	QuestionResponder
	FileServices
	LSPController
	ConfigReader
	WorkingDirectory
	ConfigResolver
	ConfigFieldEditor
	AccountRecorder
	AccountLister
	AccountActivator
	AccountUpdater
	AccountRemover
	AccountsPurger
	AccountUsage
	OAuthController
	PreferredModelUpdater
	ProviderAPIKeySetter
	CustomProviderConfigurer
	ProviderCatalog
	ProviderProxySetter
	ProjectLifecycle
	MCPController
	ThreadController
	TaskController
	BackgroundJobs
}

type Workspace interface {
	FrontendWorkspace
	EventSubscriber
}

// UsageReporter reads back what has already been recorded — tokens,
// cost, wall time, and how background delegations ended — for the
// /stats screen. It is read-only by construction: there is no setter
// anywhere in this interface, since usage is written as a side effect of
// running agents, never by a caller asking about it.
type UsageReporter interface {
	// Stats aggregates recorded usage for req's scope. It returns an
	// error only when the underlying queries fail; a scope with nothing
	// in it yields an empty snapshot, not an error, so a fresh project
	// renders as "nothing recorded yet" rather than as a failure.
	Stats(ctx context.Context, req stats.Request) (stats.Snapshot, error)
}

// AgentRunOptions configures a single Workspace.AgentRunStream call.
type AgentRunOptions struct {
	// AutoApprovePermissions grants every permission request the turn
	// raises without asking. See AgentRunStream's doc comment for what
	// this costs and who should set it.
	AutoApprovePermissions bool
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
// `sennit run`'s select-on-ctx.Done() branch.
//
// Status names what the agent is currently doing (see
// [message.Working.Label]), and is emitted only when that wording
// changes. It carries no output — a caller that has no place to show it
// can ignore Status events entirely.
type AgentRunEvent struct {
	TextDelta string
	Status    string
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
