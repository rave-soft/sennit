# Steering, tasks, and threads

> [!NOTE]
> This document was designed for both humans and agents.

Sennit has three ways work can happen alongside — or as a continuation of —
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

If that turn has already ended, the report starts a new one — but only in
the session this sennit is working in. A sennit works in exactly one:
restored from history, named on the command line, or created new. That
session and the delegations under it are the one line of work that may move
on its own, which is what lets a delegation hand off to the next without you
typing between them.

Every other conversation in the database stays where it was left. One from
last week does not pick itself back up because something it started finally
finished, and a restart that recovers a pile of interrupted delegations wakes
nothing. Switch to one of them and it becomes the session being worked in;
what you switched away from goes quiet again.

Nothing is lost either way. A report that cannot wake its session — it is not
the one being worked in, it is busy, you canceled it, or its continuation
keeps failing — waits in the inbox and is folded into the top of that
session's next turn, ahead of what you typed, the first moment you are back
in it.

The session you are in does keep working while you watch it, and that is the
point; press Esc twice to stop it, or turn delegation off entirely with
`options.background_agents: false`.

A workspace allows at most 4 tasks running at once, and at most 2 of those
started by any one turn — past either limit, starting another is refused
rather than queued. A chain of delegations starting further delegations is
capped at 3 levels deep.

Turn the feature off entirely with `options.background_agents: false` in
`sennit.json`: the model can no longer start a background task and the
task-management tools stop being offered. A task already running when the
option is turned off keeps running to completion rather than being killed —
the switch only blocks new dispatch. Threads are a separate, older feature
and are unaffected by it.

## Threads

A thread is the opposite trade-off: real isolation — its own git worktree
and branch, its own app instance and database, a merge policy for folding the
work back in — at real cost (a full agent session, plus a git merge on the
way back). Use one only when the work would otherwise collide with something
else already happening: the same files, the same branch.

A thread runs in an app instance of its own, and that instance works in one
session the same way yours does — the one your session asked for. That is
what a thread may wake itself for; it grants nothing to the workspace you are
in, which is a different instance entirely.

A thread can be cancelled without being torn down — its worktree and branch
stay on disk, so you can still inspect or resume the work — or removed
outright once you're done with it.

## Named agents remember

A named agent — anything defined as a markdown file in `.sennit/agents`,
as opposed to the built-in `coder` and `task` — is a continuing counterpart,
not a stranger on every call. Delegating to `developer` twice under the same
session replays the first exchange into the second: you can send it review
findings and it knows what it wrote.

Continuity is scoped by *who* and *where*. Two named agents under one parent
keep separate conversations, and the same agent keeps separate conversations
under different parents — which is what makes a thread's delegations stay
inside that thread, since a thread runs on its own session.

A named agent's system prompt is its markdown file, plus one thing Sennit
appends: how it reports back. Nobody reads a delegation's transcript, and a
delegated agent has neither `ask_parent` nor `question`, so its final message
is the whole handoff — what came of the work, the absolute paths it touched,
the checks it actually ran, and what it left undone. You do not have to write
that into every agent file; it is there whatever the file says.

Each delegation still gets its own session, so each call remains its own
block in the transcript and drills into just that call. The memory lives in
what the agent is shown, not in where the messages are stored, and it is
bounded: once the carried transcript grows past the budget
(`maxCarriedSubAgentChars`), the oldest whole delegations are shed, newest
kept.

The anonymous delegations — the built-in `agent` and `agentic_fetch` tools —
stay stateless on purpose. They are one-off, often run several at a time on
unrelated work, and stitching those calls into one growing conversation
would cost context without buying continuity anyone asked for.

## Choosing

- Work needs isolation, or would otherwise collide with something else →
  **thread**.
- Cheap, parallel, read-only work → **task**.
- Refining or redirecting what's already running → **steering** — just say
  it; nothing needs to be dispatched.
