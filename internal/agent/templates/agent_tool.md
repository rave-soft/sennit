Launch a task agent to investigate or perform the requested work. The delegation starts asynchronously: this call acknowledges the task immediately, and its terminal result is delivered separately to your completion inbox. Continue useful work while it runs and correlate the later completion with the task id.

You are never responsible for collecting that result. It reaches your next turn if you are still working, and wakes the session if you have already ended the turn — so when the delegation is all that is left to do, ending the turn is the correct way to wait for it, and checking on the task with `task_result` or `task_output` in the meantime is not.

Delegated agents share the workspace. Avoid concurrent edits to the same files unless the task is explicitly coordinated for that purpose.
