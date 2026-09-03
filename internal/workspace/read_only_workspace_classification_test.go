package workspace

import (
	"reflect"
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/git"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/stretchr/testify/require"
)

// refusedMethods are the Workspace methods readOnlyWorkspace must override
// and refuse: every mutation, plus Shutdown and Subscribe, which do not
// touch persisted state but do register process-level effects (tearing
// down or re-subscribing) on the real workspace behind this one.
//
// readOnlyWorkspace embeds Workspace, so any method NOT listed here (or in
// readOnlySafeMethods below) is silently promoted from the embedded field:
// a call reaches the underlying workspace unchanged. That is the intended
// behavior for a read - it is a correctness bug for a mutation. This list,
// together with TestReadOnlyWorkspace_MethodClassificationIsComplete and
// TestReadOnlyWorkspace_RefusesEveryMutatingMethod, is what turns "someone
// added a mutating method and forgot to override it" back into a build/test
// failure instead of a silent write-through.
var refusedMethods = []string{
	"ActivateAccount",
	"ActivateThread",
	"AgentCancel",
	"AgentClearQueue",
	"AgentRun",
	"AgentRunShellCommand",
	"AgentRunStream",
	"AgentSummarize",
	"ApplySessionModel",
	"AttachThread",
	"CancelTask",
	"CancelThread",
	"CompleteOAuth",
	"ConfigureCustomProvider",
	"CreateSession",
	"CreateThread",
	"DeleteSession",
	"DisableDockerMCP",
	"EnableDockerMCP",
	"FileTrackerRecordRead",
	"ImportCopilot",
	"InitCoderAgent",
	"InitCoderAgentNonInteractive",
	"LSPStart",
	"LSPStopAll",
	"MCPAuthenticate",
	"MCPRefreshPrompts",
	"MCPRefreshResources",
	"MarkProjectInitialized",
	"MergeThread",
	"OverridePreferredModel",
	"PermissionDeny",
	"PermissionGrant",
	"PermissionGrantPersistent",
	"PermissionSetSkipRequests",
	"QuestionAnswer",
	"QuestionCancel",
	"RecordAccount",
	"RefreshAccountLimits",
	"RefreshMCPTools",
	"RefreshOAuthToken",
	"PurgeAccounts",
	"RemoveAccount",
	"RemoveConfigField",
	"RemoveThread",
	"RenameSession",
	"SetCompactMode",
	"SetConfigField",
	"SetProviderAPIKey",
	"SetProviderProxy",
	"Shutdown",
	"StartOAuth",
	"Subscribe",
	"UpdateAccount",
	"UpdateAgentModel",
	"UpdatePreferredModel",
	"VerifyProviderAPIKey",
}

// readOnlySafeMethods are every other Workspace method: pure reads that
// readOnlyWorkspace is free to let the embedded Workspace answer directly,
// plus a handful (GetSession, ListMessages, SetCurrentSession, ...) that
// readOnlyWorkspace still overrides for its own reasons (scoping a thread's
// descendants, reporting the thread worktree path) but whose override is
// not a safety requirement - forgetting it would misbehave, not leak a
// mutation.
var readOnlySafeMethods = []string{
	"AgentIsBusy",
	"AgentIsReady",
	"AgentIsSessionBusy",
	"AgentModel",
	"AgentQueuedPromptsList",
	"AgentReadyErr",
	"BackgroundJobCounts",
	"BuiltinSkills",
	"Config",
	"ConfigProblems",
	"CurrentPlanUsage",
	"AccountCapabilities",
	"CustomProviderTypes",
	"DoctorProblems",
	"DockerMCPAvailable",
	"RefreshDockerMCPAvailability",
	"ListCustomCommands",
	"SkillStates",
	"FileTrackerLastReadTime",
	"FileTrackerListReadFiles",
	"GetLastSession",
	"KnownProviders",
	"GetMCPPrompt",
	"GetSession",
	"GetThread",
	"InitializePrompt",
	"LSPGetDiagnosticCounts",
	"LSPGetStates",
	"ListAccounts",
	"ListAllUserMessages",
	"ListMCPPrompts",
	"ListMessages",
	"ListMessagesBySessionIDs",
	"ListSessionHistory",
	"ListSessions",
	"ListSkills",
	"ListTasks",
	"ListThreads",
	"ListUserMessages",
	"MCPAuthURL",
	"MCPGetStates",
	"MCPPendingAuth",
	"MCPResources",
	"OAuthConfiguredProxy",
	"OAuthValidateProxy",
	"PermissionSkipRequests",
	"ProjectNeedsInitialization",
	"ReadMCPResource",
	"ReadSkill",
	"ResetAgentToolCache",
	"Resolver",
	"SessionDescendantCost",
	"SetCurrentSession",
	"SetCurrentSessionGeneration",
	"Stats",
	"SupportsTasks",
	"SupportsThreads",
	"UncommittedFiles",
	"WaitForMCPInit",
	"WorkingDir",
}

