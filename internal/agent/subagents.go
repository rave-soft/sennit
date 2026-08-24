package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"charm.land/fantasy"

	"github.com/rave-soft/sennit/internal/agent/tools"
)

// subAgentParams holds the parameters for running a sub-agent.
type subAgentParams struct {
	Agent          SessionAgent
	SessionID      string
	AgentMessageID string
	ToolCallID     string
	Prompt         string
	SessionTitle   string
	// AgentID is the configured id of a *named* agent (a `.claude/agents`
	// entry, or any config.Agents key other than coder/task). Setting it
	// is what makes this delegation part of that agent's continuing
	// conversation under this parent: the session is stamped with it and
	// the agent's earlier sessions are replayed ahead of this one.
	//
	// Left empty by the anonymous delegations - the built-in `agent` and
	// `agentic_fetch` tools - which stay stateless, one call to the next.
	AgentID string
	// SessionSetup is an optional callback invoked after session creation
	// but before agent execution, for custom session configuration.
	SessionSetup func(sessionID string)
	// Detachable marks a foreground delegation as eligible to detach
	// into the background if the person sends the parent session a new
	// message while it is still running - see canDetachSubAgent and the
	// select loop in runSubAgent. Set by the "agent" tool's foreground
	// branch and by custom agent tools; left false by agentic_fetch
	// (too short a call to be worth detaching) and by every background:
	// true dispatch (already non-blocking).
	Detachable bool
}

