package agent

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"charm.land/fantasy"
	"golang.org/x/sync/errgroup"

	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/pubsub"
)

// turnDispatcher owns the turn lifecycle of the coordinator's own agent:
// the per-session accept/queue/cancel state (through the dispatcher each
// sessionAgent embeds), the readiness work buildAgent starts and the Close
// protocol that waits for it, and the top-level Run/Steer/Summarize entry
// points that resolve a runtime and hand the call to the current agent.
//
// It does not build models, tools, or providers (runtimeBuilder's job) and
// does not run or finalize delegations (delegationFinalizer's job); it is
// the seam that orders those two behind the session's own dispatch
// protocol.
type turnDispatcher struct {
	*agentDeps

	builder    *runtimeBuilder
	delegation *delegationFinalizer

	agentPort *coordinatorAgentPort

	lifecycle *readinessLifecycle

	// lastActivity records, per session this dispatcher has run work
	// for, when that session was last busy. It is the clock the idle
	// summarize sweep reads (see idle_summarize.go) and the only state
	// that pass owns. Entries are created by markActivity and dropped
	// only when the session turns out to be gone, so the map is bounded
	// by the sessions one process actually drives.
	lastActivity *csync.Map[string, time.Time]
	// summarizeIdle is the action half of the idle sweep, split from the
	// decision half so a test can drive the policy (thresholds, the idle
	// clock, the busy and config checks) without a provider behind it.
	// nil — always, in production — means d.Summarize.
	summarizeIdle func(ctx context.Context, sessionID string) error
	// idleSummarizeGroup is the errgroup the idle sweep goroutine runs
	// in. It is separate from lifecycle.primary deliberately: the sweep
	// only ends at Close, and waitPrimary — which every turn calls
	// before it can run — would then never return.
	idleSummarizeGroup errgroup.Group
}

// Close cancels the coordinator's background readiness work (buildAgent's
// async system-prompt/tool-list setup, including sub-agent rebuilds on
// every run) and waits for it to actually stop, bounded by ctx. This is
// what keeps the git/MCP subprocesses that work may spawn (see
// internal/agent/prompt) from outliving the coordinator — production
// callers wire it into App.Shutdown; tests that build a coordinator
// directly should call it in their own teardown for the same reason.
// Safe to call even if buildAgent was never invoked, and safe to call
// concurrently or more than once: every call waits on the same
// closeDone, but only the first ever starts readyGroup.Wait.
func (d *turnDispatcher) Close(ctx context.Context) error {
	return d.lifecycle.close(ctx)
}

// buildAgent assembles a session agent for agent: its model (inherited or
// its own, see buildAgentModel) and, on this
// dispatcher's own readiness goroutines, its system prompt and tool list.
// The returned agent is not ready to run until those goroutines finish:
// the coordinator's own agent is waited on by run()'s preamble through
// readyWg, and sub-agents carry a subReady group the delegation path
// waits on.
// runUpdateModels resolves the compiled runtime and hands its model, tools
// and system prompt to agent. It owns the resolve-and-hand-off because the
// builder must not reach into the dispatcher's current agent through a
// stored pointer: it is wired into the builder as a callback, and it is
// what a config reload or credential refresh uses to pick up the new
// generation.
func (d *turnDispatcher) runUpdateModels(ctx context.Context, agent SessionAgent) error {
	return d.builder.UpdateModels(ctx, agent, d.delegation.runtimeInputs())
}

// refreshRuntimeToken refreshes an expired OAuth token for runtime's
// provider and, if that refresh landed a new generation, re-resolves
// runtime so the caller proceeds on it instead of the one it already
// compiled. A refresh failure is logged under logMsg rather than
// returned: every call site proceeds with the existing token regardless,
// and only a re-resolve failure is reported to the caller.
func (d *turnDispatcher) refreshRuntimeToken(ctx context.Context, runtime *compiledRuntime, logMsg string) (*compiledRuntime, error) {
	if err := d.builder.refreshTokenIfExpired(ctx, runtime.providerCfg, runtime.providerCredentials, d.delegation.operationPort()); err != nil {
		slog.Error(logMsg, "error", err)
		return runtime, nil
	}
	if d.builder.runtimeKey() == runtime.key {
		return runtime, nil
	}
	return d.builder.runtimeFor(ctx, d.delegation.runtimeInputs())
}

