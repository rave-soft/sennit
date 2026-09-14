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

## Windows does not kill the process tree

`interp.DefaultExecHandler`, which `internal/shell/exec_windows.go` uses
unchanged, sets no `SysProcAttr` at all: it signals one process and never sets
`WaitDelay`. Grandchildren outlive a cancelled command, and `Wait` can hang
while one of them holds the output pipe open. Unix closes exactly this through
`Setsid` and killing the group by negative PID.

The comment that used to promise `CREATE_NEW_PROCESS_GROUP` coverage was
corrected to the truth in `9364e2c87`; the implementation was deliberately not
written. Its first execution anywhere would be CI's Windows runner, and a
mistake in the order of job assignment only shows up at runtime.

Next step: a Job Object with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`, assigning
the process at launch via `CREATE_SUSPENDED`. Separate work, on a real Windows
machine.

## The transcript still splits MCP tool names naively

`internal/ui/chat/mcp.go:37` calls `proto.SplitMCPToolName(name, nil)` — the
naive "first underscore wins" fallback — while the permission dialog passes the
real server list (`internal/ui/dialog/permissions.go:643`). A server whose name
contains an underscore is rendered wrong in the transcript and right in the
dialog.

Next step: the chat renderer has no access to the server list, and threading it
through means changing the `ToolRenderOpts` contract. Do that deliberately, not
as a drive-by.

## There is no "turn started" event

The turn timer does not start when the queue hands a prompt to the next turn,
because nothing in the system announces that a turn began — the UI infers it
from the first thing the turn emits, and a queued prompt emits nothing until
the model answers.

Next step: closes only together with such an event. Adding one touches the
agent's dispatch path, so it is its own piece of work.

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
