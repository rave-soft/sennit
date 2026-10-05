Wait for one or more delegations to reach a terminal status.

Use this when your next action depends on a delegation's result. This is an
explicit event-driven wait: it resumes as soon as every requested delegation
has finished, failed, been interrupted, or been cancelled. It does not use a
timer, shell command, or polling loop. Other delegations continue running while
you wait. A person's message interrupts the wait so you can respond immediately;
the delegations continue and report automatically. Do not immediately resume
waiting instead of answering the person.

The final reports are still delivered automatically through the normal
completion inbox. After this tool returns, inspect the reported statuses and
continue the work. Do not use `bash` sleep or `job_output` to wait for an
agent.

Parameters:
- `ids` (required): one or more delegation IDs or thread names from `agent_list`.
  Each must be a delegation you started. Duplicate IDs are accepted and waited
  once.
