package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"charm.land/fantasy"

	"github.com/rave-soft/sennit/internal/agent/tools"
)

// subAgentParams holds the parameters for running a sub-agent.
type subAgentParams struct {
	Agent          SessionAgent
	SessionID      string
	ChildSessionID string
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
}

// runSubAgent runs a sub-agent and handles session management and cost accumulation.
// It creates a sub-session, runs the agent with the given prompt, and propagates
// the cost to the parent session.
func (c *coordinator) runSubAgent(ctx context.Context, params subAgentParams) (fantasy.ToolResponse, error) {
	var sessionID string
	if params.ChildSessionID != "" {
		sessionID = params.ChildSessionID
	} else {
		agentToolSessionID := c.sessions.CreateAgentToolSessionID(params.AgentMessageID, params.ToolCallID)
		session, err := c.sessions.CreateSubAgentSession(ctx, agentToolSessionID, params.SessionID, params.SessionTitle, params.AgentID)
		if err != nil {
			return fantasy.ToolResponse{}, fmt.Errorf("create session: %w", err)
		}
		sessionID = session.ID
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
		captured := snap.snapshotStreamRuntime(SessionAgentCall{SessionID: sessionID})
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
		systemPrompt, tools := snap.runtimeSnapshot(SessionAgentCall{SessionID: sessionID})
		budgetIn.SystemPromptBytes = len(systemPrompt)
		budgetIn.ToolSchemaBytes = toolSchemaBytes(tools)
	}

	priorMessages, err := c.carryOverMessages(ctx, budgetIn, params.SessionID, params.AgentID, sessionID)
	if err != nil {
		slog.Warn(
			"Failed to carry over sub-agent history; running without it",
			"agent", params.AgentID,
			"parent_session", params.SessionID,
			"child_session", sessionID,
			"error", err,
		)
	}

	// Call session setup function if provided
	if params.SessionSetup != nil {
		params.SessionSetup(sessionID)
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
			SessionID:        sessionID,
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

	// Report the child session as busy for as long as it is running:
	// nothing else can, since the delegate's dispatcher is not the one
	// the coordinator asks. See markSubSessionBusy. Exactly one of the
	// two branches below calls releaseBusy, and each calls it exactly
	// once.
	releaseBusy := c.markSubSessionBusy(sessionID)
	result, err := run(ctx)
	releaseBusy()
	// Legacy direct callers still own synchronous cost propagation. Task
	// launches provide ChildSessionID and are finalized atomically by the store.
	if params.ChildSessionID == "" {
		if costErr := c.updateParentSessionCost(context.WithoutCancel(ctx), sessionID, params.SessionID); costErr != nil {
			slog.Warn("Failed to update parent session cost", "child_session", sessionID, "parent_session", params.SessionID, "error", costErr)
		}
	}
	return c.finishSubAgent(subAgentOutcome{result: result, err: err}), nil
}

func (c *coordinator) subAgentTaskRun(parentSessionID, childSessionID, prompt string, agent SessionAgent) func(context.Context) (tools.TaskRunResult, error) {
	return func(ctx context.Context) (tools.TaskRunResult, error) {
		resp, err := c.runSubAgent(ctx, subAgentParams{
			Agent:          agent,
			SessionID:      parentSessionID,
			ChildSessionID: childSessionID,
			Prompt:         prompt,
		})
		if err != nil {
			return tools.TaskRunResult{}, err
		}
		if resp.IsError {
			return tools.TaskRunResult{}, errors.New(resp.Content)
		}
		return tools.TaskRunResult{Text: resp.Content}, nil
	}
}

type subAgentOutcome struct {
	result *fantasy.AgentResult
	err    error
}

// finishSubAgent turns a completed child run into a terminal task result.
// Asynchronous task runs are finalized transactionally by thread.lifecycle;
// this synchronous helper intentionally does not attribute cost.
func (c *coordinator) finishSubAgent(outcome subAgentOutcome) fantasy.ToolResponse {
	if outcome.err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to generate response: %s", outcome.err))
	}
	output := subAgentOutput(outcome.result)
	if output == "" {
		return fantasy.NewTextErrorResponse("Sub-agent completed but produced no text output.")
	}
	return fantasy.NewTextResponse(output)
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
