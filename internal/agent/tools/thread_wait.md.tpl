Block until the given threads settle: none of them are pending, running, or
merging anymore.

A thread's result arrives on its own once it finishes — you do not need this
tool to learn that a thread is done, not even when several are running. Left
alone, each one reports itself as it lands, and you can merge or review it
then, while the rest keep going.

Reach for this tool only when you cannot do anything useful until *several*
threads have all settled together — comparing their results against each
other, merging them in a fixed order, reviewing the combined diff. Waiting
on a single thread is never the right call.

The wait ends early if the user sends a message: they are waiting on you,
not on the threads, so answer them. The threads keep running and report
themselves when they finish.

Parameters:
- `ids` (optional): thread IDs or names to wait for. Omit to wait for every
  thread.
- `timeout_seconds` (optional): give up and return after this many seconds.
  Defaults to 600. Pass a negative value to wait with no timeout at all,
  which only ends when the threads settle, the user speaks, or the turn is
  canceled.