// runSubAgent runs a sub-agent and handles session management and cost accumulation.
// It creates a sub-session, runs the agent with the given prompt, and propagates
// the cost to the parent session.
func (c *coordinator) runSubAgent(ctx context.Context, params subAgentParams) (fantasy.ToolResponse, error) {
	// Create sub-session
	agentToolSessionID := c.sessions.CreateAgentToolSessionID(params.AgentMessageID, params.ToolCallID)
	session, err := c.sessions.CreateSubAgentSession(ctx, agentToolSessionID, params.SessionID, params.SessionTitle, params.AgentID)
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("create session: %w", err)
	}

	// The delegate is built with its system prompt and tools assembled on
	// the build's own goroutines; a delegation dispatched before those
	// land runs with an empty prompt and no tools at all. Sub-agents are
	// rebuilt on every runtime invalidation, so a tool call arriving just
	// after one is exactly when that happens.
	//
	// This wait happens *before* the carried history is assembled: the
	// carry-over budget is sized from the delegate's actual system prompt,
	// tool schemas and this delegation's prompt (see carryOverBudget), so
	// those have to be final first. Waiting here rather than after the
	// carry-over means the budget is computed from the real runtime, not
	// from a guess, and the run still waits on the same readiness group it
	// always did - nothing is waited on twice, because the group's Wait is
	// idempotent and cheap once it has resolved.
	if waiter, ok := params.Agent.(interface {
		waitReady(context.Context) error
	}); ok {
		if err := waiter.waitReady(ctx); err != nil {
			return fantasy.ToolResponse{}, fmt.Errorf("agent not ready: %w", err)
		}
	}

	// Capture one immutable runtime after readiness. The same value sizes
	// carry-over and drives Stream, so mutable agent/MCP state cannot drift
	// between those two operations.
	var runtime *streamRuntime
	if snap, ok := params.Agent.(interface {
		snapshotStreamRuntime(SessionAgentCall) streamRuntime
	}); ok {
		captured := snap.snapshotStreamRuntime(SessionAgentCall{SessionID: session.ID})
		runtime = &captured
	}

	// What this named agent already knows, from its earlier delegations
	// under the same parent. Collected after the session exists so the
	// query can exclude it by id, and treated as best-effort: a
	// delegation that has lost its memory is worse than one that
	// remembers, but far better than one that refuses to run.
	//
	// The budget is sized from the model and the concrete runtime: the
	// delegate's context window and output capacity, plus the actual byte
	// sizes of the system prompt, tool schemas and this delegation's
	// prompt, all read now that the build has landed.
	model := params.Agent.Model()
	if runtime != nil {
		model = runtime.model
	}
	budgetIn := carryOverBudgetInput{
		Model:             model,
		SystemPromptBytes: 0,
		ToolSchemaBytes:   0,
		PromptBytes:       len(params.Prompt),
	}
	if runtime != nil {
		budgetIn.SystemPromptBytes = len(runtime.systemPrompt) + len(runtime.systemPromptPrefix)
		budgetIn.ToolSchemaBytes = toolSchemaBytes(runtime.tools)
	} else if snap, ok := params.Agent.(interface {
		runtimeSnapshot(SessionAgentCall) (string, []fantasy.AgentTool)
	}); ok {
		systemPrompt, tools := snap.runtimeSnapshot(SessionAgentCall{SessionID: session.ID})
		budgetIn.SystemPromptBytes = len(systemPrompt)
		budgetIn.ToolSchemaBytes = toolSchemaBytes(tools)
	}

	priorMessages, err := c.carryOverMessages(ctx, budgetIn, params.SessionID, params.AgentID, session.ID)
	if err != nil {
		slog.Warn(
			"Failed to carry over sub-agent history; running without it",
			"agent", params.AgentID,
			"parent_session", params.SessionID,
			"child_session", session.ID,
			"error", err,
		)
	}

	// Call session setup function if provided
	if params.SessionSetup != nil {
		params.SessionSetup(session.ID)
	}

	// Get model configuration
	maxTokens := modelMaxOutputTokens(model)

	providerCfg, ok := c.cfg.Config().Providers.Get(model.ModelCfg.Provider)
	if !ok {
		return fantasy.ToolResponse{}, errModelProviderNotConfigured
	}

	// Run the agent. Takes its context explicitly - the non-detached path
	// below runs it with ctx directly, the detachable path with a child
	// context that can outlive ctx.
	run := func(runCtx context.Context) (*fantasy.AgentResult, error) {
		call := SessionAgentCall{
			SessionID:        session.ID,
			Prompt:           params.Prompt,
			PriorMessages:    priorMessages,
			MaxOutputTokens:  maxTokens,
			ProviderOptions:  getProviderOptions(model, providerCfg),
			Temperature:      model.ModelCfg.Temperature,
			TopP:             model.ModelCfg.TopP,
			TopK:             model.ModelCfg.TopK,
			FrequencyPenalty: model.ModelCfg.FrequencyPenalty,
			PresencePenalty:  model.ModelCfg.PresencePenalty,
			NonInteractive:   true,
			// Sub-agents don't track an active runtime of their own, so
			// there's nothing for a refresh to update.
			OnAuthRefresh: c.makeAuthRefreshCallback(providerCfg, nil),
		}
		if runtimeAgent, ok := params.Agent.(interface {
			runWithStreamRuntime(context.Context, SessionAgentCall, streamRuntime) (*fantasy.AgentResult, error)
		}); ok && runtime != nil {
			return runtimeAgent.runWithStreamRuntime(runCtx, call, *runtime)
		}
		return params.Agent.Run(runCtx, call)
	}

	// Detachment is only ever considered for delegations marked
	// Detachable, and only once every gate that governs background work
	// in general also clears - see canDetachSubAgent. detachSignal is
	// nil whenever detaching is off the table, which keeps the plain
	// (and by far the most common) path below byte-for-byte what it was
	// before this feature existed.
	var detachSignal <-chan struct{}
	if params.Detachable {
		detachSignal = c.canDetachSubAgent(ctx)
	}

	// Report the child session as busy for as long as it is running:
	// nothing else can, since the delegate's dispatcher is not the one
	// the coordinator asks. See markSubSessionBusy. Exactly one of the
	// two branches below calls releaseBusy, and each calls it exactly
	// once.
	if detachSignal == nil {
		releaseBusy := c.markSubSessionBusy(session.ID)
		result, err := run(ctx)
		releaseBusy()
		return c.finishSubAgent(ctx, session.ID, params.SessionID, subAgentOutcome{result: result, err: err}), nil
	}
	// Snapshotted now, off the parent ctx, because a detach (below)
	// replaces the context the child run continues under - the eventual
	// completion still needs the depth *this* turn ran at.
	depth := tools.GetDepthFromContext(ctx)
	return c.runDetachableSubAgent(ctx, run, session.ID, params, depth, detachSignal)
}

// canDetachSubAgent reports whether a foreground delegation running
// under ctx may detach into the background if the person sends the
// parent session a new message before it finishes, returning the signal
// to watch for that message (nil when detachment is not currently
// possible, in which case the caller must not attempt it). The first
// three gates mirror runBackgroundAgent's own (options.background_agents,
// the cascade-depth limit, and a wired task manager): those are product
// decisions about background work in general, and a detached delegation
// is background work just the same, even though nothing here creates a
// task-manager entry for it - see AgentDetachedResponseMetadata. The
// last gate is simply whether there is a signal to detach on at all.
func (c *coordinator) canDetachSubAgent(ctx context.Context) <-chan struct{} {
	if !c.backgroundAgentsEnabled() {
		return nil
	}
	if tools.GetDepthFromContext(ctx) >= maxTaskCascadeDepth {
		return nil
	}
	if c.tasksManager() == nil {
		return nil
	}
	return tools.WaitForUserInput(ctx)
}

