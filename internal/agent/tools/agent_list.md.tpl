List the delegations you can act on: their IDs, kind, status, and goals.

Two kinds appear here. Background **tasks** are your own subtree and
nothing else — the tasks you dispatched, the tasks those went on to
dispatch, however many levels down. Tasks started elsewhere are not
listed, and neither is your own task if you are yourself a delegation:
you are not one of your own background tasks. **Threads** are the
workspace's isolated worktrees, which are not nested and are all listed.

Each row is `id`, kind, status, name (threads only), then a one-line
summary.

Use this to see what delegated work exists before checking on one
(`agent_result`), watching it (`agent_output`), redirecting it
(`agent_send`) or stopping it (`agent_cancel`). Takes no parameters.
