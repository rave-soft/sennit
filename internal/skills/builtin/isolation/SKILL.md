---
name: isolation
description: "Use when a delegation needs an isolated git worktree. Create it with the `agent` tool using `isolation: worktree`; monitor it with the compatible agent_* management tools."
---

# Isolated delegations

Use an isolated delegation only when concurrent file changes need a separate
worktree and branch. Start it with `agent`, setting `isolation` to `worktree`
and giving it a complete prompt: scope, acceptance criteria, and constraints.

The delegation reports its terminal result automatically. Use `agent_list`,
`agent_result`, `agent_send` and `agent_cancel` to inspect or steer
delegations you started; `agent_output` is the one that does not apply, since
an isolated delegation's transcript lives in its own worktree session.
Historical `thread_list`, `thread_status`, and `thread_send` names remain
compatible aliases for the applicable management tools. On completion, the runtime removes only a clean worktree whose branch
has no commits outside its base. Changes, unique commits, or an uncertain Git
state preserve the worktree, branch, and record for explicit removal.
