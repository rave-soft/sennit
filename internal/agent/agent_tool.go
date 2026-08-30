package agent

import (
	"context"
	_ "embed"

	"github.com/rave-soft/sennit/internal/agent/tools"
)

//go:embed templates/agent_tool.md
var agentToolPurpose string

// delegationReportContract is how a delegation answers: the caller is
// told, in the description of every tool that starts one, that the
// report comes back by itself and what that means for how the turn ends.
//
// It lives in one file because three tool descriptions need it word for
// word - the built-in agent tool here, agentic_fetch, and every named
// agent in .sennit/agents, whose description is the person's own
// sentence about what that agent is for and says nothing about how
// delegation works. A caller that reads only one of them must not come
// away with a different contract than a caller that reads another. The
// coder prompt states the same rule (see templates/coder.md.tpl,
// "Delegation completions") for the turns that never read a tool
// description at all.
//
//go:embed templates/delegation_report.md
var delegationReportContract string

// agentToolDescription is what this tool is for, then how its answer
// comes back.
var agentToolDescription = agentToolPurpose + "\n" + delegationReportContract

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

// delegationSessionID is the id a delegation launched by toolCallID gives
// its child session: the same "<messageID>$$<toolCallID>" identity the
// transcript derives for that tool call, so opening the delegation in the
// UI finds the session it points at. Returns "" when the turn has no
// message id in context, leaving the task manager to generate one — an
// unopenable delegation is worse than a blocked one, but a delegation that
// refuses to start is worse than both.
func delegationSessionID(ctx context.Context, sessions interface {
	CreateAgentToolSessionID(messageID, toolCallID string) string
}, toolCallID string,
) string {
	messageID := tools.GetMessageFromContext(ctx)
	if messageID == "" || toolCallID == "" {
		return ""
	}
	return sessions.CreateAgentToolSessionID(messageID, toolCallID)
}

// delegationDepth is the depth a delegation started by ctx's turn runs
// at: one level below that turn. The single place the +1 is spelled out,
// so the task record, the delegate's own turns and the refusal check can
// never drift apart on what "one level down" means.
func delegationDepth(ctx context.Context) int {
	return tools.GetDepthFromContext(ctx) + 1
}
