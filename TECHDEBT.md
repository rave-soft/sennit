# Technical debt

Only active items belong here. Remove an entry when it is resolved or deliberately
rejected; git history keeps the investigation.

This is the only backlog. `REFACTORING.md` was a second one with the same
rule and 99 closed entries in it; it was folded into this file on
2026-09-15 and deleted. Its durable conclusions — how to review, and the
traps that cost this tree a day each — live in `AGENTS.md` under
"Reviewing and fixing". The audits themselves are in git history
(`git log -- REFACTORING.md`).

## Gemini steering compatibility

Steering after tool results reaches Fantasy as separate `Tool` and `User`
messages. Its Gemini adapter maps both to adjacent `user` contents. Anthropic and
OpenAI-compatible providers accept this flow, but Gemini has not been verified.
Sennit cannot merge the messages without dropping either tool results or steering;
that conversion happens inside Fantasy.

Next step: run a real `user → assistant(tool_calls) → tool → user(steering)`
request against Gemini. If Gemini rejects it, fix Fantasy's Google adapter by
merging adjacent contents that map to the same Gemini role.

## GitHub Copilot identity

The Copilot provider uses the inherited VS Code/Copilot OAuth client ID and presents
requests as `GitHubCopilotChat`/VS Code (`internal/oauth/copilot/http.go:11`),
while signup identifies the editor as Sennit. Keeping the provider is
intentional, but the identity mismatch remains.

Next step: either register a Sennit-owned GitHub OAuth application and use an honest
user agent, if GitHub permits Copilot API access for it, or remove the provider.

## The Windows process tree escapes the job for an instant

`internal/shell/exec_windows.go` now mirrors the Unix handler: the child
runs in a process group of its own, a Job Object with
`JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` holds the tree, cancellation sends
CTRL_BREAK and then terminates the job, and `WaitDelay` bounds a `Wait`
that a lingering grandchild would otherwise hang forever.

What is left is the gap that cannot be closed through `os/exec`: the
child is assigned to the job immediately after `CreateProcess` returns,
not before it runs, because Go does not hand back the main thread handle
a `CREATE_SUSPENDED` start would need resuming. A grandchild spawned in
the first instants of the child's life escapes the job. The window is
microseconds.

Next step: only worth closing by enumerating the new process's threads
(`CreateToolhelp32Snapshot`/`ResumeThread`) and starting suspended —
more Windows-specific syscall code, for a window that needs a process to
fork within microseconds of starting. Recorded rather than scheduled.

Also open: none of this has run on a real Windows machine. CI's Windows
runner executes `internal/shell/isolation_windows_test.go`, which is its
first execution anywhere.

## Docker MCP rendering is spread across the UI

The probe and its cache were extracted, but the drawing is spread over
`ui/chat`, `ui/dialog` and `ui/model` — eight files.

Next step: a package of its own is worth it only if a second vendor like this
appears. Generalising over one case would be the wrong move, so this stays
recorded rather than scheduled.

## `ui/model` and `ui/dialog` read the whole config

52 direct `Config()` calls between them (24 when this was first recorded, so it
is growing). `ui/chat` shows the pattern to follow: narrow interfaces
(`ModelConfig`, `CustomAgentConfig`) naming exactly what the component reads.

Next step: this is readability and encapsulation, not coupling — linkage does
not change, and it must not be sold as if it did. Worth doing where a component
is being touched anyway.

## `attachSkill` contradicts what the docs promise

`internal/ui/model/ui.go:1244` attaches the raw `SKILL.md` as an attachment,
while the documentation promises that a skill with `user-invocable: true` and
`disable-model-invocation: true` "behaves like a command". The model does read
the attached file, so there is no proven wrong outcome — but the promise and
the behaviour disagree.

Next step: choosing between "change the behaviour" and "change the promise" is
a product decision, not a technical one.
