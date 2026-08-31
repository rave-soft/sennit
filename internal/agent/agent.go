// Package agent is the core orchestration layer for Sennit AI agents.
//
// It provides session-based AI agent functionality for managing
// conversations, tool execution, and message handling. It coordinates
// interactions between language models, messages, sessions, and tools while
// handling features like automatic summarization, queuing, and token
// management.
package agent

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/latency"
	"github.com/rave-soft/sennit/internal/pubsub"
	sessionstore "github.com/rave-soft/sennit/internal/session/store"
	"github.com/rave-soft/sennit/internal/version"
	"golang.org/x/sync/errgroup"
)

var userAgent = fmt.Sprintf(brand.Name+"/%s ("+brand.RepoURL+")", version.Version)

type SessionAgent interface {
	Run(context.Context, SessionAgentCall) (*fantasy.AgentResult, error)
	// Steer submits call as an explicit steering follow-up: it makes the
	// same atomic enqueue-into-the-active-turn vs run-as-a-new-turn
	// decision Run makes as a side effect of its busy check, and reports
	// which one it took. See sessionAgent.run for the shared
	// implementation and sessionAgent.Steer for the contract.
	Steer(context.Context, SessionAgentCall) (SteerOutcome, *fantasy.AgentResult, error)
	BeginAccepted(sessionID string) *AcceptedRun
	SetModel(model Model)
	SetTools(tools []fantasy.AgentTool)
	SetSystemPrompt(systemPrompt string)
	Cancel(sessionID string)
	CancelAll()
	IsSessionBusy(sessionID string) bool
	IsBusy() bool
	QueuedPrompts(sessionID string) int
	QueuedPromptsList(sessionID string) []string
	ClearQueue(sessionID string)
	Summarize(context.Context, string, fantasy.ProviderOptions, func(context.Context, *fantasy.ProviderError) error) error
	Model() Model
	GenerateTitle(ctx context.Context, sessionID, userPrompt string)
	// SetLiveSession records the one session this sennit is working in.
	// Only that session and its delegations may be started by something
	// finishing in the background - see dispatcher.wakeAllowed.
	SetLiveSession(sessionID string)
	// DeliverTaskCompletion enqueues completion into sessionID's
	// completion inbox and, if the session is the one being worked in or
	// a delegation of it (see dispatcher.wakeAllowed), idle, and not
	// left canceled by the
	// user, starts a continuation turn for it. See dispatcher's
	// completionInbox field, runTurn.prepareStep (the mid-turn delivery
	// path), and startContinuation (the wake path).
	DeliverTaskCompletion(ctx context.Context, sessionID string, completion TaskCompletion)
	// RegisterDelegationParent records where sessionID (a running
	// delegation's own child session) should deliver a mid-run ask via
	// SendToParent. See DelegationParent.
	RegisterDelegationParent(sessionID string, parent DelegationParent)
	// SendToParent delivers a mid-run ask from sessionID to its
	// registered parent (see RegisterDelegationParent), riding the same
	// completion-inbox delivery path DeliverTaskCompletion uses - at-
	// most-once, non-blocking, and folded into the parent's next step or
	// an idle-wake continuation. Returns an error, delivering nothing,
	// if sessionID has no registered parent.
	SendToParent(ctx context.Context, sessionID, message string) error
}

type Model struct {
	Model      fantasy.LanguageModel
	CatalogCfg catwalk.Model
	ModelCfg   config.SelectedModel
	FlatRate   bool
}