// Summarize implements Coordinator: it resolves the runtime the summary
// request replays through, refreshes an expired OAuth token first, and
// hands the call to the current agent's own summarize pass.
func (d *turnDispatcher) Summarize(ctx context.Context, sessionID string) error {
	// A summarize is work on the session, whoever asked for it: the idle
	// sweep must not fire on top of one a person just ran by hand.
	d.markActivity(sessionID)
	defer d.markActivity(sessionID)
	runtime, err := d.builder.runtimeFor(ctx, d.delegation.runtimeInputs())
	if err != nil {
		return err
	}
	runtime, err = d.refreshRuntimeToken(ctx, runtime, "Failed to refresh OAuth2 token before summarize. Proceeding with existing token.")
	if err != nil {
		return err
	}
	active := newActiveRuntime(runtime)

	// The summary request replays the same conversation prefix the turns
	// did, so it wants the same routing (see withPromptCacheKey) — and it
	// is the single most expensive request a session makes, since a
	// summary is only asked for once the context is full.
	summaryOptions := withPromptCacheKey(runtime.providerOptions, runtime.model, runtime.providerCfg, sessionID)
	agent := d.agentPort.current()
	if sa, ok := agent.(*sessionAgent); ok {
		return sa.summarize(ctx, sessionID, summaryOptions, d.builder.makeAuthRefreshCallback(runtime.providerCfg, runtime.providerCredentials, active, d.delegation.operationPort()), runtime.model, runtime.systemPromptPrefix, active, nil)
	}
	return agent.Summarize(ctx, sessionID, summaryOptions, d.builder.makeAuthRefreshCallback(runtime.providerCfg, runtime.providerCredentials, active, d.delegation.operationPort()))
}

// GenerateTitle generates a session title using the current agent, with
// the model and prefix the runtime was compiled from.
func (d *turnDispatcher) GenerateTitle(ctx context.Context, sessionID, prompt string) {
	agent := d.agentPort.current()
	if agent == nil {
		return
	}
	runtime, err := d.builder.runtimeFor(ctx, d.delegation.runtimeInputs())
	if err != nil {
		slog.Error("Failed to prepare agent runtime for title", "error", err)
		return
	}
	if sa, ok := agent.(*sessionAgent); ok {
		sa.generateTitle(ctx, sessionID, prompt, runtime.model, runtime.systemPromptPrefix)
		return
	}
	agent.GenerateTitle(ctx, sessionID, prompt)
}

// runContinuation dispatches an auto-woken continuation turn for
// sessionID through the same preparation a prompted turn gets: MCP
// initialization waited out, a runtime resolved from the current config,
// an expired OAuth token refreshed first, and the model's own call
// options carried onto the call.
//
// It exists because dispatching straight to sessionAgent.Run with
// nothing but the session id and a placeholder prompt would carry no
// Runtime (no thinking options, no output-token budget), no
// OnAuthRefresh (an OAuth token expired while a delegation ran would 401
// with no retry), and no MCP wait (a continuation could wake without the
// tools it needed). Every one of those is most likely precisely when a
// continuation fires — long after the turn that started the delegation.
func (d *turnDispatcher) runContinuation(ctx context.Context, sessionID string) error {
	d.markActivity(sessionID)
	defer d.markActivity(sessionID)
	if err := d.lifecycle.waitPrimary(); err != nil {
		return err
	}
	if err := d.builder.waitForMCPInit(ctx); err != nil {
		return fmt.Errorf("failed to wait for MCP initialization: %w", err)
	}

	runtime, err := d.builder.runtimeFor(ctx, d.delegation.runtimeInputs())
	if err != nil {
		return fmt.Errorf("failed to prepare agent runtime: %w", err)
	}
	runtime, err = d.refreshRuntimeToken(ctx, runtime, "Failed to refresh OAuth2 token for a continuation. Proceeding with existing token.")
	if err != nil {
		return fmt.Errorf("failed to prepare refreshed agent runtime: %w", err)
	}
	active := newActiveRuntime(runtime)

	_, err = d.agentPort.current().Run(ctx, d.makeRunCall(SessionAgentCall{
		Runtime:       runtime,
		ActiveRuntime: active,
		SessionID:     sessionID,
		Prompt:        continuationPromptPlaceholder,
		Continuation:  true,
	}))
	return err
}