// TestReadOnlyWorkspace_MethodClassificationIsComplete is the crux of the
// safety property: it fails, loudly, the moment Workspace grows a method
// that is not yet in refusedMethods or readOnlySafeMethods above.
// readOnlyWorkspace no longer embeds a Workspace, so a missing method is
// already a compile error - but a new method that someone wires straight
// through to the real workspace without deciding whether it is safe would
// not be. Classifying every method is what forces that decision.
func TestReadOnlyWorkspace_MethodClassificationIsComplete(t *testing.T) {
	t.Parallel()

	typ := reflect.TypeOf((*Workspace)(nil)).Elem()
	all := make(map[string]bool, typ.NumMethod())
	for i := range typ.NumMethod() {
		all[typ.Method(i).Name] = true
	}

	classified := make(map[string]string, len(all))
	for _, name := range refusedMethods {
		if prev, ok := classified[name]; ok {
			t.Errorf("%s is listed twice (already classified %s)", name, prev)
			continue
		}
		classified[name] = "refused"
	}
	for _, name := range readOnlySafeMethods {
		if prev, ok := classified[name]; ok {
			t.Errorf("%s is listed twice (already classified %s)", name, prev)
			continue
		}
		classified[name] = "read-only-safe"
	}

	for name := range all {
		if _, ok := classified[name]; !ok {
			t.Errorf("Workspace.%s is not classified as refused or read-only-safe in "+
				"read_only_workspace_classification_test.go; decide whether readOnlyWorkspace "+
				"must override it to refuse a mutation and add it to the matching list", name)
		}
	}
	for name := range classified {
		if !all[name] {
			t.Errorf("%s is classified but is not a Workspace method any more; remove the stale entry", name)
		}
	}
}

