Send a follow-up message to a strand's agent.

Queues `message` as the next prompt for the strand's session. If the strand
was interrupted (its workspace is not currently running, e.g. after a
process restart), sending a message respawns its workspace and resumes it
from where it left off — the worktree and branch are untouched on disk.

Parameters:
- `id` (required): the strand's ID or name.
- `message` (required): the instruction to send.
