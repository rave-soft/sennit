Launch a task agent to investigate or perform the requested work. The delegation starts asynchronously: this call acknowledges the task immediately and returns its id, so continue useful independent work while it runs.

Delegated agents share the workspace. Avoid concurrent edits to the same files unless the task is explicitly coordinated for that purpose.
