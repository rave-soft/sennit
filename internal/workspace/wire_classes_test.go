package workspace

import (
	"reflect"
	"testing"
)

// methodClass names the wire treatment a Workspace method gets once the
// client/server split lands (CLIENT-SERVER.md, "Классы методов"). It is
// the source of truth wireDTOTest (wire_dto_test.go) uses to decide which
// methods' parameter/result types must survive encoding/json.
type methodClass string

const (
	// classUnary is an ordinary request/response call: every parameter
	// (besides context.Context) and result (besides the trailing error)
	// crosses the wire as JSON.
	classUnary methodClass = "U"
	// classCachedGetter is a getter the UI calls straight from
	// Update/View today. It still returns data that must serialize -
	// PR 0.4 moves the call itself into a cache, not the requirement
	// that its result travels as JSON.
	classCachedGetter methodClass = "C"
	// classStream is a method whose shape a later PR rewrites by hand
	// into a server-stream RPC (Subscribe, AgentRunStream, ...). Its own
	// signature is not walked; the data types the stream will actually
	// carry are added to the collector by hand alongside the S/H set in
	// wire_dto_test.go.
	classStream methodClass = "S"
	// classHandle returns or consumes a handle (another Workspace, an
	// OAuthFlow) rather than plain data. Like classStream, its signature
	// is not walked; the data type(s) it hands back (e.g. proto.Thread
	// for AttachThread) are added by hand.
	classHandle methodClass = "H"
	// classClientLocal never crosses the wire at all (Shutdown becomes a
	// client-side connection teardown).
	classClientLocal methodClass = "X"
)

