Send a follow-up message into a task's session.

If the task is still running (or idle with a live runtime), this queues
`message` as its next turn. If the task's runtime is not currently live —
for example after a process restart — sending a message reactivates it
and dispatches the message there, the same way `thread_send` resumes an
interrupted thread.

A task stopped with `task_cancel` is not reactivated this way: cancelling
was a decision, not a pause. Sending to a cancelled task fails — start a
new task instead.

Parameters:
- `id` (required): the task's ID (see `task_list`).
- `message` (required): the instruction to send.
