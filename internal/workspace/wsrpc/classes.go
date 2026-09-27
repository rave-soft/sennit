// Package wsrpc holds the client/server plumbing for serving
// workspace.Workspace over the wire (CLIENT-SERVER.md). This step (PR 0.7)
// builds the method-class table, a generator that reads it, and a loopback
// decorator that pushes every unary call through the JSON codec in-process,
// so any type or error that would break on the wire breaks in tests first —
// no gRPC yet.
//
// wsrpc may import internal/workspace (for the Workspace interface and its
// DTOs), but must never import internal/app, internal/agent, or
// internal/db — see dependency_guard_test.go in this package. The UI's own
// TestUIGoroutineGuard (internal/ui/model/wsguard_test.go) derives its
// guarded-method list from MethodClasses here, so internal/ui/model links
// this package too; the same import ban applies transitively through it.
package wsrpc

// Class names the wire treatment a Workspace method gets once the
// client/server split lands (CLIENT-SERVER.md, "Классы методов").
type Class string

const (
	// U is an ordinary request/response call: every parameter (besides
	// context.Context) and result (besides a trailing error) crosses the
	// wire as JSON. The generator (gen/genlib) emits a Request/Response
	// pair and a Loopback method for every U (and C) method.
	U Class = "U"
	// C is a getter the UI calls straight from Update/View today. Its
	// Request/Response DTOs and Loopback method are still generated
	// exactly like U's (Loopback round-trips it through JSON in-process),
	// but it gets no gRPC RPC and no generated Client method: grpcws.Client
	// answers a C getter from its own local cache instead, kept current by
	// the workspace.ClientState event (CLIENT-SERVER.md, PR 1.4).
	C Class = "C"
	// S is a method a later PR rewrites by hand into a server-stream RPC
	// (Subscribe, AgentRunStream, ...). The generator emits nothing for
	// it; Loopback gets a hand-written pass-through in loopback_manual.go.
	S Class = "S"
	// H returns or consumes a handle (another Workspace, an OAuthFlow)
	// rather than plain data. Like S, the generator emits nothing;
	// Loopback's implementation is hand-written.
	H Class = "H"
	// X never crosses the wire at all (Shutdown becomes a client-side
	// connection teardown). The generator emits nothing; Loopback still
	// needs a pass-through so it satisfies workspace.Workspace.
	X Class = "X"
)

