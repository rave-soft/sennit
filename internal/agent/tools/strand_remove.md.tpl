Tear a strand down: cancel its workspace, remove its git worktree, and
delete its record.

Refuses to remove a strand that is still running or merging, and refuses to
remove one with unmerged, uncommitted changes in its worktree, unless
`force` is set.

Parameters:
- `id` (required): the strand's ID or name.
- `force` (optional): remove even if the strand is active or has unmerged,
  dirty changes. Cancels the strand's agent if it's still running.
- `delete_branch` (optional): also delete the strand's git branch.
