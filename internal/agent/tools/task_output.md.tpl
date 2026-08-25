Show a task's progress so far: the most recent messages from its child
session, the same transcript the UI would show, without waiting for it to
finish.

You do not need this to find out how a task turns out. Its outcome is
delivered to you on its own — see `task_result` — so watching a running
task is not supervising it, and calling this round after round is not
waiting: it spends turns on a transcript that will be summarized for you
anyway, and fills your context with a session that has yet to say
anything final.

Call it when you need to see how a task is working rather than what it
concluded: it looks stuck, or headed somewhere wrong, and you are deciding
whether to redirect it (`task_send`) or stop it (`task_cancel`); or its
completion has arrived and the answer only makes sense with the reasoning
that led to it.

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
