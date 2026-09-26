package wsrpc_test

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/wsrpctest"
)

// clientStateMapping documents, for every class-C Workspace method
// (wsrpc.MethodClasses), how wsrpc.BuildClientState answers it: setup
// configures a fresh StubWorkspace with a distinctive value for that
// getter, check asserts the built workspace.ClientState reflects it. A
// class-C method with no entry here fails
// TestBuildClientState_MapsEveryClassCMethod immediately, naming it --
// the mapping-completeness check this step's brief asked for, one level
// below classes_test.go's own method -> class completeness check.
//
// AgentIsSessionBusy and AgentQueuedPromptsList are answered by
// AgentActivity's own case below in principle (a client reads
// cs.BusySessions/cs.QueuedPrompts instead of calling either getter
// directly), so their own entries just prove the specific session id
// AgentActivity names comes through BuildClientState unchanged.
var clientStateMapping = map[string]struct {
	setup func(*wsrpctest.StubWorkspace)
	check func(*testing.T, workspace.ClientState)
}{
	"AgentIsBusy": {
		setup: func(s *wsrpctest.StubWorkspace) { s.AgentIsBusyResult = true },
		check: func(t *testing.T, cs workspace.ClientState) { require.True(t, cs.AgentIsBusy) },
	},
	"AgentIsSessionBusy": {
		setup: func(s *wsrpctest.StubWorkspace) {
			s.AgentActivityResult = workspace.AgentActivity{BusySessions: []string{"distinctive-busy-session"}}
		},
		check: func(t *testing.T, cs workspace.ClientState) {
			require.Contains(t, cs.BusySessions, "distinctive-busy-session")
		},
	},
	"AgentQueuedPromptsList": {
		setup: func(s *wsrpctest.StubWorkspace) {
			s.AgentActivityResult = workspace.AgentActivity{
				QueuedPrompts: map[string][]string{"distinctive-queued-session": {"distinctive-prompt"}},
			}
		},
		check: func(t *testing.T, cs workspace.ClientState) {
			require.Equal(t, []string{"distinctive-prompt"}, cs.QueuedPrompts["distinctive-queued-session"])
		},
	},
	"AgentActivity": {
		setup: func(s *wsrpctest.StubWorkspace) {
			s.AgentActivityResult = workspace.AgentActivity{
				BusySessions:  []string{"distinctive-busy-session"},
				QueuedPrompts: map[string][]string{"distinctive-queued-session": {"distinctive-prompt"}},
			}
		},
		check: func(t *testing.T, cs workspace.ClientState) {
			require.Equal(t, []string{"distinctive-busy-session"}, cs.BusySessions)
			require.Equal(t, []string{"distinctive-prompt"}, cs.QueuedPrompts["distinctive-queued-session"])
		},
	},
	"AgentModel": {
		setup: func(s *wsrpctest.StubWorkspace) { s.AgentModelResult = wsrpctest.AgentModelSample },
		check: func(t *testing.T, cs workspace.ClientState) {
			require.Equal(t, wsrpctest.AgentModelSample, cs.AgentModel)
		},
	},
	"AgentIsReady": {
		setup: func(s *wsrpctest.StubWorkspace) { s.AgentIsReadyResult = true },
		check: func(t *testing.T, cs workspace.ClientState) { require.True(t, cs.AgentIsReady) },
	},
	"AgentReadyErr": {
		setup: func(s *wsrpctest.StubWorkspace) { s.AgentReadyErrResult = workspace.ErrAgentNotInitialized },
		check: func(t *testing.T, cs workspace.ClientState) {
			require.NotNil(t, cs.AgentReadyErr)
		},
	},
	"PermissionSkipRequests": {
		setup: func(s *wsrpctest.StubWorkspace) { s.PermissionSkipRequestsResult = true },
		check: func(t *testing.T, cs workspace.ClientState) { require.True(t, cs.PermissionSkipRequests) },
	},
	"Config": {
		setup: func(s *wsrpctest.StubWorkspace) {
			s.ConfigResult = &workspace.FrontendConfig{InitializeAs: "distinctive-agent"}
		},
		check: func(t *testing.T, cs workspace.ClientState) {
			require.NotNil(t, cs.Config)
			require.Equal(t, "distinctive-agent", cs.Config.InitializeAs)
		},
	},
	"WorkingDir": {
		setup: func(s *wsrpctest.StubWorkspace) { s.WorkingDirResult = "/distinctive/dir" },
		check: func(t *testing.T, cs workspace.ClientState) {
			require.Equal(t, "/distinctive/dir", cs.WorkingDir)
		},
	},
	"CurrentPlanUsage": {
		setup: func(s *wsrpctest.StubWorkspace) {
			s.KnownProvidersResult = []catwalk.Provider{{ID: "openai"}}
			s.CurrentPlanUsageResult = accounts.Usage{Plan: "distinctive-plan"}
			s.CurrentPlanUsageOK = true
		},
		check: func(t *testing.T, cs workspace.ClientState) {
			require.Equal(t, "distinctive-plan", cs.PlanUsage["openai"].Plan)
		},
	},
	"AccountCapabilities": {
		setup: func(s *wsrpctest.StubWorkspace) {
			s.KnownProvidersResult = []catwalk.Provider{{ID: "openai"}}
			s.AccountCapabilitiesResult = workspace.AccountCapabilities{Usage: true, OAuth: true}
		},
		check: func(t *testing.T, cs workspace.ClientState) {
			require.Equal(t, workspace.AccountCapabilities{Usage: true, OAuth: true}, cs.AccountCapabilities["openai"])
		},
	},
	"KnownProviders": {
		setup: func(s *wsrpctest.StubWorkspace) {
			s.KnownProvidersResult = []catwalk.Provider{{ID: "distinctive-provider"}}
		},
		check: func(t *testing.T, cs workspace.ClientState) {
			require.Equal(t, []catwalk.Provider{{ID: "distinctive-provider"}}, cs.KnownProviders)
		},
	},
	"CustomProviderTypes": {
		setup: func(s *wsrpctest.StubWorkspace) { s.CustomProviderTypesResult = []string{"distinctive-type"} },
		check: func(t *testing.T, cs workspace.ClientState) {
			require.Equal(t, []string{"distinctive-type"}, cs.CustomProviderTypes)
		},
	},
	"DockerMCPAvailable": {
		setup: func(s *wsrpctest.StubWorkspace) {
			s.DockerMCPAvailableResult = true
			s.DockerMCPKnownResult = true
		},
		check: func(t *testing.T, cs workspace.ClientState) {
			require.True(t, cs.DockerMCPAvailable)
			require.True(t, cs.DockerMCPKnown)
		},
	},
	"MCPPendingAuth": {
		setup: func(s *wsrpctest.StubWorkspace) {
			s.MCPPendingAuthResult = []workspace.MCPPendingAuthServer{{Name: "distinctive-server", URL: "https://example.test"}}
		},
		check: func(t *testing.T, cs workspace.ClientState) {
			require.Equal(t, []workspace.MCPPendingAuthServer{{Name: "distinctive-server", URL: "https://example.test"}}, cs.MCPPendingAuth)
		},
	},
	// MCPAuthURL(name) is answered by scanning MCPPendingAuth for name --
	// see ClientState's doc comment -- so it shares MCPPendingAuth's own
	// case above rather than having a separate one.
	"MCPAuthURL": {
		setup: func(s *wsrpctest.StubWorkspace) {
			s.MCPPendingAuthResult = []workspace.MCPPendingAuthServer{{Name: "distinctive-server", URL: "https://example.test/auth"}}
		},
		check: func(t *testing.T, cs workspace.ClientState) {
			var url string
			for _, p := range cs.MCPPendingAuth {
				if p.Name == "distinctive-server" {
					url = p.URL
				}
			}
			require.Equal(t, "https://example.test/auth", url)
		},
	},
	"WorktreeState": {
		setup: func(s *wsrpctest.StubWorkspace) {
			s.WorktreeStateResult = workspace.WorktreeState{Name: "distinctive-worktree", Active: true}
		},
		check: func(t *testing.T, cs workspace.ClientState) {
			require.Equal(t, workspace.WorktreeState{Name: "distinctive-worktree", Active: true}, cs.WorktreeState)
		},
	},
	"SupportsThreads": {
		setup: func(s *wsrpctest.StubWorkspace) { s.SupportsThreadsResult = true },
		check: func(t *testing.T, cs workspace.ClientState) { require.True(t, cs.SupportsThreads) },
	},
	"SupportsTasks": {
		setup: func(s *wsrpctest.StubWorkspace) { s.SupportsTasksResult = true },
		check: func(t *testing.T, cs workspace.ClientState) { require.True(t, cs.SupportsTasks) },
	},
	"BackgroundJobCounts": {
		setup: func(s *wsrpctest.StubWorkspace) {
			s.BackgroundJobCountsResult = workspace.BackgroundJobCounts{Active: 3, Completed: 5}
		},
		check: func(t *testing.T, cs workspace.ClientState) {
			require.Equal(t, workspace.BackgroundJobCounts{Active: 3, Completed: 5}, cs.BackgroundJobs)
		},
	},
}

// TestBuildClientState_MapsEveryClassCMethod is the reflection-driven
// completeness check this step's brief asked for: every class-C method in
// wsrpc.MethodClasses must have an entry in clientStateMapping, and that
// entry's setup/check pair must actually see its distinctive value come
// back out of wsrpc.BuildClientState.
func TestBuildClientState_MapsEveryClassCMethod(t *testing.T) {
	t.Parallel()

	for name, class := range wsrpc.MethodClasses {
		if class != wsrpc.C {
			continue
		}
		entry, ok := clientStateMapping[name]
		if !ok {
			t.Errorf("workspace.Workspace.%s is class C but has no entry in clientStateMapping "+
				"(client_state_test.go); wsrpc.BuildClientState needs to answer it, or the gap "+
				"needs to be documented and listed the way AgentIsSessionBusy is", name)
			continue
		}

		stub := &wsrpctest.StubWorkspace{}
		entry.setup(stub)
		got := wsrpc.BuildClientState(stub)
		entry.check(t, got)
	}

	for name := range clientStateMapping {
		if wsrpc.MethodClasses[name] != wsrpc.C {
			t.Errorf("clientStateMapping names %s but it is not class C in wsrpc.MethodClasses; remove the stale entry", name)
		}
	}
}