// subAgentOutcome carries a child run's result off the goroutine that
// produced it to whichever of runSubAgent's paths ends up consuming it -
// the synchronous finish below, or deliverDetachedCompletion after a
// detach.
type subAgentOutcome struct {
	result *fantasy.AgentResult
	err    error
}

// runDetachableSubAgent runs a Detachable delegation whose gates all
// cleared. The child run always moves to its own goroutine, on a
// context that can survive this call returning: once detached, the
// parent's ctx is going away (the turn that made this tool call is
// about to finish), but the child must not go with it. Until an actual
// detach happens, though, the child still dies with its parent exactly
// like the non-detachable path does - a goroutine forwards ctx's
// cancellation to the child's cancel until detaching switches it off.
func (c *coordinator) runDetachableSubAgent(
	ctx context.Context,
	run func(context.Context) (*fantasy.AgentResult, error),
	childSessionID string,
	params subAgentParams,
	depth int,
	detachSignal <-chan struct{},
) (fantasy.ToolResponse, error) {
	childCtx, cancelChild := context.WithCancel(context.WithoutCancel(ctx))
	stopForwarding := make(chan struct{})
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { close(stopForwarding) }) }
	go func() {
		select {
		case <-ctx.Done():
			cancelChild()
		case <-stopForwarding:
		}
	}()

	releaseBusy := c.markSubSessionBusy(childSessionID)
	resultCh := make(chan subAgentOutcome, 1)
	go func() {
		result, err := run(childCtx)
		releaseBusy()
		resultCh <- subAgentOutcome{result: result, err: err}
	}()

	select {
	case outcome := <-resultCh:
		// The run finished before any detach happened: report it exactly
		// as the non-detachable path would, and let it go with the child
		// context - there is nothing left for detachSignal to interrupt.
		stop()
		cancelChild()
		return c.finishSubAgent(ctx, childSessionID, params.SessionID, outcome), nil

	case <-detachSignal:
		// A result that had already landed wins over detaching it: check
		// resultCh again, non-blocking, rather than letting select's own
		// random tie-break between two simultaneously ready cases decide
		// it - a delegation that is done has nothing left to detach.
		select {
		case outcome := <-resultCh:
			stop()
			cancelChild()
			return c.finishSubAgent(ctx, childSessionID, params.SessionID, outcome), nil
		default:
		}

		// Detach: the child now outlives this call, so parent
		// cancellation must no longer reach it, and delivering its
		// eventual result is handed off to a goroutine of its own.
		//
		// Registering it now, at the moment it actually detaches, gives
		// Cancel/CancelAll/Close a handle on a run that otherwise answers
		// to nothing: its context no longer descends from ctx (it was
		// built WithoutCancel above), so there is nothing left tracking
		// it but this registry entry - see coordinator's
		// detachedDelegations doc comment.
		stop()
		c.registerDetachedDelegation(childSessionID, params.SessionID, cancelChild)
		go func() {
			outcome := <-resultCh
			// childCtx is what deliverDetachedCompletion uses to update
			// the parent's cost and enqueue the completion, so it must
			// still be live for that - cancelChild only releases it
			// afterwards, once there is nothing left to do with it.
			// unregister runs before it, so the registry never briefly
			// claims to still own a delegation cancelChild has already
			// released.
			defer cancelChild()
			defer c.unregisterDetachedDelegation(childSessionID)
			c.deliverDetachedCompletion(childCtx, childSessionID, params, depth, outcome)
		}()
		text := fmt.Sprintf(
			"Delegation moved to the background because the person sent a new message (child session %s). It keeps running; its result will be delivered separately.",
			childSessionID,
		)
		return fantasy.WithResponseMetadata(fantasy.NewTextResponse(text), AgentDetachedResponseMetadata{SessionID: childSessionID}), nil

	case <-ctx.Done():
		// The parent turn ended before the person said anything, so this
		// delegation never detached. Cancellation is already forwarding
		// to the child; wait for it to actually stop and report exactly
		// as the non-detachable path would.
		outcome := <-resultCh
		stop()
		return c.finishSubAgent(ctx, childSessionID, params.SessionID, outcome), nil
	}
}

