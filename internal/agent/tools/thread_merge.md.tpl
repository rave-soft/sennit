Merge a thread's branch back into its base branch.

Manual-policy threads are merged this way once their run completes. Retry
this after resolving a merge conflict reported by `thread_status` — fix the
conflicted files in the thread's worktree, stage them, then call this again
to finish the merge.

Parameters:
- `id` (required): the thread's ID or name.
