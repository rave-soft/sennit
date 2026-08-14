Get a task's current status and, once it has finished, its final answer.

If the task is still running, this reports its current status instead of
a result — check back later rather than polling in a tight loop.

Parameters:
- `id` (required): the task's ID (see `task_list`).
