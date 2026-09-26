package wsrpc

import (
	"slices"

	"charm.land/catwalk/pkg/catwalk"

	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/rave-soft/sennit/internal/workspace"
)

// BuildClientState reads every class-C getter off ws and folds the result
// into one workspace.ClientState (CLIENT-SERVER.md, PR 1.4a) -- the
// builder grpcws's per-hub state publisher calls after each hub event and
// on its own ticker, and Snapshot falls back to when a hub has not
// published one yet. It never sets Version: that is the publisher's job
// (it alone knows the last version it published), and Snapshot's own job
// when nothing has been published (see grpcws's eventHub.snapshot).
//
// Every getter here is documented on Workspace as non-blocking: no network
// call, no disk read past an in-memory cache. BuildClientState relies on
// that -- it must itself be safe to call from a background ticker with no
// request in flight -- and adds no I/O of its own: PlanUsage and
// AccountCapabilities are keyed off the provider id universe
// KnownProviders/Config().Providers already hand back, not off anything
// new asked of ws.
//
// Reported gap, per this step's brief ("if one cannot be answered from
// this struct alone, report it instead of approximating"): BusySessions
// and QueuedPrompts are left empty. AgentIsSessionBusy(id) and
// AgentQueuedPromptsList(id) both need a candidate sessionID, and
// Workspace has no getter that enumerates "every session id with agent
// activity" to drive them from -- unlike the other four parameterized
// getters (CurrentPlanUsage, AccountCapabilities, MCPAuthURL), whose
// parameter space is exactly KnownProviders()/MCPPendingAuth(), agent
// activity has no such closed set on this contract. The underlying data
// exists (internal/agent's dispatcher tracks a per-session state map, and
// the delegation finalizer separately tracks sessions kept busy only by a
// running delegation), but reaching it needs a new enumerable primitive
// threaded through SessionAgent/Coordinator into a new Workspace getter --
// out of scope for this step. Approximating from AgentQueuedPromptsList's
// own sessions (i.e. only sessions that happen to already have a queue)
// would silently under-report every lone in-flight turn with nothing
// queued, which is worse than an honestly empty field: a client reading
// BusySessions today must not trust it for "no one is busy" and should
// keep using AgentIsBusy()/AgentIsSessionBusy(id) directly until this gap
// closes.
func BuildClientState(ws workspace.Workspace) workspace.ClientState {
	cfg := ws.Config()

	state := workspace.ClientState{
		AgentIsBusy:            ws.AgentIsBusy(),
		AgentModel:             ws.AgentModel(),
		AgentIsReady:           ws.AgentIsReady(),
		AgentReadyErr:          workspace.EncodeError(ws.AgentReadyErr()),
		PermissionSkipRequests: ws.PermissionSkipRequests(),
		Config:                 cfg,
		WorkingDir:             ws.WorkingDir(),
		KnownProviders:         ws.KnownProviders(),
		CustomProviderTypes:    ws.CustomProviderTypes(),
		WorktreeState:          ws.WorktreeState(),
		SupportsThreads:        ws.SupportsThreads(),
		SupportsTasks:          ws.SupportsTasks(),
		BackgroundJobs:         ws.BackgroundJobCounts(),
		MCPPendingAuth:         ws.MCPPendingAuth(),
	}
	state.DockerMCPAvailable, state.DockerMCPKnown = ws.DockerMCPAvailable()

	for _, id := range knownAndConfiguredProviderIDs(ws.KnownProviders(), cfg) {
		if usage, ok := ws.CurrentPlanUsage(id); ok {
			if state.PlanUsage == nil {
				state.PlanUsage = map[string]accounts.Usage{}
			}
			state.PlanUsage[id] = usage
		}
		if state.AccountCapabilities == nil {
			state.AccountCapabilities = map[string]workspace.AccountCapabilities{}
		}
		state.AccountCapabilities[id] = ws.AccountCapabilities(id)
	}

	return state
}

// knownAndConfiguredProviderIDs is PlanUsage/AccountCapabilities' provider
// id universe: every provider this workspace knows about (KnownProviders)
// plus every provider actually configured (cfg.Providers), deduplicated —
// AccountCapabilities(id) is meaningful for both (a provider can be
// configured without being in the catalog, e.g. a custom provider), and
// CurrentPlanUsage(id) is empty/ok=false for one with nothing to report,
// which the caller already skips.
func knownAndConfiguredProviderIDs(known []catwalk.Provider, cfg *workspace.FrontendConfig) []string {
	seen := make(map[string]struct{}, len(known))
	var ids []string
	add := func(id string) {
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for _, p := range known {
		add(string(p.ID))
	}
	if cfg != nil {
		for _, p := range cfg.Providers {
			add(p.ID)
		}
	}
	slices.Sort(ids)
	return ids
}