// TestReadOnlyWorkspace_RefusesEveryMutatingMethod is the behavioral half
// of the classification above: for every name in refusedMethods, it proves
// readOnlyWorkspace both reports a refusal AND never lets the call reach
// the embedded workspace (via stubWorkspace.calls) - promotion from
// embedding would satisfy the interface without doing either.
func TestReadOnlyWorkspace_RefusesEveryMutatingMethod(t *testing.T) {
	t.Parallel()

	cases := map[string]func(t *testing.T, ro *readOnlyWorkspace){
		"ActivateThread": func(t *testing.T, ro *readOnlyWorkspace) {
			_, err := ro.ActivateThread(t.Context(), "id")
			require.True(t, IsReadOnlyError(err))
		},
		"AgentCancel": func(_ *testing.T, ro *readOnlyWorkspace) {
			ro.AgentCancel("sess-1")
		},
		"AgentClearQueue": func(_ *testing.T, ro *readOnlyWorkspace) {
			ro.AgentClearQueue("sess-1")
		},
		"AgentRun": func(t *testing.T, ro *readOnlyWorkspace) {
			err := ro.AgentRun(t.Context(), "sess-1", "hello")
			require.True(t, IsReadOnlyError(err))
		},
		"AgentRunShellCommand": func(t *testing.T, ro *readOnlyWorkspace) {
			_, err := ro.AgentRunShellCommand(t.Context(), "sess-1", "echo hi", 80, nil, false)
			require.True(t, IsReadOnlyError(err))
		},
		"AgentRunStream": func(t *testing.T, ro *readOnlyWorkspace) {
			_, err := ro.AgentRunStream(t.Context(), "sess-1", "hello", AgentRunOptions{})
			require.True(t, IsReadOnlyError(err))
		},
		"AgentSummarize": func(t *testing.T, ro *readOnlyWorkspace) {
			err := ro.AgentSummarize(t.Context(), "sess-1")
			require.True(t, IsReadOnlyError(err))
		},
		"ApplySessionModel": func(t *testing.T, ro *readOnlyWorkspace) {
			switched, err := ro.ApplySessionModel(t.Context(), "sess-1")
			require.NoError(t, err)
			require.False(t, switched)
		},
		"AttachThread": func(t *testing.T, ro *readOnlyWorkspace) {
			_, _, err := ro.AttachThread(t.Context(), "id")
			require.True(t, IsReadOnlyError(err))
		},
		"CancelTask": func(t *testing.T, ro *readOnlyWorkspace) {
			err := ro.CancelTask(t.Context(), "id", "reason")
			require.True(t, IsReadOnlyError(err))
		},
		"CancelThread": func(t *testing.T, ro *readOnlyWorkspace) {
			err := ro.CancelThread(t.Context(), "id", "reason")
			require.True(t, IsReadOnlyError(err))
		},
		"ConfigureCustomProvider": func(t *testing.T, ro *readOnlyWorkspace) {
			_, err := ro.ConfigureCustomProvider(t.Context(), config.ScopeGlobal, ConfigureCustomProviderParams{ID: "x", BaseURL: "http://example.com"})
			require.True(t, IsReadOnlyError(err))
		},
		"CreateSession": func(t *testing.T, ro *readOnlyWorkspace) {
			_, err := ro.CreateSession(t.Context(), "title")
			require.True(t, IsReadOnlyError(err))
		},
		"CreateThread": func(t *testing.T, ro *readOnlyWorkspace) {
			_, err := ro.CreateThread(t.Context(), proto.CreateThreadRequest{Name: "x"})
			require.True(t, IsReadOnlyError(err))
		},
		"DeleteSession": func(t *testing.T, ro *readOnlyWorkspace) {
			err := ro.DeleteSession(t.Context(), "sess-1")
			require.True(t, IsReadOnlyError(err))
		},
		"DisableDockerMCP": func(t *testing.T, ro *readOnlyWorkspace) {
			err := ro.DisableDockerMCP()
			require.True(t, IsReadOnlyError(err))
		},
		"EnableDockerMCP": func(t *testing.T, ro *readOnlyWorkspace) {
			err := ro.EnableDockerMCP(t.Context())
			require.True(t, IsReadOnlyError(err))
		},
		"FileTrackerRecordRead": func(t *testing.T, ro *readOnlyWorkspace) {
			ro.FileTrackerRecordRead(t.Context(), "sess-1", "/foo")
		},
		"ImportCopilot": func(t *testing.T, ro *readOnlyWorkspace) {
			_, ok := ro.ImportCopilot()
			require.False(t, ok)
		},
		"InitCoderAgent": func(t *testing.T, ro *readOnlyWorkspace) {
			err := ro.InitCoderAgent(t.Context())
			require.True(t, IsReadOnlyError(err))
		},
		"InitCoderAgentNonInteractive": func(t *testing.T, ro *readOnlyWorkspace) {
			err := ro.InitCoderAgentNonInteractive(t.Context())
			require.True(t, IsReadOnlyError(err))
		},
		"LSPStart": func(t *testing.T, ro *readOnlyWorkspace) {
			ro.LSPStart(t.Context(), "/tmp")
		},
		"LSPStopAll": func(t *testing.T, ro *readOnlyWorkspace) {
			ro.LSPStopAll(t.Context())
		},
		"MCPAuthenticate": func(t *testing.T, ro *readOnlyWorkspace) {
			err := ro.MCPAuthenticate(t.Context(), "name")
			require.True(t, IsReadOnlyError(err))
		},
		"MCPRefreshPrompts": func(t *testing.T, ro *readOnlyWorkspace) {
			ro.MCPRefreshPrompts(t.Context(), "name")
		},
		"MCPRefreshResources": func(t *testing.T, ro *readOnlyWorkspace) {
			ro.MCPRefreshResources(t.Context(), "name")
		},
		"MarkProjectInitialized": func(t *testing.T, ro *readOnlyWorkspace) {
			err := ro.MarkProjectInitialized()
			require.True(t, IsReadOnlyError(err))
		},
		"MergeThread": func(t *testing.T, ro *readOnlyWorkspace) {
			_, err := ro.MergeThread(t.Context(), "id")
			require.True(t, IsReadOnlyError(err))
		},
		"OverridePreferredModel": func(t *testing.T, ro *readOnlyWorkspace) {
			err := ro.OverridePreferredModel(config.SelectedModel{})
			require.True(t, IsReadOnlyError(err))
		},
		"PermissionDeny": func(t *testing.T, ro *readOnlyWorkspace) {
			require.False(t, ro.PermissionDeny(permission.PermissionRequest{}))
		},
		"PermissionGrant": func(t *testing.T, ro *readOnlyWorkspace) {
			require.False(t, ro.PermissionGrant(permission.PermissionRequest{}))
		},
		"PermissionGrantPersistent": func(t *testing.T, ro *readOnlyWorkspace) {
			require.False(t, ro.PermissionGrantPersistent(permission.PermissionRequest{}))
		},
		"PermissionSetSkipRequests": func(_ *testing.T, ro *readOnlyWorkspace) {
			ro.PermissionSetSkipRequests(true)
		},
		"QuestionAnswer": func(t *testing.T, ro *readOnlyWorkspace) {
			require.False(t, ro.QuestionAnswer("", nil))
		},
		"QuestionCancel": func(t *testing.T, ro *readOnlyWorkspace) {
			require.False(t, ro.QuestionCancel())
		},
		"RefreshMCPTools": func(t *testing.T, ro *readOnlyWorkspace) {
			ro.RefreshMCPTools(t.Context(), "name")
		},
		"RefreshOAuthToken": func(t *testing.T, ro *readOnlyWorkspace) {
			err := ro.RefreshOAuthToken(t.Context(), config.ScopeWorkspace, "provider")
			require.True(t, IsReadOnlyError(err))
		},
		"RemoveConfigField": func(t *testing.T, ro *readOnlyWorkspace) {
			err := ro.RemoveConfigField(config.ScopeWorkspace, "key")
			require.True(t, IsReadOnlyError(err))
		},
		"RemoveThread": func(t *testing.T, ro *readOnlyWorkspace) {
			err := ro.RemoveThread(t.Context(), "id", proto.RemoveThreadOptions{})
			require.True(t, IsReadOnlyError(err))
		},
		"RenameSession": func(t *testing.T, ro *readOnlyWorkspace) {
			err := ro.RenameSession(t.Context(), "sess-1", "new title")
			require.True(t, IsReadOnlyError(err))
		},
		"SetCompactMode": func(t *testing.T, ro *readOnlyWorkspace) {
			err := ro.SetCompactMode(config.ScopeWorkspace, true)
			require.True(t, IsReadOnlyError(err))
		},
		"SetConfigField": func(t *testing.T, ro *readOnlyWorkspace) {
			err := ro.SetConfigField(config.ScopeWorkspace, "key", "val")
			require.True(t, IsReadOnlyError(err))
		},
		"SetProviderAPIKey": func(t *testing.T, ro *readOnlyWorkspace) {
			err := ro.SetProviderAPIKey(config.ScopeWorkspace, "provider", "key")
			require.True(t, IsReadOnlyError(err))
		},
		"RecordAccount": func(t *testing.T, ro *readOnlyWorkspace) {
			_, err := ro.RecordAccount(config.ScopeWorkspace, "provider", accounts.LegacyCredential{})
			require.True(t, IsReadOnlyError(err))
		},
		"ActivateAccount": func(t *testing.T, ro *readOnlyWorkspace) {
			err := ro.ActivateAccount(config.ScopeWorkspace, "provider", "account")
			require.True(t, IsReadOnlyError(err))
		},
		"UpdateAccount": func(t *testing.T, ro *readOnlyWorkspace) {
			err := ro.UpdateAccount("provider", accounts.Account{ID: "account"})
			require.True(t, IsReadOnlyError(err))
		},
		"RemoveAccount": func(t *testing.T, ro *readOnlyWorkspace) {
			err := ro.RemoveAccount(config.ScopeWorkspace, "provider", "account")
			require.True(t, IsReadOnlyError(err))
		},
		"PurgeAccounts": func(t *testing.T, ro *readOnlyWorkspace) {
			err := ro.PurgeAccounts(config.ScopeWorkspace, "provider")
			require.True(t, IsReadOnlyError(err))
		},
		"SetProviderProxy": func(t *testing.T, ro *readOnlyWorkspace) {
			err := ro.SetProviderProxy("provider", "http://proxy.example:8080")
			require.True(t, IsReadOnlyError(err))
		},
		"RefreshAccountLimits": func(t *testing.T, ro *readOnlyWorkspace) {
			_, err := ro.RefreshAccountLimits(t.Context(), "provider")
			require.True(t, IsReadOnlyError(err))
		},
		"StartOAuth": func(t *testing.T, ro *readOnlyWorkspace) {
			_, _, err := ro.StartOAuth(t.Context(), "provider", "")
			require.True(t, IsReadOnlyError(err))
		},
		"CompleteOAuth": func(t *testing.T, ro *readOnlyWorkspace) {
			_, err := ro.CompleteOAuth(t.Context(), "provider", "", &oauth.Token{}, false)
			require.True(t, IsReadOnlyError(err))
		},
		"Shutdown": func(_ *testing.T, ro *readOnlyWorkspace) {
			ro.Shutdown()
		},
		"Subscribe": func(_ *testing.T, ro *readOnlyWorkspace) {
			ro.Subscribe(nil)
		},
		"UpdateAgentModel": func(t *testing.T, ro *readOnlyWorkspace) {
			err := ro.UpdateAgentModel(t.Context())
			require.True(t, IsReadOnlyError(err))
		},
		"UpdatePreferredModel": func(t *testing.T, ro *readOnlyWorkspace) {
			err := ro.UpdatePreferredModel(config.ScopeWorkspace, config.SelectedModel{})
			require.True(t, IsReadOnlyError(err))
		},
		"VerifyProviderAPIKey": func(t *testing.T, ro *readOnlyWorkspace) {
			err := ro.VerifyProviderAPIKey(t.Context(), "provider", "key")
			require.True(t, IsReadOnlyError(err))
		},
	}

	// The table above and refusedMethods must name exactly the same set,
	// so neither can drift from the other unnoticed.
	for _, name := range refusedMethods {
		if _, ok := cases[name]; !ok {
			t.Fatalf("refusedMethods lists %s but no test case exercises it", name)
		}
	}
	inRefused := make(map[string]bool, len(refusedMethods))
	for _, name := range refusedMethods {
		inRefused[name] = true
	}
	for name := range cases {
		if !inRefused[name] {
			t.Fatalf("test case %s is not in refusedMethods", name)
		}
	}

	for name, check := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stub := &stubWorkspace{}
			ro := NewReadOnlyWorkspace(stub, "/tmp/thread-worktree", "sess-1", "", git.UncommittedFiles)
			check(t, ro)
			require.Zerof(t, stub.calls[name],
				"%s reached the underlying workspace; readOnlyWorkspace must refuse it itself, not let it forward", name)
		})
	}
}