// makeRunCall carries the per-run model options resolved from runtime onto
// the SessionAgentCall, plus the auth-refresh callback that re-resolves them
// after a successful credential refresh. Every SessionAgentCall assembled
// from a runtime goes through this one point, so the options cannot drift
// from the runtime they were resolved out of.
func (d *turnDispatcher) makeRunCall(call SessionAgentCall) SessionAgentCall {
	runtime := call.Runtime
	call.MaxOutputTokens = runtime.maxOutputTokens
	call.ProviderOptions = withPromptCacheKey(runtime.providerOptions, runtime.model, runtime.providerCfg, call.SessionID)
	call.Temperature = runtime.temperature
	call.TopP = runtime.topP
	call.TopK = runtime.topK
	call.FrequencyPenalty = runtime.frequencyPenalty
	call.PresencePenalty = runtime.presencePenalty
	port := d.delegation.operationPort()
	call.OnAuthRefresh = d.builder.makeAuthRefreshCallback(runtime.providerCfg, runtime.providerCredentials, call.ActiveRuntime, port)
	// Both rotation triggers are wired here, at the primary
	// per-turn call site: an interactive turn hitting a 429 mid-conversation,
	// or crossing its usage threshold between steps, is exactly the case
	// rotation exists for. Each returns nil when rotation is disabled or
	// the provider's RotateOn doesn't match (see rotatorFor), so this is a
	// complete no-op for every call until a provider opts in.
	call.OnRateLimit = d.builder.makeRateLimitCallback(runtime.providerCfg, runtime.providerCredentials, call.ActiveRuntime, port)
	call.RotateThreshold = d.builder.makeThresholdRotateCallback(runtime.providerCfg, runtime.providerCredentials, call.ActiveRuntime, port)
	return call
}

