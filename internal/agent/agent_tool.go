package agent

import (
	"context"
	_ "embed"
	"errors"
	"fmt"

	"charm.land/fantasy"

	"github.com/rave-soft/sennit/internal/agent/prompt"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/config"
)

//go:embed templates/agent_tool.md
var agentToolDescription string

type AgentParams struct {
	Prompt string `json:"prompt" description:"The task for the agent to perform"`
	// Background, when true, dispatches the prompt as a task delegation
	// instead of running it inline: the call returns immediately with
	// the task's id, child session id, and status, rather than blocking
	// for the subagent's text. See the tool description for when this is
	// appropriate.
	Background bool `json:"background,omitempty" description:"Run as a background task delegation instead of blocking for a result. Suited to read-only/research work; leave unset for anything that edits files."`
}

const (
	AgentToolName = "agent"
)

// AgentBackgroundResponseMetadata is the structured metadata a
// background agent tool call returns immediately: enough for the caller
// to reference the task later (poll it or be notified), not the
// delegation's full record.
type AgentBackgroundResponseMetadata struct {
	TaskID    string `json:"task_id"`
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
}

// AgentDetachedResponseMetadata is the structured metadata a foreground
// delegation returns when it detaches into the background mid-run (see
// coordinator.runDetachableSubAgent) instead of blocking for its result.
// Deliberately its own type rather than a reuse of
// AgentBackgroundResponseMetadata: a detached delegation was never
// created through the task manager, so it has no TaskID and no Status
// beyond "still running" - carrying those fields empty would read as if
// they meant something.
type AgentDetachedResponseMetadata struct {
	SessionID string `json:"session_id"`
}

func (c *coordinator) agentTool(ctx context.Context, cfg agentConfig) (fantasy.AgentTool, error) {
	agentCfg, ok := cfg.Agents()[config.AgentTask]
	if !ok {
		return nil, errors.New("task agent not configured")
	}
	prompt, err := taskPrompt(prompt.WithWorkingDir(c.cfg.WorkingDir()))
	if err != nil {
		return nil, err
	}

	agent, err := c.buildAgent(ctx, prompt, agentCfg, true)
	if err != nil {
		return nil, err
	}
	return fantasy.NewParallelAgentTool(
		AgentToolName,
		agentToolDescription,
		func(ctx context.Context, params AgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Prompt == "" {
				return fantasy.NewTextErrorResponse("prompt is required"), nil
			}

			sessionID := tools.GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, errors.New("session id missing from context")
			}

			if params.Background {
				return c.runBackgroundAgent(ctx, sessionID, params.Prompt)
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
				SessionTitle:   "New Agent Session",
				Detachable:     true,
			})
		},
	), nil
}

// runBackgroundAgent creates a task delegation for prompt, parented to
// sessionID, and returns its metadata immediately without waiting for the
// task's own run to finish — that result reaches the parent through a
// separate mechanism (polling or notification), not this call.
//
// If no task manager is wired (tasksManager returns nil — e.g. not a git
// repository, or Attach declined for some other reason), this reports the
// failure back to the model as a tool error rather than silently running
// prompt in the foreground instead: a caller that asked for background
// and got foreground without being told would believe work is proceeding
// in parallel when it is not.
func (c *coordinator) runBackgroundAgent(ctx context.Context, sessionID, prompt string) (fantasy.ToolResponse, error) {
	// options.background_agents is the workspace-level opt-out. Checked
	// first, ahead of the cascade-depth and manager checks below, because
	// it is the most fundamental of the three: config said no, full stop.
	if !c.backgroundAgentsEnabled() {
		return fantasy.NewTextErrorResponse(
			"Background delegation is disabled in this workspace (options.background_agents). Retry the same request with background unset to run it in the foreground instead.",
		), nil
	}

	// Refuse before touching the task manager at all: a turn already at
	// the cascade limit still runs (it has real work to do — the
	// completion that woke it), but must not be able to start yet
	// another task whose own eventual completion would wake a turn one
	// level deeper still. See maxTaskCascadeDepth.
	depth := tools.GetDepthFromContext(ctx)
	if depth >= maxTaskCascadeDepth {
		return fantasy.NewTextErrorResponse(fmt.Sprintf(
			"Background delegation depth limit (%d) reached: this turn is itself a background continuation too many levels deep to start another one. Finish this work directly instead of delegating it further.",
			maxTaskCascadeDepth,
		)), nil
	}

	taskManager := c.tasksManager()
	if taskManager == nil {
		return fantasy.NewTextErrorResponse(
			"Background delegation is unavailable in this workspace. Retry the same request with background unset to run it in the foreground instead.",
		), nil
	}

	info, err := taskManager.Create(ctx, tools.TaskCreateArgs{
		Goal:            prompt,
		ParentSessionID: sessionID,
		Depth:           depth,
	})
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to start background task: %s", err)), nil
	}

	text := fmt.Sprintf(
		"Started background task %s (session %s, status=%s). It is running independently; its result will follow separately.",
		info.ID, info.SessionID, info.Status,
	)
	return fantasy.WithResponseMetadata(fantasy.NewTextResponse(text), AgentBackgroundResponseMetadata{
		TaskID:    info.ID,
		SessionID: info.SessionID,
		Status:    info.Status,
	}), nil
}
