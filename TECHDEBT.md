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

## GitHub Copilot identity

The Copilot provider uses the inherited VS Code/Copilot OAuth client ID and presents
requests as `GitHubCopilotChat`/VS Code, while signup identifies the editor as
Sennit. Keeping the provider is intentional, but the identity mismatch remains.

Next step: either register a Sennit-owned GitHub OAuth application and use an honest
user agent, if GitHub permits Copilot API access for it, or remove the provider.
