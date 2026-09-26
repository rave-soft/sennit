package grpcws

import (
	"log/slog"
	"slices"

	"charm.land/catwalk/pkg/catwalk"

	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/rave-soft/sennit/internal/workspace"
)

// cachedState returns this Client's local workspace.ClientState cache,
// never making a network call -- every class-C getter below reads through
// it (CLIENT-SERVER.md, PR 1.4b, build step 3). Before the first
// successful Connect it is the zero value, which this logs once per
// Client (not once per call: a UI frame loop would otherwise spam it).
func (c *Client) cachedState() workspace.ClientState {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.haveState {
		c.noConnectionOnce.Do(func() {
			slog.Error("Wsrpc client class-C getter called before Connect succeeded, returning zero value", "handle", c.handle)
		})
	}
	return c.state
}

// The methods below are class C (wsrpc.MethodClasses): each answers
// straight from cachedState(), per workspace.ClientState's own field ->
// method mapping doc comment. None of them makes an RPC.

func (c *Client) AgentIsBusy() bool { return c.cachedState().AgentIsBusy }

func (c *Client) AgentIsSessionBusy(sessionID string) bool {
	return slices.Contains(c.cachedState().BusySessions, sessionID)
}

func (c *Client) AgentModel() workspace.AgentModel { return c.cachedState().AgentModel }

func (c *Client) AgentIsReady() bool { return c.cachedState().AgentIsReady }

func (c *Client) AgentReadyErr() error { return workspace.DecodeError(c.cachedState().AgentReadyErr) }

func (c *Client) AgentQueuedPromptsList(sessionID string) []string {
	return c.cachedState().QueuedPrompts[sessionID]
}

func (c *Client) AgentActivity() workspace.AgentActivity {
	st := c.cachedState()
	return workspace.AgentActivity{BusySessions: st.BusySessions, QueuedPrompts: st.QueuedPrompts}
}

func (c *Client) PermissionSkipRequests() bool { return c.cachedState().PermissionSkipRequests }

func (c *Client) Config() *workspace.FrontendConfig { return c.cachedState().Config }

func (c *Client) WorkingDir() string { return c.cachedState().WorkingDir }

func (c *Client) CurrentPlanUsage(providerID string) (accounts.Usage, bool) {
	v, ok := c.cachedState().PlanUsage[providerID]
	return v, ok
}

func (c *Client) AccountCapabilities(providerID string) workspace.AccountCapabilities {
	return c.cachedState().AccountCapabilities[providerID]
}

func (c *Client) KnownProviders() []catwalk.Provider { return c.cachedState().KnownProviders }

func (c *Client) CustomProviderTypes() []string { return c.cachedState().CustomProviderTypes }

func (c *Client) DockerMCPAvailable() (available, known bool) {
	st := c.cachedState()
	return st.DockerMCPAvailable, st.DockerMCPKnown
}

func (c *Client) MCPPendingAuth() []workspace.MCPPendingAuthServer {
	return c.cachedState().MCPPendingAuth
}

// MCPAuthURL scans the cached MCPPendingAuth list for name -- folded in
// there instead of a separate cache field, per workspace.ClientState's own
// doc comment.
func (c *Client) MCPAuthURL(name string) string {
	for _, s := range c.cachedState().MCPPendingAuth {
		if s.Name == name {
			return s.URL
		}
	}
	return ""
}

func (c *Client) WorktreeState() workspace.WorktreeState { return c.cachedState().WorktreeState }

func (c *Client) SupportsThreads() bool { return c.cachedState().SupportsThreads }

func (c *Client) SupportsTasks() bool { return c.cachedState().SupportsTasks }

func (c *Client) BackgroundJobCounts() workspace.BackgroundJobCounts {
	return c.cachedState().BackgroundJobs
}
