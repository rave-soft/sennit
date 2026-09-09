---
name: threads
description: "Use when a delegation needs an isolated git worktree. Create it with the `agent` tool using `isolation: worktree`; monitor it with the compatible agent_* management tools."
---

# Isolated delegations

Use an isolated delegation only when concurrent file changes need a separate
worktree and branch. Start it with `agent`, setting `isolation` to `worktree`
and giving it a complete prompt: scope, acceptance criteria, and constraints.

The delegation reports its terminal result automatically. Use `agent_list`,
`agent_result`, `agent_send`, `agent_cancel`, and `agent_output` to inspect or
steer delegations you started. Historical `thread_list`, `thread_status`, and
`thread_send` names remain compatible aliases for the applicable management
tools. Worktree merge and cleanup policy remains managed by the runtime and
user interfaces; do not attempt lifecycle tool calls that are not offered.
