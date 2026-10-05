---
name: tasks
description: Use when deciding whether work should delegate to an agent, run concurrently, be isolated in a worktree, or happen directly in the current turn. Covers agent, agent_wait, agent_list, agent_result, agent_send, agent_cancel, and agent_output.
---

# Delegations

A delegation runs in its own child session. By default, `agent` waits for its
terminal event and receives the result in the same tool response, so continue
from that result without shell sleep, `job_output`, or polling.

Set `background: true` only when there is useful independent work to do while
the delegation runs. Its terminal report is delivered automatically through the
completion inbox. Use `agent_wait` for selected background delegations when the
next action depends on all of them finishing.

`agent_wait` is event-driven. It waits for completed, failed, interrupted, or
cancelled terminal states. A user message interrupts a default delegation wait
or `agent_wait`; respond to the user while delegations continue and report
automatically. The durable inbox can deliver an outcome already returned inline.
Correlate by delegation and child-session id and do not repeat actions for it.

Use `isolation: worktree` when delegated file changes need an independent
worktree and branch. Ordinary delegations share the current workspace, so do
not run conflicting edits concurrently.

## Delegation management

- `agent_list` lists delegations you started and their statuses.
- `agent_result` reads a specific completed delegation's result.
- `agent_wait` waits for selected background delegations without polling.
- `agent_send` queues a follow-up instruction for a delegation.
- `agent_cancel` stops a delegation and its descendant tasks.
- `agent_output` reads an unisolated delegation's transcript.

A completion can arrive from a cancelled ancestor's child. It is labeled so the
receiving agent can tell why it owns that report.
