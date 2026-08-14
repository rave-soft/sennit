Stop a running background task.

Cancels the task's in-flight agent run and leaves it in a terminal
"interrupted" state with the given reason recorded as its error. Has no
effect on a task that has already finished — its real outcome (completed,
failed) is not overwritten.

Parameters:
- `id` (required): the task's ID (see `task_list`).
- `reason` (optional): why the task is being cancelled, recorded on its
  record.