// run is the shared implementation behind the coordinator's Run and
// RunAccepted entry points. When accept is non-nil it is threaded onto the
// SessionAgentCall as Accepted so sessionAgent.Run can consume the accept
// reservation under dispatchMu; when nil (the in-process/local path) no
// accept tracking applies.
func (d *turnDispatcher) run(ctx context.Context, accept *AcceptedRun, sessionID string, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	// Both ends of the turn count as activity for the idle summarize
	// sweep: the entry mark covers a turn that outlives the idle window,
	// and the deferred one restarts the clock from the moment the
	// session actually went quiet. See markActivity.
	d.markActivity(sessionID)
	defer d.markActivity(sessionID)
	if err := d.lifecycle.waitPrimary(); err != nil {
		return nil, err
	}

	// Wait for MCP initialization to complete before building the tool list.
	// Without this, slow-to-start MCP servers (e.g. stdio Python via uv) may
	// not have registered their tools yet when buildTools reads the registry,
	// so their tools silently never appear in the LLM tool palette — even
	// though sennit_info reports them as connected.
	if err := d.builder.waitForMCPInit(ctx); err != nil {
		return nil, fmt.Errorf("failed to wait for MCP initialization: %w", err)
	}

	runtime, err := d.builder.runtimeFor(ctx, d.delegation.runtimeInputs())
	if err != nil {
		return nil, fmt.Errorf("failed to prepare agent runtime: %w", err)
	}

	// We don't return here because the event handling to ask the user to reauthenticate
	// depends on the flow below. If refresh fails, proceed with the token we have.
	runtime, err = d.refreshRuntimeToken(ctx, runtime, "Failed to refresh OAuth2 token. Proceeding with existing token.")
	if err != nil {
		return nil, fmt.Errorf("failed to prepare refreshed agent runtime: %w", err)
	}
	active := newActiveRuntime(runtime)

	// Coalesce per-attempt RunComplete payloads so only the final
	// outcome reaches subscribers. Without this, the first attempt's
	// failed RunComplete (unauthorized) would race ahead of the
	// retry's success, and `sennit run` would exit on the stale error
	// before ever seeing the retry result. Each attempt's
	// SessionAgentCall.OnComplete hook overwrites latest; we publish
	// exactly once after retries resolve, via PublishMustDeliver, so
	// a momentarily-full subscriber buffer can't silently drop the
	// terminal event.
	var (
		latest    notify.RunComplete
		hasLatest bool
	)
	onComplete := func(rc notify.RunComplete) {
		latest = rc
		hasLatest = true
	}
	// Propagate the caller-supplied RunID (set via agent.WithRunID
	// at the dispatch boundary in AgentDispatcher.Send) onto the
	// SessionAgentCall so the terminal RunComplete event echoes it
	// back. Both attempts in the retry chain reuse the same RunID;
	// the coalesce closure publishes the final outcome under that
	// same correlator.
	runID := RunIDFromContext(ctx)
	promptOrigin := PromptOriginFromContext(ctx)
	// A steering dispatch (agent.WithSteering) asks for this prompt to
	// reach a turn already in flight rather than queue behind it. run()
	// below is called exactly once: d.run has no auth-retry loop of its
	// own - a mid-stream auth failure is retried inside
	// sessionAgent.Run's own Stream call via OnAuthRefresh, never by
	// calling run() again here. dispatchDecision already guards its own
	// call to onDispatch with a sync.Once (see dispatchOutcome's
	// "dispatched" closure in run_turn.go), so wrapping it a second time
	// here was redundant - removed rather than left as defensive
	// duplication that suggested a second call site which does not exist.
	onDispatch, steering := SteeringFromContext(ctx)
	run := func() (*fantasy.AgentResult, error) {
		return d.agentPort.current().Run(ctx, d.makeRunCall(SessionAgentCall{
			Runtime:       runtime,
			ActiveRuntime: active,
			SessionID:     sessionID,
			RunID:         runID,
			Prompt:        prompt,
			PromptOrigin:  promptOrigin,
			Steering:      steering,
			OnDispatch:    onDispatch,
			Attachments:   attachments,
			OnComplete:    onComplete,
			Accepted:      accept,
		}))
	}
	_, activeSkillsSnapshot, skillTrackerSnapshot := d.delegation.skillsSnapshot()
	beforeLoaded := skillTrackerSnapshot.LoadedNames()
	result, originalErr := run()
	logTurnSkillUsage(sessionID, prompt, activeSkillsSnapshot, skillTrackerSnapshot, beforeLoaded)

	// Notify only if still unauthorized after retry — a successful
	// retry means the user doesn't need to re-authenticate. AWS SSO is
	// handled transparently inside OnAuthRefresh, so it needs no post-run
	// notification here.
	if hasLatest && d.runComplete != nil {
		// Detached, with a bounded deadline of its own: this is the
		// authoritative terminal event, and the commonest reason to be
		// publishing it is that the run was cancelled — which cancels
		// ctx too, so publishing on it dropped the very event a caller
		// waiting on this RunID needs. The deferred publisher inside
		// sessionAgent.run already detaches for the same reason.
		publishCtx, cancelPublish := context.WithTimeout(context.WithoutCancel(ctx), runCompletePublishTimeout)
		d.runComplete.PublishMustDeliver(publishCtx, pubsub.UpdatedEvent, latest)
		cancelPublish()
		// Signal to the dispatcher (AgentDispatcher.run) that the
		// authoritative terminal RunComplete for this run was already
		// emitted, so it does not publish a duplicate fallback for the
		// error it is about to receive.
		MarkRunCompletePublished(ctx)
	}
	return result, originalErr
}

// BeginAccepted reserves an accept slot for sessionID on the active
// agent and returns the ownership handle. It is the fire-and-forget
// dispatch path's only way to mark a run as accepted-but-not-yet-active
// so a cancel arriving before the run registers in activeRequests is not
// lost.
func (d *turnDispatcher) BeginAccepted(sessionID string) *AcceptedRun {
	return d.agentPort.current().BeginAccepted(sessionID)
}

// Steer implements Coordinator.
func (d *turnDispatcher) Steer(ctx context.Context, call SessionAgentCall) (SteerOutcome, *fantasy.AgentResult, error) {
	d.markActivity(call.SessionID)
	defer d.markActivity(call.SessionID)
	return d.agentPort.current().Steer(ctx, call)
}

func (d *turnDispatcher) Cancel(sessionID string) {
	d.agentPort.current().Cancel(sessionID)
}

func (d *turnDispatcher) CancelAll() {
	d.agentPort.current().CancelAll()
}

func (d *turnDispatcher) ClearQueue(sessionID string) {
	d.agentPort.current().ClearQueue(sessionID)
}

func (d *turnDispatcher) Model() Model {
	return d.agentPort.current().Model()
}

func (d *turnDispatcher) QueuedPrompts(sessionID string) int {
	return d.agentPort.current().QueuedPrompts(sessionID)
}

func (d *turnDispatcher) QueuedPromptsList(sessionID string) []string {
	return d.agentPort.current().QueuedPromptsList(sessionID)
}
