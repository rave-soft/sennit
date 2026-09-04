Launch an agent to investigate the requested work, or perform it if `subagent_type` names an agent with write access. The delegation starts asynchronously: this call acknowledges the task immediately and returns its id, so continue useful independent work while it runs.

Omit `subagent_type` to get the general-purpose agent: it is read-only (it can search, read, and report findings, but cannot edit files, write files, or run commands), the right choice for investigation, or when no listed agent matches the work. Pass `subagent_type` to choose a specific listed agent instead; whether that agent can perform the work rather than only investigate it depends on its own configured tools (see its description below). `description` is a short label for the delegation and shows up as its session's title.

A delegated agent that writes shares the workspace with you. Avoid concurrent edits to the same files unless the task is explicitly coordinated for that purpose.
