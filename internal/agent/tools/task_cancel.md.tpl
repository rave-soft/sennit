Stop a running background task.

Cancels the task's in-flight agent run and leaves it in a terminal
"interrupted" state with the given reason recorded as its error. Has no
effect on a task that has already finished — its real outcome (completed,
failed) is not overwritten.

Cancelling a task cancels the tasks it started too: work delegated by
something you just stopped has nobody left to report to, and left running
it would keep editing the workspace unsupervised.

You can only cancel a task you started (`task_list` shows exactly those).
Your own id is refused — stopping your own work means ending your turn
with your report, not cancelling yourself — and so is the id of a
delegation you are running under.

Parameters:
- `id` (required): the task's ID (see `task_list`).
- `reason` (optional): why the task is being cancelled, recorded on its
  record and on every task cancelled along with it.
