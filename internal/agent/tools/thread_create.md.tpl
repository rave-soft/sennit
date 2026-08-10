Create a thread: a parallel agent work stream that runs in its own git
worktree and branch, fully isolated from your own workspace (separate data
directory, database, and agent session). This is the tool to reach for when
the user asks for parallel or isolated work under any name — an "epic", a
"track", a "workstream", a separate "worktree", or just "do these in
parallel" — they all mean threads.

Use this to fan work out to an independent agent that can run concurrently
with you — e.g. a large refactor, an exploratory spike, or a task you want
to keep off your own branch. The thread's agent starts working on `goal`
immediately in the background; use `thread_status` or `thread_wait` to check
on it, `thread_send` to give it follow-up instructions, and `thread_merge`
to fold its branch back in once it's done.

Parameters:
- `name` (required): a short slug (lowercase letters, digits, hyphens) that
  becomes the thread's branch name (`thread/<name>`) and worktree directory.
  Must be unique.
- `goal` (required): the task to hand to the thread's agent.
- `base_branch` (optional): the branch to fork from. Defaults to the
  repository's currently checked-out branch.
- `merge_policy` (optional): `auto` (default) merges the thread back into
  its base branch automatically when it finishes successfully; `manual`
  leaves it at `completed` for you to review and merge yourself with
  `thread_merge`.
