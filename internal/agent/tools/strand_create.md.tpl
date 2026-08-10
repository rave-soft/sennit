Create a strand: a parallel agent work stream that runs in its own git
worktree and branch, fully isolated from your own workspace (separate data
directory, database, and agent session). This is the tool to reach for when
the user asks for parallel or isolated work under any name — an "epic", a
"track", a "workstream", a separate "worktree", or just "do these in
parallel" — they all mean strands.

Use this to fan work out to an independent agent that can run concurrently
with you — e.g. a large refactor, an exploratory spike, or a task you want
to keep off your own branch. The strand's agent starts working on `goal`
immediately in the background; use `strand_status` or `strand_wait` to check
on it, `strand_send` to give it follow-up instructions, and `strand_merge`
to fold its branch back in once it's done.

Parameters:
- `name` (required): a short slug (lowercase letters, digits, hyphens) that
  becomes the strand's branch name (`strand/<name>`) and worktree directory.
  Must be unique.
- `goal` (required): the task to hand to the strand's agent.
- `base_branch` (optional): the branch to fork from. Defaults to the
  repository's currently checked-out branch.
- `merge_policy` (optional): `auto` (default) merges the strand back into
  its base branch automatically when it finishes successfully; `manual`
  leaves it at `completed` for you to review and merge yourself with
  `strand_merge`.
