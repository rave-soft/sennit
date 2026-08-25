package agent

import (
	"context"
	_ "embed"
	"errors"
	"fmt"

	"charm.land/fantasy"

	promptpkg "github.com/rave-soft/sennit/internal/agent/prompt"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/config"
)

//go:embed templates/agent_tool.md
var agentToolDescription string

// AgentParams is deliberately limited to the work to delegate. Delegations are
// always asynchronous; a tool call is only an acknowledgement of launch.
type AgentParams struct {
	Prompt string `json:"prompt" description:"The task for the agent to perform"`
}

const AgentToolName = "agent"

func intPointer(value int) *int { return &value }

// AgentBackgroundResponseMetadata identifies a durable task whose terminal
// outcome will be delivered to the parent completion inbox.
type AgentBackgroundResponseMetadata struct {
	TaskID    string `json:"task_id"`
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
}

func (c *coordinator) agentTool(_ context.Context, cfg agentConfig) (fantasy.AgentTool, error) {
	if _, ok := cfg.Agents()[config.AgentTask]; !ok {
		return nil, errors.New("task agent not configured")
	}
	return tools.WithToolSchemaConstraints(fantasy.NewParallelAgentTool(
		AgentToolName,
		agentToolDescription,
		func(ctx context.Context, params AgentParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Prompt == "" {
				return fantasy.NewTextErrorResponse("prompt is required"), nil
			}
			sessionID := tools.GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, errors.New("session id missing from context")
			}
			return c.runBackgroundAgent(ctx, sessionID, params.Prompt)
		},
	), map[string]tools.ToolSchemaConstraint{"prompt": {MinLength: intPointer(1)}}), nil
}

// runBackgroundAgent creates a durable task and returns before its child turn
// is prepared or run. Completion is delivered independently by the task
// lifecycle, never by keeping the caller's tool invocation open.
func (c *coordinator) launchDelegation(ctx context.Context, args tools.TaskCreateArgs) (fantasy.ToolResponse, error) {
	if !c.backgroundAgentsEnabled() {
		return fantasy.NewTextErrorResponse("Delegation is disabled in this workspace (options.background_agents)."), nil
	}
	depth := tools.GetDepthFromContext(ctx)
	if depth >= maxTaskCascadeDepth {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Delegation depth limit (%d) reached; finish this work directly.", maxTaskCascadeDepth)), nil
	}
	manager := c.tasksManager()
	if manager == nil {
		return fantasy.NewTextErrorResponse("Delegation is unavailable in this workspace."), nil
	}
	args.Depth = depth
	info, err := manager.Create(ctx, args)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to start delegation: %s", err)), nil
	}
	return fantasy.WithResponseMetadata(
		fantasy.NewTextResponse(fmt.Sprintf("Started delegation %s (session %s, status=%s). Its result will follow separately.", info.ID, info.SessionID, info.Status)),
		AgentBackgroundResponseMetadata{TaskID: info.ID, SessionID: info.SessionID, Status: info.Status},
	), nil
}

func (c *coordinator) runBackgroundAgent(ctx context.Context, sessionID, delegatedPrompt string) (fantasy.ToolResponse, error) {
	return c.launchDelegation(ctx, tools.TaskCreateArgs{
		Goal:            delegatedPrompt,
		ParentSessionID: sessionID,
		SessionTitle:    "New Agent Session",
		Factory: func(ctx context.Context, childSessionID string) (func(context.Context) (tools.TaskRunResult, error), func(), error) {
			agentCfg, ok := c.cfg.Config().Agents[config.AgentTask]
			if !ok {
				return nil, nil, errors.New("task agent not configured")
			}
			p, err := taskPrompt(promptpkg.WithWorkingDir(c.cfg.WorkingDir()))
			if err != nil {
				return nil, nil, err
			}
			agent, err := c.buildAgent(ctx, p, agentCfg, true)
			if err != nil {
				return nil, nil, err
			}
			return c.subAgentTaskRun(sessionID, childSessionID, delegatedPrompt, agent), nil, nil
		},
	})
}
