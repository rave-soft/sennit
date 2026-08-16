Send a follow-up message to a thread's agent.

Queues `message` as the next prompt for the thread's session. If the thread
was interrupted (its workspace is not currently running, e.g. after a
process restart), sending a message respawns its workspace and resumes it
from where it left off — the worktree and branch are untouched on disk.

The result tells you whether the thread's agent reads the message now or
only later. A thread that is mid-turn — especially one deep in a sub-agent
call, which can run for many minutes — does not see the message until that
turn ends, so a deadline or a course correction sent then steers nothing
in flight. When the result says the message was queued, treat the thread as
not yet informed: keep the deadline out of it, or wait for the thread's
completion and act on what it actually produced.

Parameters:
- `id` (required): the thread's ID or name.
- `message` (required): the instruction to send.
