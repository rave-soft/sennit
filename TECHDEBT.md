# Technical debt

Only active items belong here. Remove an entry when it is resolved or deliberately
rejected; git history keeps the investigation.

## Gemini steering compatibility

Steering after tool results reaches Fantasy as separate `Tool` and `User`
messages. Its Gemini adapter maps both to adjacent `user` contents. Anthropic and
OpenAI-compatible providers accept this flow, but Gemini has not been verified.
Sennit cannot merge the messages without dropping either tool results or steering;
that conversion happens inside Fantasy.

Next step: run a real `user → assistant(tool_calls) → tool → user(steering)`
request against Gemini. If Gemini rejects it, fix Fantasy's Google adapter by
merging adjacent contents that map to the same Gemini role.

## Screen model decomposition

`internal/ui/model` still contains about 14,700 non-test lines. Extracting chat,
threads, the dock, and shared caches exhausted the clean package boundaries: most
remaining code consists of methods on `*UI` and `*Root`.

The attached-thread request/lifecycle state is now isolated from `Root`; its
release boundary is covered by tests. Next step: continue the state-object refactor
in `UI`, splitting cohesive screen state and behavior with tests around each
boundary before extraction.

## Multiple-agent configuration

The coordinator and runtime builder still assume a single primary agent/model in
several paths (`internal/agent/coordinator.go`, `internal/agent/runtime_builder.go`,
`internal/app/app.go`). This blocks making agent selection and model configuration
fully dynamic.

Next step: define the runtime configuration per agent, route model selection through
that value, then remove the legacy top-level agent-config concept.

## GitHub Copilot identity

The Copilot provider uses the inherited VS Code/Copilot OAuth client ID and presents
requests as `GitHubCopilotChat`/VS Code, while signup identifies the editor as
Sennit. Keeping the provider is intentional, but the identity mismatch remains.

Next step: either register a Sennit-owned GitHub OAuth application and use an honest
user agent, if GitHub permits Copilot API access for it, or remove the provider.

## File tracker path identity

`internal/filetracker` computes relative paths from raw path spellings. On Windows,
short and long forms of the same path can differ, unlike the canonicalized checks
used elsewhere.

Next step: canonicalize the workspace and tracked path before `filepath.Rel`, then
add a regression test covering equivalent path aliases.
