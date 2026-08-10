Merge a strand's branch back into its base branch.

Manual-policy strands are merged this way once their run completes. Retry
this after resolving a merge conflict reported by `strand_status` — fix the
conflicted files in the strand's worktree, stage them, then call this again
to finish the merge.

Parameters:
- `id` (required): the strand's ID or name.