// methodClasses maps every Workspace method to its class, taken from the
// method-class table in CLIENT-SERVER.md ("Классы методов"). Assignments
// marked "U!" in that table are already "U" here: PR 0.1 (commits
// 287299dbb, b7e39af67, caf930146) already added the error/ctx signatures
// that marking called for, so the "!" qualifier has nothing left to track.
// Methods the table lists as leaving the contract (ResetAgentToolCache,
// Resolver) are simply absent from both this map and Workspace, so they
// need no entry here.
var methodClasses = map[string]methodClass{
	// SessionStore.
	"CreateSession":               classUnary,
	"GetSession":                  classUnary,
	"ListSessions":                classUnary,
	"GetLastSession":              classUnary,
	"RenameSession":               classUnary,
	"DeleteSession":               classUnary,
	"SetCurrentSession":           classUnary,
	"SetCurrentSessionGeneration": classUnary,
	"SessionDescendantCost":       classUnary,
	"ListMessages":                classUnary,
	"ListMessagesBySessionIDs":    classUnary,
	"ListUserMessages":            classUnary,
	"ListAllUserMessages":         classUnary,

	// AgentController.
	"AgentRun":                     classUnary,
	"AgentRunShellCommand":         classStream,
	"AgentRunStream":               classStream,
	"AgentCancel":                  classUnary,
	"AgentIsBusy":                  classCachedGetter,
	"AgentIsSessionBusy":           classCachedGetter,
	"AgentModel":                   classCachedGetter,
	"AgentIsReady":                 classCachedGetter,
	"AgentReadyErr":                classCachedGetter,
	"AgentQueuedPromptsList":       classCachedGetter,
	"AgentClearQueue":              classUnary,
	"AgentSummarize":               classUnary,
	"UpdateAgentModel":             classUnary,
	"ApplySessionModel":            classUnary,
	"InitCoderAgent":               classUnary,
	"InitCoderAgentNonInteractive": classUnary,

	// UsageReporter.
	"Stats": classUnary,

	// PermissionResolver.
	"PermissionGrant":           classUnary,
	"PermissionGrantPersistent": classUnary,
	"PermissionDeny":            classUnary,
	"PermissionSkipRequests":    classCachedGetter,
	"PermissionSetSkipRequests": classUnary,

	// QuestionResponder.
	"QuestionAnswer": classUnary,
	"QuestionCancel": classUnary,

	// FileServices.
	"UncommittedFiles":         classUnary,
	"FileTrackerRecordRead":    classUnary,
	"FileTrackerLastReadTime":  classUnary,
	"FileTrackerListReadFiles": classUnary,
	"ListSessionHistory":       classUnary,

	// LSPController. Already called from a tea.Cmd, so the table keeps
	// these "U" rather than "C" even though they read cached state.
	"LSPStart":               classUnary,
	"LSPStopAll":             classUnary,
	"LSPGetStates":           classUnary,
	"LSPGetDiagnosticCounts": classUnary,

	// ConfigReader / WorkingDirectory.
	"Config":     classCachedGetter,
	"WorkingDir": classCachedGetter,

	// ConfigFieldEditor.
	"SetConfigField":    classUnary,
	"RemoveConfigField": classUnary,

	// Accounts.
	"RecordAccount":        classUnary,
	"ListAccounts":         classUnary,
	"ActivateAccount":      classUnary,
	"UpdateAccountFields":  classUnary,
	"RemoveAccount":        classUnary,
	"SetProviderProxy":     classUnary,
	"PurgeAccounts":        classUnary,
	"RefreshAccountLimits": classUnary,
	"CurrentPlanUsage":     classCachedGetter,
	"AccountCapabilities":  classCachedGetter,

	// ModelsRefresher / PreferredModelUpdater / ProviderAPIKeySetter /
	// CustomProviderConfigurer.
	"RefreshProviderModels":   classUnary,
	"UpdatePreferredModel":    classUnary,
	"OverridePreferredModel":  classUnary,
	"SetProviderAPIKey":       classUnary,
	"ConfigureCustomProvider": classUnary,

	// ProviderCatalog.
	"VerifyProviderAPIKey": classUnary,
	"KnownProviders":       classCachedGetter,
	"CustomProviderTypes":  classCachedGetter,

	// OAuthController.
	"StartOAuth":                  classHandle,
	"CompleteOAuth":               classUnary,
	"OAuthConfiguredProxy":        classUnary,
	"OAuthValidateProxy":          classUnary,
	"ImportCopilot":               classUnary,
	"RefreshOAuthToken":           classUnary,
	"RefreshOAuthTokenForAccount": classUnary,

	// ProjectLifecycle.
	"ProjectNeedsInitialization": classUnary,
	"MarkProjectInitialized":     classUnary,
	"InitializePrompt":           classUnary,
	"ListSkills":                 classUnary,
	"ReadSkill":                  classUnary,
	"ConfigProblems":             classUnary,
	"SkillStates":                classUnary,
	"BuiltinSkills":              classUnary,
	"DoctorProblems":             classUnary,
	"ListCustomCommands":         classUnary,

	// MCPController.
	"WaitForMCPInit":               classUnary,
	"MCPGetStates":                 classUnary,
	"MCPResources":                 classUnary,
	"MCPRefreshPrompts":            classUnary,
	"MCPRefreshResources":          classUnary,
	"RefreshMCPTools":              classUnary,
	"ReadMCPResource":              classUnary,
	"ListMCPPrompts":               classUnary,
	"GetMCPPrompt":                 classUnary,
	"EnableDockerMCP":              classUnary,
	"DisableDockerMCP":             classUnary,
	"DockerMCPAvailable":           classCachedGetter,
	"RefreshDockerMCPAvailability": classUnary,
	"MCPAuthenticate":              classUnary,
	"MCPPendingAuth":               classCachedGetter,
	"MCPAuthURL":                   classCachedGetter,

	// WorktreeController.
	"EnterWorktree": classHandle,
	"ExitWorktree":  classHandle,

	// ThreadController.
	"ListThreads":     classUnary,
	"CreateThread":    classUnary,
	"ActivateThread":  classUnary,
	"CancelThread":    classUnary,
	"RemoveThread":    classUnary,
	"SupportsThreads": classCachedGetter,
	"AttachThread":    classHandle,

	// TaskController.
	"ListTasks":     classUnary,
	"CancelTask":    classUnary,
	"SupportsTasks": classCachedGetter,

	// BackgroundJobs.
	"BackgroundJobCounts": classCachedGetter,

	// EventSubscriber.
	"Subscribe":     classStream,
	"SubscribeWith": classStream,
	"Shutdown":      classClientLocal,
}

// TestMethodClassificationIsComplete fails, loudly, the moment Workspace
// grows a method that methodClasses does not classify, or methodClasses
// names a method Workspace no longer has. Mirrors
// TestReadOnlyWorkspace_MethodClassificationIsComplete's shape
// (read_only_workspace_classification_test.go), the pattern this table
// reuses per CLIENT-SERVER.md's "Проверка полноты по рефлексии" note.
func TestMethodClassificationIsComplete(t *testing.T) {
	t.Parallel()

	typ := reflect.TypeOf((*Workspace)(nil)).Elem()
	all := make(map[string]bool, typ.NumMethod())
	for i := range typ.NumMethod() {
		all[typ.Method(i).Name] = true
	}

	for name := range all {
		if _, ok := methodClasses[name]; !ok {
			t.Errorf("Workspace.%s has no entry in methodClasses (wire_classes_test.go); "+
				"classify it as U, C, S, H, or X per CLIENT-SERVER.md's method-class table", name)
		}
	}
	for name, class := range methodClasses {
		if !all[name] {
			t.Errorf("methodClasses names %s (class %s) but Workspace has no such method any more; remove the stale entry", name, class)
		}
	}
}
