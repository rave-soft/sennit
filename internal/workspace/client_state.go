package workspace

import (
	"charm.land/catwalk/pkg/catwalk"

	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/rave-soft/sennit/internal/wireerr"
)

// ClientState is one full snapshot of everything a class-C getter (see
// wsrpc.MethodClasses) answers, published as a single event instead of one
// event per getter (CLIENT-SERVER.md, PR 1.4, "Дизайн, уточнён
// 2026-09-26"): 20 getters, five of them parameterized, would otherwise
// mean 20 event types and a client-side cache field for each. Version is a
// monotonic counter the publisher (grpcws's per-hub state publisher)
// bumps only when the built state actually changed since the last publish
// — a client applies a ClientState wholesale and discards one whose
// Version is not strictly greater than what it already has, which is what
// makes replay/resync idempotent for this event the way it already is for
// message/session (compared by UpdatedAt).
//
// Every field below is JSON-safe (snake_case tags live on the types it
// embeds — FrontendConfig, wireerr.Error, accounts.Usage/
// AccountCapabilities) and traces back to exactly one class-C method.
// Field -> method mapping, so a new class-C method with nothing here
// fails wsrpc's BuildClientState mapping test instead of silently being
// answered by a stale cache read:
//
//	AgentIsBusy                 -> AgentController.AgentIsBusy
//	BusySessions                -> AgentController.AgentIsSessionBusy(id), in principle: id's
//	                               presence in this sorted set would answer it for any sessionID.
//	                               NOT YET POPULATED -- see wsrpc.BuildClientState's doc comment
//	                               for why (no getter enumerates "every session id with agent
//	                               activity" to build this from without a new Workspace method).
//	QueuedPrompts               -> AgentController.AgentQueuedPromptsList(id), same reported gap:
//	                               would be keyed by sessionID, only for sessions with a
//	                               non-empty queue, once something can enumerate them.
//	AgentModel                  -> AgentController.AgentModel
//	AgentIsReady                -> AgentController.AgentIsReady
//	AgentReadyErr                       -> AgentController.AgentReadyErr, carried as *wireerr.Error since
//	                               the plain error isn't JSON-safe.
//	PermissionSkipRequests      -> PermissionResolver.PermissionSkipRequests
//	Config                      -> ConfigReader.Config
//	WorkingDir                  -> WorkingDirectory.WorkingDir
//	PlanUsage                   -> AccountUsage.CurrentPlanUsage(providerID): one entry per
//	                               provider that has a usage snapshot at all (the getter's own
//	                               "ok" bool), keyed by providerID in place of the parameter.
//	AccountCapabilities         -> AccountUsage.AccountCapabilities(providerID): one entry per
//	                               provider this workspace knows about (KnownProviders) or has
//	                               configured (Config().Providers), keyed by providerID.
//	DockerMCPAvailable/         -> MCPController.DockerMCPAvailable (available, known bool).
//	  DockerMCPKnown
//	MCPPendingAuth              -> MCPController.MCPPendingAuth, with MCPController.MCPAuthURL
//	                               folded into each entry's URL so the parameterized getter needs
//	                               no separate field.
//	WorktreeState               -> WorktreeController.WorktreeState
//	SupportsThreads             -> ThreadController.SupportsThreads
//	SupportsTasks               -> TaskController.SupportsTasks
//	BackgroundJobs              -> BackgroundJobs.BackgroundJobCounts
//
// Two class-C getters the design note above does not mention -
// ProviderCatalog.KnownProviders and ProviderCatalog.CustomProviderTypes -
// are answered here too (KnownProviders, CustomProviderTypes fields):
// both are ordinary, bounded getters with nothing that would make them
// "approximate" the way an unbounded parameter space would, so leaving
// them out would just be a gap in the design note, not a deliberate
// omission - see wsrpc.BuildClientState's own doc comment.
type ClientState struct {
	Version uint64 `json:"version"`

	AgentIsBusy   bool           `json:"agent_is_busy"`
	BusySessions  []string       `json:"busy_sessions"`
	AgentModel    AgentModel     `json:"agent_model"`
	AgentIsReady  bool           `json:"agent_is_ready"`
	AgentReadyErr *wireerr.Error `json:"agent_ready_err,omitempty"`
	// QueuedPrompts holds only sessions with a non-empty queue -- a session
	// absent from this map has none, same as AgentQueuedPromptsList
	// returning an empty slice for it.
	QueuedPrompts map[string][]string `json:"queued_prompts,omitempty"`

	PermissionSkipRequests bool `json:"permission_skip_requests"`

	Config     *FrontendConfig `json:"config"`
	WorkingDir string          `json:"working_dir"`

	// PlanUsage holds only providers CurrentPlanUsage reports "ok" for.
	PlanUsage map[string]accounts.Usage `json:"plan_usage,omitempty"`
	// AccountCapabilities covers every provider this workspace knows
	// about (KnownProviders) plus every provider configured in Config,
	// so a client can answer AccountCapabilities(id) for any id it could
	// otherwise ask about.
	AccountCapabilities map[string]AccountCapabilities `json:"account_capabilities,omitempty"`

	KnownProviders      []catwalk.Provider `json:"known_providers,omitempty"`
	CustomProviderTypes []string           `json:"custom_provider_types,omitempty"`

	DockerMCPAvailable bool `json:"docker_mcp_available"`
	DockerMCPKnown     bool `json:"docker_mcp_known"`
	// MCPPendingAuth carries each pending server's own auth URL
	// (MCPPendingAuthServer.URL), so MCPAuthURL(name) is answerable by
	// scanning this slice for name instead of a separate map.
	MCPPendingAuth []MCPPendingAuthServer `json:"mcp_pending_auth,omitempty"`

	WorktreeState WorktreeState `json:"worktree_state"`

	SupportsThreads bool `json:"supports_threads"`
	SupportsTasks   bool `json:"supports_tasks"`

	BackgroundJobs BackgroundJobCounts `json:"background_jobs"`
}
