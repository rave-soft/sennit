package agent

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/session"
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

// namedAgent is one delegatable agent as the agent tool advertises it:
// the id a caller passes as subagent_type, and the person's own sentence
// about what it is for.
type namedAgent struct {
	ID          string
	Description string
}

// agentToolDescription is what this tool is for, which agents it can run,
// and how its answer comes back.
//
// The roster is part of the description rather than a separate listing
// because the description is the only thing a model reads before choosing
// a subagent_type, and it is rebuilt whenever the config version changes
// (see delegationFinalizer.runtimeInputs), so a newly added .sennit/agents
// file shows up without a restart. An empty roster - no user-defined
// agents configured, or a delegated caller, which may not start named
// agents - leaves the section out entirely rather than advertising a
// parameter with nothing to put in it.
func agentToolDescription(named []namedAgent) string {
	var b strings.Builder
	b.WriteString(agentToolPurpose)
	if len(named) > 0 {
		b.WriteString("\nAvailable `subagent_type` values:\n\n")
		for _, a := range named {
			fmt.Fprintf(&b, "- `%s`: %s\n", a.ID, a.Description)
		}
		b.WriteString("\nOmit `subagent_type` to run the built-in general-purpose agent.\n")
	}
	b.WriteByte('\n')
	b.WriteString(delegationReportContract)
	return b.String()
}

// delegatedAgentContract is the other half of that handoff, told to the
// delegation rather than to its caller: your last message is the report,
// and it is all anyone gets.
//
// A named agent's system prompt is otherwise entirely the person's own
// file in .sennit/agents - nothing in it comes from Sennit - so without
// this the agent has no way to know its transcript is never read, that
// the caller cannot come back with a question (a sub-agent build has
// neither ask_parent nor question - see gateAllows), or that a bare
// "done" strands the work it just finished.
//
//go:embed templates/delegated_agent.md
var delegatedAgentContract string

// delegatedAgentPrompt is a named agent's system prompt: what the person
// wrote it to be, then how it answers. The person's definition comes
// first so the contract reads as the closing word on the handoff, and it
// is appended as plain text - the definition is a template, this is not.
func delegatedAgentPrompt(definition string) string {
	return definition + "\n\n" + delegatedAgentContract
}

// AgentParams is the work to delegate and which agent to hand it to.
// Delegations are always asynchronous; a tool call is only an
// acknowledgement of launch.
//
// SubagentType is what unified the delegation surface: user-defined
// agents used to be registered as one tool each, named after the agent,
// so the tool list grew with every file in .sennit/agents and a caller
// had to be told separately which of those names were agents at all.
// They are now this one field, and the tool's own description carries
// the roster.
type AgentParams struct {
	Prompt string `json:"prompt" description:"The task for the agent to perform"`
	// SubagentType names a user-defined agent (.sennit/agents, config
	// Agents). Empty runs the built-in general-purpose agent, which is
	// what every caller got before this field existed.
	SubagentType string `json:"subagent_type,omitempty" description:"Which agent to run; omit for the general-purpose agent"`
	// Description is a short label for the delegation, used as its child
	// session's title. Optional: an empty one falls back to the agent's
	// own name.
	Description string `json:"description,omitempty" description:"Short (3-5 word) label for this delegation"`
}

const AgentToolName = "agent"

// subAgentAgentToolKey names the delegated-caller build of the agent tool
// in delegationToolsBuilt. It is a map key only - the tool it holds
// registers under AgentToolName like any other - so it deliberately looks
// nothing like a tool name a model could call.
const subAgentAgentToolKey = "agent#sub"

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
func delegationSessionID(ctx context.Context, toolCallID string) string {
	messageID := tools.GetMessageFromContext(ctx)
	if messageID == "" || toolCallID == "" {
		return ""
	}
	return session.CreateAgentToolSessionID(messageID, toolCallID)
}

// delegationDepth is the depth a delegation started by ctx's turn runs
// at: one level below that turn. The single place the +1 is spelled out,
// so the task record, the delegate's own turns and the refusal check can
// never drift apart on what "one level down" means.
func delegationDepth(ctx context.Context) int {
	return tools.GetDepthFromContext(ctx) + 1
}
