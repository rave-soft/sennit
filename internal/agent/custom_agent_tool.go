package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"charm.land/fantasy"

	"github.com/rave-soft/sennit/internal/agent/prompt"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/config"
)

type CustomAgentParams struct {
	Prompt string `json:"prompt" description:"The task for the agent to perform"`
}

func (c *coordinator) customAgentTools(ctx context.Context, cfg agentConfig) ([]fantasy.AgentTool, error) {
	agents := cfg.Agents()
	ids := make([]string, 0, len(agents))
	for id := range agents {
		if id != config.AgentCoder && id != config.AgentTask {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	result := make([]fantasy.AgentTool, 0, len(ids))
	for _, id := range ids {
		tool, err := c.buildCustomAgentTool(ctx, id, agents[id])
		if err != nil {
			return nil, fmt.Errorf("build agent tool %q: %w", id, err)
		}
		result = append(result, tool)
	}
	return result, nil
}

//nolint:unparam // keeps the common tool-builder signature.
func (c *coordinator) buildCustomAgentTool(_ context.Context, id string, agentCfg config.Agent) (fantasy.AgentTool, error) {
	return fantasy.NewParallelAgentTool(
		id,
		customAgentDescription(id, agentCfg)+" The call returns immediately; correlate its later completion by task and child session id.",
		func(ctx context.Context, params CustomAgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Prompt == "" {
				return fantasy.NewTextErrorResponse("prompt is required"), nil
			}
			parentID := tools.GetSessionFromContext(ctx)
			if parentID == "" {
				return fantasy.ToolResponse{}, errors.New("session id missing from context")
			}
			latest, ok := c.cfg.Config().Agents[id]
			if !ok {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Agent %q is no longer configured.", id)), nil
			}
			return c.launchDelegation(ctx, tools.TaskCreateArgs{
				Goal:            params.Prompt,
				ParentSessionID: parentID,
				SessionTitle:    latest.Name,
				AgentID:         id,
				SessionID:       delegationSessionID(ctx, c.sessions, call.ID),
				Factory: func(ctx context.Context, childID string) (func(context.Context) (tools.TaskRunResult, error), func(), error) {
					definition, ok := c.cfg.Config().Agents[id]
					if !ok {
						return nil, nil, fmt.Errorf("agent %q is no longer configured", id)
					}
					systemPrompt, err := prompt.NewPrompt(id, definition.Prompt, prompt.WithWorkingDir(c.cfg.WorkingDir()))
					if err != nil {
						return nil, nil, fmt.Errorf("parse prompt: %w", err)
					}
					agent, err := c.buildAgent(ctx, systemPrompt, definition, true)
					if err != nil {
						return nil, nil, err
					}
					return c.subAgentTaskRun(parentID, childID, params.Prompt, agent), nil, nil
				},
			})
		},
	), nil
}

func sameAgentDefinition(a, b config.Agent) bool {
	return a.Model == b.Model &&
		a.ReasoningEffort == b.ReasoningEffort &&
		a.Prompt == b.Prompt &&
		slices.Equal(a.AllowedTools, b.AllowedTools) &&
		slices.Equal(a.ContextPaths, b.ContextPaths) &&
		maps.EqualFunc(a.AllowedMCP, b.AllowedMCP, slices.Equal)
}

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
