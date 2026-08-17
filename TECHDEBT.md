# Technical debt

What is still open after the Crush fork, the rebrand, and the removal of the
external services. Every entry carries its reason and a concrete next step, so
it can be picked up again without repeating the investigation.

Closed entries do not accumulate here: once a debt is paid the entry is
deleted, and the history stays in git.

## Open debt

- **Gemini and two consecutive user contents.** Steering is delivered as its
  own `user` message after all tool results
  (`internal/agent/completion_inbox.go`, `prepareStep` in
  `internal/agent/turn.go`). Anthropic merges such a pair itself; for
  OpenAI-compatible providers this was verified with a live request (200, and
  the model acted on the steering specifically); in OpenAI Responses keeping
  them separate is what the protocol expects. On Gemini the `fantasy` adapter
  puts tool results into a `genai.Content{Role: user}` and then creates a
  **second** `Content{Role: user}` right after it; nothing merges
  same-role contents, not in the adapter and not here, and Gemini answers 400
  to non-alternating roles in some cases. Whether it accepts this particular
  shape is an empirical question, and there is no key to ask it with. Next
  step: run one real `user → assistant(tool_calls) → tool → user(steering)`
  request against Gemini once a key is available; if it 400s, merge
  consecutive user contents, preferably in the adapter, otherwise by
  normalizing before the call. Until then, mid-turn steering on Gemini is an
  untested path.

## Decisions deferred pending confirmation

### GitHub Copilot

Not removed, and keeping it is a deliberate decision (2026-08) rather than an
omission. It is a model provider, like OpenAI or Anthropic, not a Charm
service: removing it takes a backend away from the user, it does not detach
the fork from someone else's infrastructure.

What ships with it is worth knowing, because "just another provider" does not
describe the situation fully. The client authenticates with someone else's
OAuth application and presents itself as someone else's product:

- `clientID = "Iv1.b507a08c87ecfe98"` (`internal/oauth/copilot/oauth.go`) —
  the Copilot/VS Code client ID, not one registered to rave-soft.
- `userAgent = "GitHubCopilotChat/0.32.4"`, `editorVersion = "vscode/1.105.1"`
  (`internal/oauth/copilot/http.go`) — to GitHub's API, Sennit presents itself
  as the Copilot Chat extension for VS Code.

All of this is inherited from upstream rather than introduced here. An
inconsistency lives next to it: `SignupURL` in
`internal/oauth/copilot/urls.go` carries `editor=sennit`, so signup is branded
as Sennit while the traffic claims to be VS Code.

Three possible next steps, none of them started:

1. Do nothing — the status quo this entry records.
2. Remove Copilot entirely. The surface is larger than it looks: it is
   mentioned in 36 files, 24 of them non-test. The `internal/oauth/copilot`
   package (419 lines) and the `internal/ui/dialog/oauth_copilot.go` dialog
   (77 lines) go entirely; then the branches in `internal/cmd/login.go` and
   `logout.go`, the refresh path in
   `internal/config/credentials/credentials.go`, `SetupGitHubCopilot` and the
   token exchange in `internal/config/{config,store,providers_merge,reload}.go`,
   the dialog wiring in `internal/ui/model/`, the "model not enabled in
   Copilot" diagnostic in `internal/agent/turn.go`, and the plumbing in
   `internal/workspace/`. After that `catwalk.InferenceProviderCopilot` is no
   longer used anywhere.
3. Keep it, but stop impersonating VS Code: register a GitHub OAuth App of our
   own and send an honest `User-Agent`. Three constants to edit, but it needs
   an application registered, and whether GitHub grants such a client access
   to the Copilot API is an empirical question — theirs is a restricted list.

## Limitations of imported agent definitions

Not debt but the contract of `sennit import` — documented here because the
code points at this section (`internal/config/import.go`, the `sennit-config`
skill).

User's decision (2026-08): Sennit no longer scans another tool's config
directories on its own. Discovery — `internal/config/agents_markdown.go`
(`agentDirs`) and `internal/config/load.go` (`GlobalSkillsDirs`,
`projectSkillSubdirs`) — reads only `.sennit/agents` and `.sennit/skills`
(plus their global equivalents and `options.skills_paths`). It used to take in
`.claude/agents`, `.opencode/agent`, `.agents/skills`, `.claude/skills`,
`.cursor/skills`, and `.opencode/skills` as well — meaning Sennit silently
trusted the contents of directories another tool writes, without validating
them and without telling the user what failed to apply.

Bringing files over now goes through an explicit import only — `sennit import
claude|opencode [--skills] [--agents] [--dry-run] [--global] [--force]`
(`internal/config/import.go`, `internal/cmd/import.go`). The import copies
into `.sennit/skills`/`.sennit/agents` rather than reading a foreign directory
on the fly, and prints a report for every file (`imported`/`adjusted`/
`skipped` plus the reason or warnings) — the same limitations below still
apply, but the user now sees them at import time instead of discovering them
afterwards in the logs.

- `permission:` blocks from opencode files are **not applied**, neither on a
  normal load nor after an import. A role locked to read-only there gets
  everything listed in its `tools` under Sennit. The import reports this as a
  warning ("permission block is not supported; restrict tools via the tools
  list instead") and leaves a comment in the frontmatter, but does not write
  the field itself. Restrict via the `tools` list or the config's
  `permissions` section instead.
- The `model` field from foreign files understands `provider/model-id`
  references when such a model exists among the configured providers — it
  resolves through `config.ResolveModelString`. There are no `large`/`small`
  slots for `Agent.Model` any more: those words carry no special meaning even
  in Sennit's own agent files and are treated like any other unresolvable
  string. Values that resolve to nothing (`opus`, say — a model name from
  another tool that is not in the config — or literally `large`/`small`) are
  not dropped silently on import: the `model` field is omitted (the agent
  inherits the main model), the original value stays in the frontmatter as a
  comment (`# original model: ... — not available`), and the import reports it
  as a warning rather than an `slog.Debug`.
- `reasoning_effort` / opencode's `effort`: `low`/`medium`/`high` carry over
  as they are; close but non-standard values (`max`, `minimal`, ...) map onto
  the nearest valid level with a warning; unrecognized ones are dropped with a
  warning and left as a frontmatter comment.
- opencode's `temperature`/`top_p`: `config.Agent` has no such fields. The
  import neither rejects the file nor invents a field — the value stays as a
  frontmatter comment and the import reports a warning.
- Tool names with no counterpart (`WebSearch`, for instance) are not dropped
  silently on import — they are reported as a warning naming the tool. On a
  normal load of `.sennit/agents`, the Claude Code name translation
  (`Read`→`read` and so on, `ClaudeToolNames` in `agents_markdown.go`) no
  longer applies at all: files in Sennit's own directory are expected to name
  tools Sennit's way already. The translator was not deleted — it is used by
  the import only.
- Delegation is single-level: a role cannot call another role.
