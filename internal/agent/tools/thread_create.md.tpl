Create a thread: a parallel agent work stream that runs in its own git
worktree and branch, fully isolated from your own workspace (separate data
directory, database, and agent session).

Use this ONLY when threads are explicitly called for: the user asked for one
under any name — "thread", "epic", "track", "workstream", "worktree", or
"work on this in parallel/isolation" — a skill or project instructions
prescribe one, or you need to isolate work that is colliding (edit/merge
conflicts between concurrent efforts, or someone else's changes showing up
in git). A request that merely splits into independent chunks is not a
reason to create threads — do the work yourself or use subagents.

The thread's agent starts on `goal` immediately in the background; use
`thread_status` to check on it, `thread_send` for follow-up
instructions, and `thread_merge` to fold its branch back in once done.

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
