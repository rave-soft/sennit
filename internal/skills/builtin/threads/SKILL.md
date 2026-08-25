---
name: threads
description: Use ONLY when threads are explicitly called for — the user asks for one in chat (thread, epic, track, workstream, worktree, or the same words in any other language, including asking to work on something in parallel or in isolation), another skill or project instructions prescribe them, or you hit a concrete isolation problem — merge/edit conflicts from concurrent work, or someone else's changes appearing in git. Do NOT reach for threads just because a request splits into parallelizable chunks. Also covers writing a thread's goal and handling settled statuses (conflict, merge_blocked, failed, interrupted).
---

# Threads

A thread is a parallel agent work stream: its own git worktree, branch, and
fully isolated agent session (separate data directory, database, everything).
It exists to **isolate** work that would otherwise collide — not as a
general-purpose way to speed up multi-part requests.

## When to use threads

Threads are opt-in. Use them only when one of these holds:

1. **The user asked for one explicitly.** Any of the aliases counts — a
   *thread*, an *epic*, a *track*, a *workstream*, a *worktree*, the same
   words in whatever language the user writes in, or a request to work on
   something in parallel or in isolation. Whatever the label, they all map
   to the `thread_*` tools; there is no separate epic/track/worktree
   feature.
2. **Another skill or project instructions prescribe them** for the current
   task.
3. **You hit a real isolation problem**: concurrent work is stepping on
   itself (edit/merge conflicts between parallel efforts), or you notice in
   git that someone else is changing the working tree or branch under you.
   Moving the work into a thread's own worktree resolves the contention —
   you may create one on your own initiative here.

Do **not** reach for threads outside those cases. In particular:

- A request that merely *splits into independent chunks* ("do X and Y and
  Z") is not, by itself, a reason for threads — do the work yourself, or
  use subagents. Threads carry real overhead: a worktree, a branch, a full
  agent session, and a git merge on the way back.
- Sub-tasks that touch the same files or depend on each other's output
  belong in one place — splitting them manufactures the very merge
  conflicts threads exist to avoid.
- Exploratory/read-only work (research, locating code) is a job for the
  `Explore`/general-purpose agent tools — there's nothing to merge back.

When threads are warranted, prefer fewer, larger ones over many tiny ones,
and split along **disjoint file sets** so the merges stay clean.

## Naming

Pick a short slug: lowercase letters, digits, hyphens (e.g. `auth-refactor`,
`fix-flaky-tests`). It becomes the branch name (`thread/<name>`) and the
worktree directory name, and must be unique among live threads.

## Writing a self-contained goal

The thread's agent shares **none** of your context — no conversation
history, no earlier exploration, nothing you've already figured out. The
`goal` you pass to `thread_create` is the entire brief it gets. Write it like
a ticket handed to a new hire who just joined the repo:

- Name the exact files/directories in scope, and call out anything adjacent
  that's off-limits (so two threads don't collide).
- State acceptance criteria concretely: what should build, what tests should
  pass, what behavior should hold.
- Repeat any repo conventions the thread needs and won't discover on its
  own from context (e.g. "read AGENTS.md first", "match the comment style in
  package X", "add tests next to the existing ones").
- If the task depends on something another thread is producing, don't split
  it — sequence it yourself instead.

A goal like "improve the auth code" is not self-contained; a goal like
"internal/auth/token.go: fix the expiry check described below to use UTC
instead of local time, add a table-driven test in token_test.go covering
the DST boundary, keep the exported API unchanged" is.

## Fan-out and monitoring

1. `thread_create` once per independent chunk of work. Leave `merge_policy`
   at its default (`auto`) unless the user wants to review before merging
   — see [Manual merge](#manual-merge-policy) below.
2. **Then finish your turn.** Say what you launched and stop. A thread's
   outcome is delivered to you on its own the moment it settles — you are
   woken with it, one thread at a time, and `thread_status` on that thread
   tells you how it actually landed. Nothing needs to be polled or waited
   on to find that out, and handling each thread as it arrives beats
   sitting until the slowest one is done.

Do not poll with `bash sleep`: it costs a turn, tells you nothing when it
returns, and the threads were going to reach you without it.

### When to block instead

until every named thread leaves pending/running/merging. It is for the one
case the arriving-outcome path does not cover: **you cannot do anything
useful until several threads have all settled together** — comparing their
results against each other, merging them in a fixed order, reviewing the
combined diff. Waiting on a single thread is never right; its outcome was
already coming to you.

It gives up after ten minutes by default (`timeout_seconds` to change that).
A timeout is not a reason to wait again — the threads are still running and
still report themselves, so end your turn unless you truly cannot proceed
without them. And a message from the user ends the wait immediately: answer
them, the threads keep going.

A running thread is not steerable. `thread_send` is not in the default tool
set, and where it is enabled it does not interrupt anything: a thread that
is mid-turn only reads the message as its *next* prompt, and a thread deep
in a sub-agent call can be minutes from that point. A deadline ("you have
five minutes") sent that way is read after the deadline has passed. So
treat a launched thread as running to completion: put everything it needs
in the goal, and if you need it to stop now, that is `thread_remove` with
`force`, not a message.

## Handling settled statuses

- **`merged`** — done; nothing to do but tell the user and, once you've
  confirmed you don't need the worktree anymore, `thread_remove` it.
- **`conflict`** — the merge hit conflicting hunks, left unresolved in the
  thread's worktree. Do not try to resolve them yourself from outside that
  worktree. If you have `thread_send`, send the thread's agent an
  instruction to resolve the conflicts (open the conflicted files,
  pick/combine the changes, stage them) and report back, then call
  `thread_merge` again. Without it, report the conflict and the conflicted
  paths to the user and let them decide — resolving in the thread's
  worktree is theirs to drive.
- **`merge_blocked`** — the merge couldn't even be attempted cleanly (e.g.
  the base branch is checked out in your own worktree and it's dirty).
  `thread_status` gives the reason in its `error` field. This needs the
  user: either they clean/commit their own working tree, or you re-run
  `thread_merge` once it's safe.
- **`failed`** — the thread's agent run errored out. `thread_status` gives
  the error. Either `thread_send` a fix-up instruction to retry (if you have
  that tool), or `thread_remove` (with `force` if needed) and report the
  failure.
- **`interrupted`** — the thread's process was torn down mid-run (e.g. Sennit
  restarted) rather than the task failing. Resuming it in place is a
  `thread_send`: that respawns its agent session in the same worktree with
  the message as new input. Without that tool, say so and let the user
  resume it from the thread view — do not `thread_remove` an interrupted
  thread to "restart" it, since that throws away work already in its
  worktree.

## Manual merge policy

Pass `merge_policy: "manual"` when the user wants to review a thread's diff
before it lands, or wants it published as a PR instead of merged locally.
A manual thread settles at `completed` and stays there — it does not
merge itself. From there you have two options:

- Leave it: tell the user the branch (`thread/<name>`) and worktree path so
  they can review and merge it themselves, or call `thread_merge` once they
  approve.
- Open a PR: `thread_send` an instruction telling the thread's agent to run
  `gh pr create` from inside its own worktree (it has the branch checked
  out and pushed access to do so), then report the PR URL back via its
  result.

## Cleanup

Once a thread is merged (or its branch has been handed off and is no longer
needed locally), `thread_remove` it to free the worktree. Pass
`delete_branch: true` if the branch itself should go too (skip this for a
thread whose branch is now upstream of a PR). Use `force` only when removing
an unmerged or still-active thread deliberately — it discards uncommitted
work in that thread's worktree.
