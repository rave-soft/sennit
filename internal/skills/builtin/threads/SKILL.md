---
name: threads
description: Use when the user's request splits into multiple independent chunks of work you could parallelize — e.g. "do X and Y and Z" across disjoint files or subsystems — and you're deciding whether to fan work out to thread_create, how to write a thread's goal, or how to handle a thread's status (conflict, merge_blocked, failed, interrupted) once it settles. Also applies whenever the user asks for parallel/isolated work under other names — epic, track, workstream, worktree, "работай параллельно", "эпик", "трек", "ворктри" — these all map to threads.
---

# Threads

A thread is a parallel agent work stream: its own git worktree, branch, and
fully isolated agent session (separate data directory, database, everything).
Use threads to fan independent work out to concurrent agents instead of doing
it yourself serially.

Users describe this concept with many words — an *epic*, a *track*, a
*workstream*, a *worktree*, "работай над этим параллельно/изолированно", or
simply "do these in parallel". Whatever the label, if the user wants isolated
parallel work streams, use the `thread_*` tools; there is no separate
epic/track/worktree feature.

## When to split work into threads

Split when the sub-tasks are genuinely independent and touch **disjoint file
sets**: separate packages, separate features, a batch of unrelated bug fixes.
Each thread merges back with a real git merge, so overlapping edits mean
conflicts you'll have to resolve later instead of coordination you could have
done up front.

Do **not** reach for threads when:

- The task is a handful of small, sequential edits — just do them yourself.
- The sub-tasks touch the same files or depend on each other's output (e.g.
  "add a field to this struct" then "use it in five call sites") — one
  thread doing both, or you doing it directly, avoids a merge conflict.
- The work is exploratory/read-only (research, locating code) — that's a
  job for the `Explore`/general-purpose agent tools, not a thread, since
  there's nothing to merge back.

When in doubt, prefer fewer, larger threads over many tiny ones — each thread
carries the fixed overhead of a worktree, a branch, and its own agent session.

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
- **`interrupted`** — the thread's process was torn down mid-run (e.g. Braid
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
