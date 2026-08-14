# Steering, tasks, and threads

> [!NOTE]
> This document was designed for both humans and agents.

Braid has three ways work can happen alongside — or as a continuation of —
what you're already doing: **steering** a turn that's running, a
**background task**, and a **thread**. They solve different problems and
carry very different overhead. This is the one place all three are written
down together.

## Steering

Send a message while the agent is already working on something, and it does
not start a new turn or interrupt whatever tool call is in flight. It's
folded into the current turn and picked up at its next step, alongside
whatever the model was already doing.

This is the behavior people misread most: sending a message does not stop or
redirect what's running immediately, only once it reaches a step boundary. If
you actually want to stop the current turn, press Esc twice (once to arm it,
again to confirm) rather than typing something else.

## Background tasks

A background task is a delegation with **no isolation**: it runs in the same
working directory and the same app instance as the turn that created it, with
its own child session. That's what makes it cheap to start, and also what it
is a poor fit for — it competes for the same files and the same permission
prompts as everything else in the workspace, so it doesn't suit work that
edits files. It's best for read-only or research work: something to go look
into while the current turn keeps going.

A task isn't polled for its result. Once it finishes, its outcome is
delivered back automatically and shows up as a report at the next step of
whatever turn created it.

A workspace allows at most 4 tasks running at once, and at most 2 of those
started by any one turn — past either limit, starting another is refused
rather than queued. A task waking a follow-up turn that starts another task
is capped at 3 levels deep.

Turn the feature off entirely with `options.background_agents: false` in
`braid.json`: the model can no longer start a background task and the
task-management tools stop being offered. A task already running when the
option is turned off keeps running to completion rather than being killed —
the switch only blocks new dispatch. Threads are a separate, older feature
and are unaffected by it.

In client/server mode, a remote client can list and cancel a workspace's
tasks, but cannot send one a follow-up or read its transcript that way —
those are only reachable from inside the same process the task itself runs
in.

## Threads

A thread is the opposite trade-off: real isolation — its own git worktree
and branch, its own app instance and database, a merge policy for folding the
work back in — at real cost (a full agent session, plus a git merge on the
way back). Use one only when the work would otherwise collide with something
else already happening: the same files, the same branch.

A thread can be cancelled without being torn down — its worktree and branch
stay on disk, so you can still inspect or resume the work — or removed
outright once you're done with it.

## Choosing

- Work needs isolation, or would otherwise collide with something else →
  **thread**.
- Cheap, parallel, read-only work → **task**.
- Refining or redirecting what's already running → **steering** — just say
  it; nothing needs to be dispatched.
