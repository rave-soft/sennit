Tear a thread down: cancel its workspace, remove its git worktree, and
delete its record.

Refuses to remove a thread that is still running or merging, and refuses to
remove one with unmerged, uncommitted changes in its worktree, unless
`force` is set.

Parameters:
- `id` (required): the thread's ID or name.
- `force` (optional): remove even if the thread is active or has unmerged,
  dirty changes. Cancels the thread's agent if it's still running.
- `delete_branch` (optional): also delete the thread's git branch.