// finishSubAgent turns a completed child run into the tool result the
// non-detached paths return. The parent's cost is updated on success
// only, matching the behavior this replaced: a failed run has nothing
// settled worth attributing.
func (c *coordinator) finishSubAgent(ctx context.Context, childSessionID, parentSessionID string, outcome subAgentOutcome) fantasy.ToolResponse {
	if outcome.err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to generate response: %s", outcome.err))
	}

	// Update parent session cost on a best-effort basis. A failure here must
	// not discard the sub-agent output that was already produced.
	if err := c.updateParentSessionCost(ctx, childSessionID, parentSessionID); err != nil {
		slog.Warn(
			"Failed to update parent session cost",
			"child_session", childSessionID,
			"parent_session", parentSessionID,
			"error", err,
		)
	}

	output := subAgentOutput(outcome.result)
	if output == "" {
		return fantasy.NewTextErrorResponse("Sub-agent completed but produced no text output.")
	}
	return fantasy.NewTextResponse(output)
}

// deliverDetachedCompletion runs on its own goroutine, once a detached
// delegation's child run has finished, to update the parent's cost and
// deliver the result through the completion inbox - the only delivery
// path left, since the tool call that started this delegation already
// returned. ctx is the child's own recovered context (see
// runDetachableSubAgent), not the parent's, which is gone by the time
// this runs.
func (c *coordinator) deliverDetachedCompletion(ctx context.Context, childSessionID string, params subAgentParams, depth int, outcome subAgentOutcome) {
	if outcome.err == nil {
		if err := c.updateParentSessionCost(ctx, childSessionID, params.SessionID); err != nil {
			slog.Warn(
				"Failed to update parent session cost",
				"child_session", childSessionID,
				"parent_session", params.SessionID,
				"error", err,
			)
		}
	}

	completion := TaskCompletion{
		// A detached delegation has no task-manager record of its own,
		// so its child session id doubles as its delegation id - the
		// same identifier the completion also carries as
		// ChildSessionID, kept distinct in the struct because
		// formatTaskCompletion and the model-facing text treat them as
		// separate concepts for every other completion source.
		DelegationID:   childSessionID,
		Kind:           "delegation",
		Name:           params.SessionTitle,
		Goal:           params.Prompt,
		ChildSessionID: childSessionID,
		Depth:          depth,
		TerminalAt:     time.Now(),
	}
	if outcome.err != nil {
		if errors.Is(outcome.err, context.Canceled) {
			// Cancel/CancelAll/Close reached this delegation through the
			// detached-delegation registry (see cancelDetachedDelegations),
			// not a provider or tool failure - say so, rather than
			// reporting a person's own cancellation as if something broke.
			// The parent session is not wake-eligible right now either
			// way (Cancel sets its dispatcher's cancelled flag before
			// reaching here - see coordinator.Cancel - and
			// wakeEligibleLocked refuses to wake a cancelled session), so
			// this still reaches the model, just on the next real turn
			// rather than an auto-continuation.
			completion.Status = "interrupted"
			completion.Error = "Delegation was cancelled before it finished."
		} else {
			completion.Status = "failed"
			completion.Error = outcome.err.Error()
		}
	} else {
		completion.Status = "completed"
		output := subAgentOutput(outcome.result)
		if output == "" {
			output = "Sub-agent completed but produced no text output."
		}
		completion.ResultText = output
	}
	c.DeliverTaskCompletion(ctx, params.SessionID, completion)
}

func subAgentOutput(result *fantasy.AgentResult) string {
	if result == nil {
		return ""
	}
	return result.Response.Content.Text()
}

// updateParentSessionCost accumulates the cost from a child session to its
// parent session.
//
// The accumulation is a single narrow UPDATE (cost = cost + delta), which
// is what makes it safe against every other writer of that row. The
// Get-modify-Save it replaces did not just race sibling delegations — it
// wrote the whole row back, so a turn's usage save or a todo write that
// landed between the read and the write was overwritten with the values
// this call had read before them. parentCostMu is kept: two siblings
// still read the child cost and issue their updates concurrently, and
// serialising them keeps the published session events in a sensible
// order.
func (c *coordinator) updateParentSessionCost(ctx context.Context, childSessionID, parentSessionID string) error {
	c.parentCostMu.Lock()
	defer c.parentCostMu.Unlock()

	childSession, err := c.sessions.Get(ctx, childSessionID)
	if err != nil {
		return fmt.Errorf("get child session: %w", err)
	}

	if err := c.sessions.AddCost(ctx, parentSessionID, childSession.Cost); err != nil {
		return fmt.Errorf("get parent session: %w", err)
	}

	return nil
}
