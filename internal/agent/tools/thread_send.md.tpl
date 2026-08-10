Send a follow-up message to a thread's agent.

Queues `message` as the next prompt for the thread's session. If the thread
was interrupted (its workspace is not currently running, e.g. after a
process restart), sending a message respawns its workspace and resumes it
from where it left off — the worktree and branch are untouched on disk.

Parameters:
- `id` (required): the thread's ID or name.
- `message` (required): the instruction to send.