// MethodClasses maps every Workspace method to its class, taken from the
// method-class table in CLIENT-SERVER.md ("Классы методов"). Assignments
// marked "U!" in that table are already "U" here: PR 0.1 (commits
// 287299dbb, b7e39af67, caf930146) already added the error/ctx signatures
// that marking called for, so the "!" qualifier has nothing left to track.
// Methods the table lists as leaving the contract (ResetAgentToolCache,
// Resolver) are simply absent from both this map and Workspace, so they
// need no entry here.
//
// Moved here from internal/workspace/wire_classes_test.go (a _test.go-only
// table that neither the generator nor internal/ui/model could import) —
// see this package's own classes_test.go for the completeness check, and
// internal/workspace/wire_classes_ui_guard_test.go for the check that keeps
// the UI's guard wrappers matching it.
var MethodClasses = map[string]Class{
	// SessionStore.
	"CreateSession":               U,
	"GetSession":                  U,
	"ListSessions":                U,
	"GetLastSession":              U,
	"RenameSession":               U,
	"DeleteSession":               U,
	"SetCurrentSession":           U,
	"SetCurrentSessionGeneration": U,
	"SessionDescendantCost":       U,
	"ListMessages":                U,
	"ListMessagesBySessionIDs":    U,
	"ListUserMessages":            U,
	"ListAllUserMessages":         U,

	// AgentController.
	"AgentRun":                     U,
	"AgentRunShellCommand":         S,
	"AgentRunStream":               S,
	"AgentCancel":                  U,
	"AgentIsBusy":                  C,
	"AgentIsSessionBusy":           C,
	"AgentModel":                   C,
	"AgentIsReady":                 C,
	"AgentReadyErr":                C,
	"AgentQueuedPromptsList":       C,
	"AgentActivity":                C,
	"AgentClearQueue":              U,
	"AgentSummarize":               U,
	"UpdateAgentModel":             U,
	"ApplySessionModel":            U,
	"InitCoderAgent":               U,
	"InitCoderAgentNonInteractive": U,

	// UsageReporter.
	"Stats": U,

	// PermissionResolver.
	"PermissionGrant":           U,
	"PermissionGrantPersistent": U,
	"PermissionDeny":            U,
	"PermissionSkipRequests":    C,
	"PermissionSetSkipRequests": U,

	// QuestionResponder.
	"QuestionAnswer": U,
	"QuestionCancel": U,

	// PendingPromptsReader.
	"PendingPrompts": U,

	// FileServices.
	"UncommittedFiles":         U,
	"FileTrackerRecordRead":    U,
	"FileTrackerLastReadTime":  U,
	"FileTrackerListReadFiles": U,
	"ListSessionHistory":       U,
	"ListProjectFiles":         U,
	"AttachProjectFile":        U,
	// PrepareSessionChanges is U, not C: unlike the LSPController getters
	// below, nothing calls it synchronously from Update/View -- it only
	// ever runs inside a tea.Cmd (session.go's refreshModifiedFiles /
	// sessionLoadResolver.resolve), the same as any other on-demand U
	// call, so there is no caching requirement to flag with C.
	"PrepareSessionChanges": U,

	// LSPController. Already called from a tea.Cmd, so the table keeps
	// these "U" rather than "C" even though they read cached state.
	"LSPStart":               U,
	"LSPStopAll":             U,
	"LSPGetStates":           U,
	"LSPGetDiagnosticCounts": U,

	// ConfigReader / WorkingDirectory.
	"Config":     C,
	"WorkingDir": C,

	// ConfigFieldEditor.
	"SetConfigField":    U,
	"RemoveConfigField": U,

	// Accounts.
	"RecordAccount":        U,
	"ListAccounts":         U,
	"ActivateAccount":      U,
	"UpdateAccountFields":  U,
	"RemoveAccount":        U,
	"SetProviderProxy":     U,
	"PurgeAccounts":        U,
	"RefreshAccountLimits": U,
	"CurrentPlanUsage":     C,
	"AccountCapabilities":  C,

	// ModelsRefresher / PreferredModelUpdater / ProviderAPIKeySetter /
	// CustomProviderConfigurer.
	"RefreshProviderModels":   U,
	"UpdatePreferredModel":    U,
	"OverridePreferredModel":  U,
	"SetProviderAPIKey":       U,
	"ConfigureCustomProvider": U,

	// ProviderCatalog.
	"VerifyProviderAPIKey": U,
	"KnownProviders":       C,
	"CustomProviderTypes":  C,

	// OAuthController.
	"StartOAuth":                   H,
	"OAuthConfiguredProxy":         U,
	"OAuthProviderConfiguredProxy": U,
	"OAuthValidateProxy":           U,
	"ImportCopilot":                U,
	"RefreshOAuthToken":            U,
	"RefreshOAuthTokenForAccount":  U,

	// ProjectLifecycle.
	"ProjectNeedsInitialization": U,
	"MarkProjectInitialized":     U,
	"InitializePrompt":           U,
	"ListSkills":                 U,
	"ReadSkill":                  U,
	"ConfigProblems":             U,
	"SkillStates":                U,
	"BuiltinSkills":              U,
	"DoctorProblems":             U,
	"ListCustomCommands":         U,

	// MCPController.
	"WaitForMCPInit":               U,
	"MCPGetStates":                 U,
	"MCPResources":                 U,
	"MCPRefreshPrompts":            U,
	"MCPRefreshResources":          U,
	"RefreshMCPTools":              U,
	"ReadMCPResource":              U,
	"ListMCPPrompts":               U,
	"GetMCPPrompt":                 U,
	"EnableDockerMCP":              U,
	"DisableDockerMCP":             U,
	"DockerMCPAvailable":           C,
	"RefreshDockerMCPAvailability": U,
	"MCPAuthenticate":              U,
	"MCPPendingAuth":               C,
	"MCPAuthURL":                   C,

	// WorktreeController.
	"EnterWorktree":  H,
	"ExitWorktree":   H,
	"ResumeWorktree": H,
	// WorktreeState is C: the command palette (internal/ui/dialog/
	// commands.go) calls it directly while building its item list, the
	// same way the LSPController/MCP getters above are read straight
	// from a dialog constructor.
	"WorktreeState": C,

	// ThreadController.
	"ListThreads":     U,
	"CreateThread":    U,
	"ActivateThread":  U,
	"CancelThread":    U,
	"RemoveThread":    U,
	"SupportsThreads": C,
	"AttachThread":    H,

	// TaskController.
	"ListTasks":     U,
	"CancelTask":    U,
	"SupportsTasks": C,

	// BackgroundJobs.
	"BackgroundJobCounts": C,

	// EventSubscriber.
	"Subscribe":     S,
	"SubscribeWith": S,
	"Shutdown":      X,
}
