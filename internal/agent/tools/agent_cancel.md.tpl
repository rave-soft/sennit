Stop a running delegation. Takes a task's ID or a thread's ID or name.

Cancels the delegation's in-flight agent run and leaves it in a terminal
"interrupted" state with the given reason recorded as its error. Has no
effect on one that has already finished — its real outcome (completed,
failed) is not overwritten.

Cancelling a task cancels the tasks it started too: work delegated by
something you just stopped has nobody left to report to, and left running
it would keep editing the workspace unsupervised. A cancelled thread keeps
its worktree and branch, so its work can still be inspected or resumed;
use `thread_remove` to clear it away.

You can only cancel a task you started (`agent_list` shows exactly those).
Your own id is refused — stopping your own work means ending your turn
with your report, not cancelling yourself — and so is the id of a
delegation you are running under.

Parameters:
- `id` (required): the delegation's ID, or a thread's name.
- `reason` (optional): why it is being cancelled, recorded on its record
  and on every task cancelled along with it.
