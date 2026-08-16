package config

import (
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
)

type Agent struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	// This is the id of the system prompt used by the agent
	Disabled bool `json:"disabled,omitempty"`

	// Model optionally pins this agent to a specific "provider/model-id",
	// letting a sub-agent use a model other agents don't. Left empty, the
	// agent inherits the application's main model. A value that does not
	// resolve to a known provider/model falls back to the empty default
	// with a warning logged.
	Model string `json:"model,omitempty" jsonschema:"description=Specific model as 'provider/model-id'; omit to inherit the app's main model"`

	// Prompt is the system prompt for this agent. User-defined agents must
	// set it; the built-in coder and task agents leave it empty and fall back
	// to their embedded templates.
	Prompt string `json:"prompt,omitempty" jsonschema:"description=System prompt for this agent. Required for user-defined agents"`

	// ReasoningEffort overrides the selected model's effort for this agent, so
	// a cheap reviewer and a deep implementer can share one model. An effort
	// the model does not offer is ignored at call time in favour of the
	// model's own default; the enum below lists the common levels, but any
	// level the model advertises is accepted.
	ReasoningEffort string `json:"reasoning_effort,omitempty" jsonschema:"description=Reasoning effort for this agent\\, overriding the model's. Ignored if the model does not support it,enum=low,enum=medium,enum=high"`

	// The available tools for the agent
	//  if this is nil, all tools are available
	AllowedTools []string `json:"allowed_tools,omitempty"`

	// this tells us which MCPs are available for this agent
	//  if this is empty all mcps are available
	//  the string array is the list of tools from the AllowedMCP the agent has available
	//  if the string array is nil, all tools from the AllowedMCP are available
	AllowedMCP map[string][]string `json:"allowed_mcp,omitempty"`

	// Overrides the context paths for this agent
	ContextPaths []string `json:"context_paths,omitempty"`
}

// SetupAgents discovers user-defined agents from .sennit/agents/*.md, merges
// them with the two built-ins, and writes the combined set back to c.Agents.
//
// Every user-defined agent becomes a tool the coder can call to delegate work,
// named after the agent's id. Those tool names are appended to the coder's
// allowed tools so the tool filter in the agent package keeps them; the ids are
// validated first so a role can never shadow a real tool.
//
// SetupAgents is idempotent: it fully recomputes c.Agents from markdown on
// every call — it never trusts whatever was already sitting in c.Agents (a
// previous run's built-ins, or nothing at all), so repeated calls (config
// reload, workspace switch) converge on the same result instead of drifting.
func (c *Config) SetupAgents() {
	c.setupAgents(nil)
}

// SetupAgentsWithInherited configures built-in and discovered agents, using
// inherited as the lowest-priority source of user-defined agents. Project-local
// definitions in the current working directory override inherited definitions.
func (c *Config) SetupAgentsWithInherited(inherited map[string]Agent) {
	c.setupAgents(inherited)
}

func (c *Config) setupAgents(inherited map[string]Agent) {
	// SetupAgents fully recomputes c.Agents and can run more than once on
	// the same live Config (e.g. after a markdown agent file changes), so
	// drop any agent problems from a previous run before validUserAgents
	// re-adds the current ones. Otherwise a problem sticks around forever
	// even after the offending agent definition is fixed.
	c.Problems = slices.DeleteFunc(c.Problems, func(p Problem) bool { return p.Area == AreaAgent })

	if c.jsonAgentsBlockDetected {
		c.addProblem(Problem{
			Severity: SeverityWarn,
			Area:     AreaAgent,
			Subject:  "agents",
			Message:  "agents in sennit.json are ignored — define agents as .sennit/agents/*.md files",
			Hint:     "move each entry to .sennit/agents/<name>.md (frontmatter: name, description, model, tools; body is the prompt)",
		})
	}

	allowedTools := resolveAllowedTools(allToolNames(), c.Options.DisabledTools)
	providers := c.providersOrEmpty()

	// Markdown files under .sennit/agents are the only source of user-defined
	// agents. A JSON "agents" block is never read (see loadFromBytes in
	// load.go); if one was present, jsonAgentsBlockDetected records it and
	// the Problem above surfaces it instead of using it.
	c.Agents = maps.Clone(inherited)
	if c.Agents == nil {
		c.Agents = make(map[string]Agent)
	}
	maps.Copy(c.Agents, discoverMarkdownAgents(c.workingDir, providers))

	userAgents, invalid := c.validUserAgents()
	for id, reason := range invalid {
		slog.Warn("Ignoring invalid agent definition", "agent", id, "reason", reason)
	}

	// The coder delegates through one tool per user-defined agent, so those
	// names have to survive buildTools' allow-list filter. They are appended
	// after the built-in tools — sorted among themselves for a stable order —
	// so the built-in ordering callers already rely on stays untouched.
	roleTools := slices.Sorted(maps.Keys(userAgents))
	coderTools := append(slices.Clone(allowedTools), roleTools...)

	agents := map[string]Agent{
		AgentCoder: {
			ID:           AgentCoder,
			Name:         "Coder",
			Description:  "An agent that helps with executing coding tasks.",
			ContextPaths: c.Options.ContextPaths,
			AllowedTools: coderTools,
		},

		AgentTask: {
			ID:           AgentTask,
			Name:         "Task",
			Description:  "An agent that helps with searching for context and finding implementation details.",
			ContextPaths: c.Options.ContextPaths,
			AllowedTools: resolveReadOnlyTools(allowedTools),
			// NO MCPs or LSPs by default
			AllowedMCP: map[string][]string{},
		},
	}

	for id, agent := range userAgents {
		agent.ID = id
		if agent.Name == "" {
			agent.Name = id
		}
		// A nil allowed_tools means "everything the coder may use". An empty
		// list is a deliberate choice and stays empty.
		if agent.AllowedTools == nil {
			agent.AllowedTools = slices.Clone(allowedTools)
		}
		if agent.ContextPaths == nil {
			agent.ContextPaths = c.Options.ContextPaths
		}
		agents[id] = agent
	}

	c.Agents = agents
}