type sessionAgent struct {
	model              *csync.Value[Model]
	systemPromptPrefix *csync.Value[string]
	systemPrompt       *csync.Value[string]
	tools              *csync.Slice[fantasy.AgentTool]

	isSubAgent           bool
	sessions             sessionstore.Service
	messages             MessageService
	disableAutoSummarize bool
	autoSummarizeAt      int64
	notify               pubsub.Publisher[notify.Notification]
	runComplete          pubsub.Publisher[notify.RunComplete]
	mcp                  *mcp.Registry

	// latency is nil-safe: when nil, the two handoff waits are still
	// logged, just not recorded. Every test that builds an agent without
	// a database relies on that, and measurement must never be the
	// reason a turn cannot run.
	latency latency.Recorder

	// continuationRunner, when set, is how an auto-woken continuation is
	// dispatched: through the coordinator, so the turn gets the runtime,
	// provider options, token refresh and MCP wait a prompted turn gets.
	// Only the coordinator's own agent has one; anything else falls back
	// to a plain Run. See startContinuation and coordinator.runContinuation.
	continuationRunner func(ctx context.Context, sessionID string) error
	// continuationContext is the coordinator lifecycle context. It keeps
	// retry work independent from a short-lived triggering call while making
	// it stop when the coordinator closes.
	continuationContext func() context.Context

	// subReady is the readiness group of a sub-agent build: the system
	// prompt and tool list are assembled off the build's own goroutine,
	// and a delegation that starts before they land runs with neither.
	// Only set for sub-agents — the coordinator's own agent is waited on
	// by run()'s preamble instead. See waitReady, and buildAgent for why
	// each sub-agent build gets a group of its own.
	subReady *errgroup.Group

	// dispatcher owns the accept/queue/cancel protocol state shared by Run
	// and Summarize's dispatch handoffs. Embedded so dispatcher's pure
	// pass-through methods are promoted onto SessionAgent's method set
	// without a forwarding wrapper for each - see dispatch.go.
	*dispatcher
}

type SessionAgentOptions struct {
	Model                Model
	SystemPromptPrefix   string
	SystemPrompt         string
	IsSubAgent           bool
	DisableAutoSummarize bool
	AutoSummarizeAt      int64
	Sessions             sessionstore.Service
	Messages             MessageService
	Tools                []fantasy.AgentTool
	Notify               pubsub.Publisher[notify.Notification]
	RunComplete          pubsub.Publisher[notify.RunComplete]
	MCP                  *mcp.Registry
	// Latency is optional; see sessionAgent.latency.
	Latency latency.Recorder
}

func NewSessionAgent(
	opts SessionAgentOptions,
) SessionAgent {
	// SessionAgentOptions builds an uncached runtime. Apply the fixed policy
	// before publishing its tools to any Run.
	if len(opts.Tools) > 0 {
		opts.Tools[len(opts.Tools)-1].SetProviderOptions(cacheControlOptions())
	}
	a := &sessionAgent{
		model:                csync.NewValue(opts.Model),
		systemPromptPrefix:   csync.NewValue(opts.SystemPromptPrefix),
		systemPrompt:         csync.NewValue(opts.SystemPrompt),
		isSubAgent:           opts.IsSubAgent,
		sessions:             opts.Sessions,
		messages:             opts.Messages,
		disableAutoSummarize: opts.DisableAutoSummarize,
		autoSummarizeAt:      opts.AutoSummarizeAt,
		tools:                csync.NewSliceFrom(opts.Tools),
		notify:               opts.Notify,
		runComplete:          opts.RunComplete,
		mcp:                  opts.MCP,
		latency:              opts.Latency,
		dispatcher:           newDispatcher(),
	}
	// Wired after construction since the hook closes over a: dispatch
	// itself must stay free of any dependency on a or on pubsub (see
	// dispatcher's doc comment).
	a.onQueueChanged = a.publishQueueChanged
	return a
}

// AcceptedRun owns exactly one accept reservation taken by
// BeginAccepted. It is the only carrier of accept-state across the
// AgentDispatcher.run / Coordinator.Run / sessionAgent.Run layers: a
// counter > 0 means a dispatched prompt is in flight and has not yet
// completed the dispatch handoff in Run. Close is the only way to
// release the reservation and is idempotent.
//
// AcceptedRun and BeginAccepted/endAccepted live in dispatch.go as part of
// the dispatcher type.

