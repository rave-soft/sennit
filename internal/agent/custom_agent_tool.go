package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"charm.land/fantasy"

	"github.com/rave-soft/braid/internal/agent/prompt"
	"github.com/rave-soft/braid/internal/agent/tools"
	"github.com/rave-soft/braid/internal/config"
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
func (c *coordinator) customAgentTools(ctx context.Context) ([]fantasy.AgentTool, error) {
	cfg := c.cfg.Config()

	// Deterministic order: the tool list feeds the model's prompt, and a map
	// iteration would reshuffle it between runs and defeat prompt caching.
	ids := make([]string, 0, len(cfg.Agents))
	for id := range cfg.Agents {
		if id == config.AgentCoder || id == config.AgentTask {
			continue
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)

	agentTools := make([]fantasy.AgentTool, 0, len(ids))
	for _, id := range ids {
		agentCfg := cfg.Agents[id]

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

	return fantasy.NewParallelAgentTool(
		id,
		customAgentDescription(id, agentCfg),
		func(ctx context.Context, params CustomAgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Prompt == "" {
				return fantasy.NewTextErrorResponse("prompt is required"), nil
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
			})
		},
	), nil
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