// UserAgents returns a copy of the configured user-defined agents.
func (c *Config) UserAgents() map[string]Agent {
	agents := make(map[string]Agent)
	for id, agent := range c.Agents {
		if id == AgentCoder || id == AgentTask {
			continue
		}
		agents[id] = cloneAgent(agent)
	}
	return agents
}

func cloneAgents(agents map[string]Agent) map[string]Agent {
	cloned := make(map[string]Agent, len(agents))
	for id, agent := range agents {
		cloned[id] = cloneAgent(agent)
	}
	return cloned
}

func cloneAgent(agent Agent) Agent {
	agent.AllowedTools = slices.Clone(agent.AllowedTools)
	agent.AllowedMCP = maps.Clone(agent.AllowedMCP)
	agent.ContextPaths = slices.Clone(agent.ContextPaths)
	return agent
}

// validUserAgents splits the decoded agent map into the definitions that can
// be used and the ones that cannot, with a reason for each rejection. Rejecting
// beats failing the whole load: one bad agent should not stop the app from
// starting.
func (c *Config) validUserAgents() (valid map[string]Agent, invalid map[string]string) {
	valid = make(map[string]Agent)
	invalid = make(map[string]string)
	builtinTools := allToolNames()
	providers := c.providersOrEmpty()

	for id, agent := range c.Agents {
		switch {
		case id == AgentCoder || id == AgentTask:
			// Built-ins are rebuilt from scratch on every call; a leftover
			// entry from a previous SetupAgents is not a user definition.
			continue
		case agent.Disabled:
			continue
		case !validAgentID(id):
			invalid[id] = "id must be non-empty and contain only letters, digits, '_' or '-'"
		case slices.Contains(builtinTools, id):
			invalid[id] = "id collides with a built-in tool of the same name"
		case strings.TrimSpace(agent.Prompt) == "":
			invalid[id] = "prompt is required for user-defined agents"
		default:
			// A model string that does not resolve to a known provider/model
			// is not worth rejecting the whole agent over; fall back to the
			// empty default and say so, symmetric to the markdown agent
			// loader. This is also where stray "large"/"small" values land
			// now that those words carry no special meaning here.
			if agent.Model != "" {
				if _, err := ResolveModelString(providers, agent.Model); err != nil {
					slog.Warn("Unrecognised model for agent, falling back to the app's main model",
						"agent", id, "model", agent.Model, "error", err)
					c.addProblem(Problem{
						Severity: SeverityWarn,
						Area:     AreaAgent,
						Subject:  id,
						Message:  fmt.Sprintf("agent %s: model %s not found — falls back to the main model", id, agent.Model),
						Hint:     "run 'sennit models' to see available provider/model pairs",
					})
					agent.Model = ""
				}
			}
			valid[id] = agent
		}
	}
	return valid, invalid
}

// validAgentID reports whether id is usable as a tool name. Providers reject
// tool names outside this character set, so an invalid id would otherwise
// surface as an opaque API error on the first request.
func validAgentID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}
