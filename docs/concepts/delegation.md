# Steering and delegation

> [!NOTE]
> This document was designed for both humans and agents.

Sennit has two ways work can happen alongside — or as a continuation of —
what you're already doing: **steering** a turn that's running, and
**delegating** it to a subagent. A delegation runs in the working directory
you are in, unless you ask for isolation, in which case it gets a git
worktree of its own. That flag is the whole difference, and it is where the
cost is. This is the one place both are written down together.

## Steering

Send a message while the agent is already working on something, and it does
not start a new turn or interrupt whatever tool call is in flight. It's
folded into the current turn and picked up at its next step, alongside
whatever the model was already doing.

This is the behavior people misread most: sending a message does not stop or
redirect what's running immediately, only once it reaches a step boundary. If
you actually want to stop the current turn, press Esc twice (once to arm it,
again to confirm) rather than typing something else.

## Delegations

A delegation is `agent`, with a `subagent_type` naming one of the agents in
`.sennit/agents` (or none, for the general-purpose one). By default it has
**no isolation**: it runs in the same working directory and the same app
instance as the turn that created it, with its own child session. That's
what makes it cheap to start, and also what it is a poor fit for — it
competes for the same files and the same permission prompts as everything
else in the workspace, so it doesn't suit work that edits files. It's best
for read-only or research work: something to go look into while the current
turn keeps going.

A delegation isn't polled for its result. Once it finishes, its outcome is
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

A workspace allows at most 4 unisolated delegations running at once, and at
most 2 of those started by any one turn — past either limit, starting
another is refused rather than queued. Isolated ones count toward neither:
they spawn an app of their own and never touch the resources those limits
protect. A chain of delegations starting further delegations is capped at 3
levels deep either way.

## Every delegation tool answers from where you stand

The delegations form a tree, and `agent_list`, `agent_result`,
`agent_output`, `agent_send` and `agent_cancel` all read it from one place:
the session the call came from. Each reaches that session's own subtree —
what it started, and what those started, at any depth — and nothing else. A
delegation cannot cancel itself, cannot cancel the delegation it hangs
under, and cannot reach across to a sibling's work; the session you type
into still sees everything under it, and none of the conversation you had
last week.

The rule reads like tidiness and is not. Before it existed, a delegated
agent meaning to stop one of the two delegations it had started passed its
own id to the cancel tool — its own row was in the listing and nothing checked
whose it was. It killed its own turn mid-sentence. The report it owed the
session waiting on it died with it, its own child went on editing the
repository for nine more minutes with nobody left above it, and the work
stopped where it stood until morning.

Cancelling a delegation cancels everything it started, for the second half
of that same story: a delegation exists to answer the one that dispatched
it, and once that one is stopped, its children are running for nobody —
still holding concurrency slots, still raising permission prompts, still
writing to the workspace.

And a report addressed to a delegation that was cancelled goes to
whoever started it instead. A cancelled delegation's session is the one
inbox in the system with nobody behind it: no person will type into it
and no turn will ever begin there again, so anything left in it is
thrown away. What arrives one level up is labeled with the delegation
that never read it — "your delegation was cancelled, here is what its
own child managed to finish" is a different thing from an ordinary
report, and the reader has to be able to tell.

Turn the feature off entirely with `options.background_agents: false` in
`sennit.json`: the model can no longer start a delegation and the
management tools stop being offered. One already running when the option is
turned off keeps running to completion rather than being killed — the switch
only blocks new dispatch.

## Isolation

`agent` with `isolation: "worktree"` is the opposite trade-off: real
isolation — its own git worktree and branch, its own app instance and
database — at real cost (a full agent session). Ask for it only when the
work would otherwise collide with something else already happening: the same
files, the same branch.

An isolated delegation runs in an app instance of its own, and that instance
works in one session the same way yours does — the one your session asked
for. That is what it may wake itself for; it grants nothing to the workspace
you are in, which is a different instance entirely. Its transcript is in that
session, which is why `agent_output` is the one management tool it does not
answer.

Nothing is merged back for you. When it reaches a terminal status Sennit
removes the worktree and branch only if the worktree is clean and the branch
has no commits outside its base; changes, unique commits, or a Git state
Sennit cannot read leave both on disk for you to deal with. Cancelling is the
same deal: the delegation stops, its worktree and branch stay, and you can
inspect or resume the work.

## Taking the session into a worktree yourself

Isolation is for work you hand off. The `worktree` command in the palette is
for work you keep: it moves **the session you are in** into a worktree of its
own, and `exit worktree` brings it back. The conversation does not change —
same session, same transcript, same id — only its working root does, and
entering the same worktree again picks up where it was left. The breadcrumb
bar names the worktree so it stays obvious which tree you are typing into.

It is refused while a turn or an unfinished tool call is in flight, rather
than interrupting one. Delegations already running are not moved or
restarted; they keep the root they started in, and a completion that lands
mid-move is delivered once, to the session wherever it now lives.

## Named agents remember

A named agent — anything defined as a markdown file in `.sennit/agents`,
as opposed to the built-in `coder` and `task` — is a continuing counterpart,
not a stranger on every call. Delegating to `developer` twice under the same
session (`agent` with `subagent_type: developer`) replays the first exchange
into the second: you can send it review findings and it knows what it wrote.

Continuity is scoped by *who* and *where*. Two named agents under one parent
keep separate conversations, and the same agent keeps separate conversations
under different parents — which is what keeps an isolated delegation's own
delegations inside it, since it runs on its own session.

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

The anonymous delegations — `agent` with no `subagent_type`, and
`agentic_fetch` — stay stateless on purpose. They are one-off, often run several at a time on
unrelated work, and stitching those calls into one growing conversation
would cost context without buying continuity anyone asked for.

## Choosing

- Refining or redirecting what's already running → **steering** — just say
  it; nothing needs to be dispatched.
- Cheap, parallel, read-only work → **delegate** it as it comes.
- Work that would collide with what's already running — the same files, the
  same branch → delegate it with **`isolation: "worktree"`**.
- Work you are going to do yourself, off the main tree → the **`worktree`**
  command, which takes this session there.
