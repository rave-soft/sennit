Block until the given threads settle: none of them are pending, running, or
merging anymore.

A thread's result now arrives on its own once it finishes — you do not need
this tool just to learn that a single thread is done. Reach for it only when
you specifically need to block until several threads have all settled
together before continuing (e.g. before merging or reviewing their combined
output).

The wait ends early if the user sends a message: they are waiting on you,
not on the threads, so answer them. The threads keep running and report
themselves when they finish.

Parameters:
- `ids` (optional): thread IDs or names to wait for. Omit to wait for every
  thread.
- `timeout_seconds` (optional): give up and return after this many seconds.
  Omit or 0 for no timeout beyond the tool call's own cancellation.
