package agent

import (
	"charm.land/fantasy"
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
	// Depth is how many delegation levels below a person's own turn this
	// delegation runs — one below the turn that started it. Carried onto
	// the delegate's own call so a delegation that delegates again is
	// counted as the nesting it is; see maxTaskCascadeDepth.
	Depth int
	// SessionSetup is an optional callback invoked after session creation
	// but before agent execution, for custom session configuration.
	SessionSetup func(sessionID string)
}

type subAgentOutcome struct {
	result *fantasy.AgentResult
	err    error
}

func subAgentOutput(result *fantasy.AgentResult) string {
	if result == nil {
		return ""
	}
	return result.Response.Content.Text()
}
