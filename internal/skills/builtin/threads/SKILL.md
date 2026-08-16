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
2. `thread_wait` (with the returned IDs, or no `ids` for all of them) to
   block until every thread leaves pending/running/merging. Use
   `timeout_seconds` if you don't want to wait indefinitely.
3. `thread_status` per thread once `thread_wait` returns, to see how each
   one actually landed — `thread_wait` only tells you they've settled, not
   into what state.

## Handling settled statuses

- **`merged`** — done; nothing to do but tell the user and, once you've
  confirmed you don't need the worktree anymore, `thread_remove` it.
- **`conflict`** — the merge hit conflicting hunks, left unresolved in the
  thread's worktree. `thread_send` an instruction telling the thread's agent
  to resolve the conflicts (open the conflicted files, pick/combine the
  changes, stage them) and report back, then call `thread_merge` again. Do
  not try to resolve the conflict yourself from outside the thread's
  worktree.
- **`merge_blocked`** — the merge couldn't even be attempted cleanly (e.g.
  the base branch is checked out in your own worktree and it's dirty).
  `thread_status` gives the reason in its `error` field. This needs the
  user: either they clean/commit their own working tree, or you re-run
  `thread_merge` once it's safe.
- **`failed`** — the thread's agent run errored out. `thread_status` gives
  the error. Decide whether to `thread_send` a fix-up instruction to retry,
  or `thread_remove` (with `force` if needed) and report the failure.
- **`interrupted`** — the thread's process was torn down mid-run (e.g. Sennit
  restarted) rather than the task failing. `thread_send` a message to resume
  it — this respawns its agent session in the same worktree with the
  message as new input.

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
