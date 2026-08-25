---
name: tasks
description: Use when deciding whether a request should run as an asynchronous task, be handed off to a thread, or just happen directly in the current turn — including whenever a task_* tool (task_list, task_result, task_send, task_cancel, task_output) is available and delegated work might apply. Every agent call starts an asynchronous task — cheap, no isolation, suited to read-only/research work. Do NOT reach for one just because work could run concurrently — most requests should run directly in the current turn. See the threads skill instead when the work needs real isolation (its own worktree and branch).
---

# Background tasks

A background task is a delegation with **no isolation**: it runs in the same
working directory and the same app instance as the turn that created it, with
its own child session. That is what makes it cheap to start — no worktree, no
branch, no second database — and also what bounds what it is good for: it
competes for the same files and the same permission prompts as everything
else in the workspace, so it is not a way to get real parallelism on work
that edits files.

Start one with the `agent` tool — delegation is always asynchronous and there
is no separate task-creation tool.

## Choosing: steering, a task, or a thread

- **Steering.** A message sent while a turn is already running is folded into
  that turn at its next step, not started as a new one, and does not
  interrupt a tool call in flight. Most "also do X" or "actually, do Y
  instead" follow-ups are this — nothing needs to be dispatched.
- **A background task.** Cheap, parallel work that doesn't touch files —
  research, locating code, drafting something to report back on. The default
  for "go look into this while I keep working."
- **A thread.** Real isolation: its own git worktree, branch, app instance,
  and database, with a merge policy for folding the work back in. Use it only
  when the work would otherwise collide with something already happening —
  see the `threads` skill.

Rule of thumb: isolation → thread; cheap parallel read-only work → task;
refining work already in flight → steering (say it, don't dispatch anything).

## When to use one

- Suited to read-only/research work. Anything that edits files competes with
  the current turn (and any other active task) for the same working
  directory and the same permission queue — prefer doing it directly, or use
  a thread if it genuinely needs isolation.
- Do not poll for the result. A task's outcome — completed, failed, or
  cancelled, with its result or error — is delivered into your own context
  automatically once it finishes, arriving as a system-generated report at
  your next step. Correlate by task and child-session id; sibling completions
  may arrive in any order. `task_result` is for checking in on one that hasn't
  reported back yet, not the normal way to receive it.

## Limits

- At most 4 tasks may be active in a workspace at once, and at most 2 of
  those may belong to the turn doing the dispatching. Past either limit, a
  new delegation is refused with the current count and the limit in
  the message — wait for one to finish, or do the work directly instead of
  retrying immediately.
- A task's completion may wake a follow-up turn that starts another task, but
  that chain is capped at 3 levels deep. Past it, the `agent` tool refuses
  further background dispatch from that chain; finish the work directly
  instead of trying to delegate again.
- A workspace can have this feature turned off entirely
  (`options.background_agents`). When it is, new delegation is refused and the
  task_* tools are not offered at all — do the work directly instead.

## Monitoring and follow-up

- `task_list` — every task in the workspace and its current status.
- `task_result` — a specific task's outcome once it has one.
- `task_send` — queue a follow-up message into a task's own session. Refused
  once the task has been cancelled.
- `task_cancel` — stop a task's in-flight run.
- `task_output` — a tail of a task's own transcript (user/assistant text
  only), for checking on progress without waiting for it to finish.

A client attached remotely to a server-hosted workspace can list and cancel
tasks, but cannot send it a follow-up or read its transcript that way — those
only go through the task_* tools above, which run in the same process as the
task itself.
