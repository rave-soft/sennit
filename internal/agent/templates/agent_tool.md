Launch an agent to investigate or perform the requested work. The delegation starts asynchronously: this call acknowledges the task immediately and returns its id, so continue useful independent work while it runs.

Pass `subagent_type` to choose a specific agent; omit it to get the general-purpose agent, which is the right choice when no listed agent matches the work. `description` is a short label for the delegation and shows up as its session's title.

Delegated agents share the workspace. Avoid concurrent edits to the same files unless the task is explicitly coordinated for that purpose.
