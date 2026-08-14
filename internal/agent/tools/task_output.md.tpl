Show a task's progress so far: the most recent messages from its child
session (the same transcript the UI would show), without waiting for it
to finish.

Only user and assistant text is included — no tool calls, tool results,
or reasoning — and only the most recent messages, not the whole
transcript, so checking in on a task does not flood your own context. If
there are more messages than shown, the response says so ("showing last N
of M") instead of silently hiding them; ask again with a higher `limit`
for more, up to the maximum.

Parameters:
- `id` (required): the task's ID (see `task_list`).
- `limit` (optional): how many of the most recent messages to return
  (default 20, maximum 100).
