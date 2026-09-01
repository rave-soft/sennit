Get a delegation's current status, and its final answer if it has already
finished. Takes a task's ID or a thread's ID or name.

You do not need this to receive a delegation's result. Every delegation's
terminal outcome is delivered to you on its own: if it finishes while you
are still working, the completion reaches your next turn, and if you have
already ended your turn, its arrival wakes the session. Ending the turn is
how you wait for a delegation. Calling this in a loop is not waiting — it
spends turns to learn what you would have been told anyway.

Call it when a decision of yours needs the answer sooner than the
completion would bring it: whether to stop the delegation
(`agent_cancel`), redirect it (`agent_send`), or start work that only
makes sense once it has finished.

Parameters:
- `id` (required): the delegation's ID, or a thread's name (see
  `agent_list`).
