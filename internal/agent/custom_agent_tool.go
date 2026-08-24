package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"charm.land/fantasy"

	"github.com/rave-soft/sennit/internal/agent/prompt"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/config"
)

// CustomAgentParams is the input schema for every user-defined agent tool.
// It deliberately mirrors AgentParams: the caller describes the task in prose
// and the delegate decides how to carry it out.
type CustomAgentParams struct {
	Prompt string `json:"prompt" description:"The task for the agent to perform"`
}

// customAgentTools builds one delegation tool per user-defined agent.
//
// Each tool is registered under the agent's id, so a config entry named
// "reviewer" gives the coder a `reviewer` tool. Tools are parallel-capable:
// fantasy may run several delegations from a single assistant turn
// concurrently, and each one gets its own sub-session.
//
// Sub-agents never receive these tools — see the isSubAgent guard in
// buildTools — which keeps delegation one level deep and makes the build
// terminate: building a role's tool list can never recurse into building
// another role.
func (c *coordinator) customAgentTools(ctx context.Context, cfg agentConfig) ([]fantasy.AgentTool, error) {
	agents := cfg.Agents()

	// Deterministic order: the tool list feeds the model's prompt, and a map
	// iteration would reshuffle it between runs and defeat prompt caching.
	ids := make([]string, 0, len(agents))
	for id := range agents {
		if id == config.AgentCoder || id == config.AgentTask {
			continue
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)

	agentTools := make([]fantasy.AgentTool, 0, len(ids))
	for _, id := range ids {
		agentCfg := agents[id]

		tool, err := c.buildCustomAgentTool(ctx, id, agentCfg)
		if err != nil {
			return nil, fmt.Errorf("build agent tool %q: %w", id, err)
		}
		agentTools = append(agentTools, tool)
	}
	return agentTools, nil
}

func (c *coordinator) buildCustomAgentTool(ctx context.Context, id string, agentCfg config.Agent) (fantasy.AgentTool, error) {
	systemPrompt, err := prompt.NewPrompt(
		id,
		agentCfg.Prompt,
		prompt.WithWorkingDir(c.cfg.WorkingDir()),
	)
	if err != nil {
		return nil, fmt.Errorf("parse prompt: %w", err)
	}

	agent, err := c.buildAgent(ctx, systemPrompt, agentCfg, true)
	if err != nil {
		return nil, err
	}

	// The delegate above is pinned to the agent definition as of this
	// build — its model in particular. The tool list is compiled once per
	// runtime and a runtime lives for a whole turn, so an agent edited
	// mid-turn (its model switched in .sennit/agents while a long pipeline
	// is delegating to it over and over) would otherwise keep running on
	// the old definition until the turn ended, while the config — and the
	// chat's label for the delegation, which reads it — already named the
	// new one. Check at call time and rebuild the delegate when the
	// definition moved; the common case is a cheap comparison.
	var rebuildMu sync.Mutex
	current := agentCfg
	delegate := func(ctx context.Context) (SessionAgent, error) {
		rebuildMu.Lock()
		defer rebuildMu.Unlock()
		latest, ok := c.cfg.Config().Agents[id]
		if !ok || sameAgentDefinition(latest, current) {
			return agent, nil
		}
		latestPrompt, err := prompt.NewPrompt(id, latest.Prompt, prompt.WithWorkingDir(c.cfg.WorkingDir()))
		if err != nil {
			return nil, fmt.Errorf("parse prompt: %w", err)
		}
		rebuilt, err := c.buildAgent(ctx, latestPrompt, latest, true)
		if err != nil {
			return nil, err
		}
		agent, current = rebuilt, latest
		return agent, nil
	}

	return fantasy.NewParallelAgentTool(
		id,
		customAgentDescription(id, agentCfg),
		func(ctx context.Context, params CustomAgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Prompt == "" {
				return fantasy.NewTextErrorResponse("prompt is required"), nil
			}
			agent, err := delegate(ctx)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("agent %q: %w", id, err)
			}

			sessionID := tools.GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, errors.New("session id missing from context")
			}

			agentMessageID := tools.GetMessageFromContext(ctx)
			if agentMessageID == "" {
				return fantasy.ToolResponse{}, errors.New("agent message id missing from context")
			}

			return c.runSubAgent(ctx, subAgentParams{
				Agent:          agent,
				SessionID:      sessionID,
				AgentMessageID: agentMessageID,
				ToolCallID:     call.ID,
				Prompt:         params.Prompt,
				SessionTitle:   agentCfg.Name,
				AgentID:        id,
				Detachable:     true,
			})
		},
	), nil
}

// sameAgentDefinition reports whether two definitions of an agent would
// build the same delegate: the fields buildAgent and the delegate's system
// prompt read. Description is deliberately left out — it only feeds the
// tool's own description, which the calling model has already been shown
// for this turn.
func sameAgentDefinition(a, b config.Agent) bool {
	return a.Model == b.Model &&
		a.ReasoningEffort == b.ReasoningEffort &&
		a.Prompt == b.Prompt &&
		slices.Equal(a.AllowedTools, b.AllowedTools) &&
		slices.Equal(a.ContextPaths, b.ContextPaths) &&
		maps.EqualFunc(a.AllowedMCP, b.AllowedMCP, slices.Equal)
}

// customAgentDescription is what the calling model reads when deciding whether
// to delegate, so an agent without a description still needs something better
// than an empty string.
func customAgentDescription(id string, agentCfg config.Agent) string {
	if agentCfg.Description != "" {
		return agentCfg.Description
	}
	name := agentCfg.Name
	if name == "" {
		name = id
	}
	return fmt.Sprintf("Delegate a task to the %s agent.", name)
}
