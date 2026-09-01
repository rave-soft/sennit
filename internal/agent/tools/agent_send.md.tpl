Send a follow-up message into a delegation's session. Takes a task's ID or
a thread's ID or name.

If the delegation is still running (or idle with a live runtime), this
queues `message` as its next turn. If its runtime is not currently live —
for example after a process restart — sending reactivates it and
dispatches the message there.

A delegation stopped with `agent_cancel` is not reactivated this way:
cancelling was a decision, not a pause. Sending to a cancelled one fails —
start new work instead.

The result tells you whether the delegation's agent reads the message now
or only after the turn it is already running finishes. When it says the
message was queued, the agent has not been informed yet — do not count on
it having acted on the message.

For tasks this reaches only the ones you started. Your own id is refused,
and so is that of a delegation you are running under: what you have to say
to whoever is waiting on you belongs in your report.

Parameters:
- `id` (required): the delegation's ID, or a thread's name.
- `message` (required): the instruction to send.