// recordLatency records one observed handoff wait, if a recorder was
// wired. It exists so the two call sites in turn.go can stay a single
// line each and need no nil check of their own.
func (a *sessionAgent) recordLatency(ctx context.Context, kind latency.Kind, sessionID string, waited time.Duration) {
	if a.latency == nil {
		return
	}
	a.latency.Record(ctx, kind, sessionID, waited)
}

// publishRunComplete emits the authoritative terminal event for a turn.
// It honors the per-call OnComplete hook when set (so the coordinator can
// coalesce retries) and otherwise falls back to the RunComplete broker.
// ctx is used only for the bounded-blocking must-deliver publish; the
// terminal payload is supplied by the caller. This is the single emit path
// shared by the streaming defer and the cancel-on-entry early return so a
// caller waiting on RunComplete (e.g. `sennit run` with a RunID) always
// observes exactly one terminal event regardless of which Run branch ends
// the turn.
func (a *sessionAgent) publishRunComplete(ctx context.Context, call SessionAgentCall, complete notify.RunComplete) {
	if call.OnComplete != nil {
		call.OnComplete(complete)
		return
	}
	if a.runComplete == nil {
		return
	}
	a.runComplete.PublishMustDeliver(ctx, pubsub.UpdatedEvent, complete)
}

func (a *sessionAgent) SetModel(model Model) {
	a.model.Set(model)
}

func (a *sessionAgent) SetTools(tools []fantasy.AgentTool) {
	a.tools.SetSlice(tools)
}

func (a *sessionAgent) SetSystemPrompt(systemPrompt string) {
	a.systemPrompt.Set(systemPrompt)
}

func (a *sessionAgent) Model() Model {
	return a.model.Get()
}

// waitReady blocks until this agent's build has finished assembling its
// system prompt and tools, or ctx ends. A nil group (the coordinator's own
// agent, or any agent built without one) is already ready.
//
// errgroup.Wait is safe to call from more than one goroutine here: every
// Add happened before the group was handed over, and each caller gets the
// same stored error.
func (a *sessionAgent) waitReady(ctx context.Context) error {
	if a == nil || a.subReady == nil {
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- a.subReady.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// runtimeSnapshot returns the system prompt and tool set the agent is
// currently configured to run with, for callers that need their concrete
// sizes before a turn starts. The carry-over budget is the main one: it
// has to size the carried history against the *actual* prompt and schemas
// this call will send, not against a guess, so it reads these after the
// build has landed (waitReady) rather than before.
//
// It returns the agent's own fields, not a call.Runtime: a sub-agent
// delegation is always run with the agent's own prompt and tools (its
// SessionAgentCall has no Runtime pinned), so the agent's fields are
// exactly what the next Run will send. A nil receiver yields empty values,
// matching waitReady's nil-safety.
func (a *sessionAgent) runtimeSnapshot(call SessionAgentCall) (systemPrompt string, tools []fantasy.AgentTool) {
	if a == nil {
		return "", nil
	}
	runtime := a.effectiveStreamRuntime(call)
	// The prefix is sent as a separate system message during prompt preparation,
	// but consumes the same context window as the base prompt.
	return runtime.systemPrompt + runtime.systemPromptPrefix, runtime.tools
}

// callModel is the model this call actually runs on: the runtime the
// coordinator resolved for it, if there is one, and the agent's own copy
// otherwise.
//
// The distinction matters wherever a turn is described rather than run —
// the model pinned on the session, the assistant record written for a
// cancelled turn, the telemetry — because a.model is replaced by
// UpdateModels while a turn is in flight, so those all attributed the
// turn to whatever the instance had switched to since.
func (a *sessionAgent) callModel(call SessionAgentCall) Model {
	if call.Runtime != nil {
		return call.Runtime.model
	}
	return a.model.Get()
}
