---
name: sennit-config
description: Use when the user needs help configuring Sennit — writing sennitrc (the Bash config format) or sennit.json, setting up providers, models, LSPs, MCP servers, hooks, skills, permissions, or changing Sennit behavior.
---

# Sennit Configuration

Sennit supports two config formats:

- **`sennitrc`** — a Bash script that builds config by calling Sennit builtins.
  **Preferred.** Because it is real Bash you get includes, secrets,
  conditionals, and variables for free.
- **`sennit.json`** — static JSON. Fully supported; see
  [Legacy JSON format](#legacy-json-format).

Both are discovered together and deep-merged. Priority (highest to lowest):

1. `.sennit/sennit.json` — the canonical, highest-priority project config.
   This is also where `sennit config set` and agent-driven config writes land
   (the workspace scope), so it always wins on conflicts, including over
   `.sennitrc`/`sennitrc` in the same directory.
2. `.sennit/sennitrc` / `.sennitrc` / `sennitrc` / `.sennit.json` / `sennit.json`
   (project-local, closer-to-cwd wins; Windows uses `.\.sennitrc` /
   `.\sennitrc`)
3. `$XDG_CONFIG_HOME/sennit/sennitrc` or `~/.config/sennit/sennitrc`
   (`%XDG_CONFIG_HOME%\sennit\sennitrc` or
   `%USERPROFILE%\.config\sennit\sennitrc` on Windows)

`.sennit/sennit.json` and `.sennit/sennitrc` are checked at every directory in the
upward walk, same as the root-level names; `.sennit/sennit.json` is the highest
priority among the JSON variants, and `.sennit/sennitrc` the highest among the
`sennitrc` variants.

Config, skills, and agent definitions are picked up automatically (a 2s
background poll) — no restart needed after editing `sennit.json`/`sennitrc`,
adding or removing a `SKILL.md`, or adding/editing an agent markdown file,
whether the edit comes from you, the agent's own file tools, or a human.

Data directories (`~/.local/share/sennit` and `%LOCALAPPDATA%\sennit`) contain
machine-owned JSON state only; Sennit does not discover or execute a `sennitrc`
from those locations.

If a directory has both `sennitrc` and `sennit.json`, they merge (`sennitrc` wins
on conflicts) and Sennit logs a warning.

## sennitrc at a glance

A `sennitrc` is a plain Bash script executed at load time with the same embedded
shell the `bash` tool uses. It builds config by calling builtins (`provider`,
`model`, `mcp`, `lsp`, `hook`, `permissions`, `option`). Statements run top to
bottom; later statements win, and `remove`/`reset` operate on anything defined
earlier or pulled in via `source`.

```bash
#!/usr/bin/env bash
# Includes and secrets are just Bash.
source ~/.config/sennit/shared.sh

provider add anthropic --api-key "$ANTHROPIC_API_KEY"

model anthropic/claude-sonnet-4-20250514 --max-tokens 16384

option skill-path ./skills
permissions allow read ls grep edit
```

Values are ordinary Bash — quote and expand normally (`"$VAR"`, `$(cmd)`,
`${VAR:?required}`). A failing `$(command)` aborts the load.

`SENNIT_VERSION` is exported into the script so you can feature-detect the
running Sennit (it is the literal `devel` for local builds):

```bash
[[ "$SENNIT_VERSION" != devel ]] && lsp add gopls --command gopls
```

## Commands

All entity commands are verb-first. `remove` accepts `rm` as an alias. Booleans
accept `true/false/1/0/yes/no`, case-insensitive.

### providers

```bash
provider add <id> [flags]    # define/update; repeated calls merge
provider remove <id>         # alias: rm — removes the provider and its models
```

Flags: `--name`, `--type` (`openai`, `openai-compat`, `anthropic`, or a local
type like `ollama`, `lmstudio`, `llamacpp`), `--api-key`, `--base-url`,
`--disable BOOL`, `--flat-rate BOOL`, `--discover-models BOOL`,
`--system-prompt-prefix TEXT`, `--extra-header KEY VALUE` (repeatable),
`--extra-body JSON`, `--provider-options JSON`.

```bash
provider add deepseek \
  --type openai-compat \
  --base-url "https://api.deepseek.com/v1" \
  --api-key "${DEEPSEEK_API_KEY:?set DEEPSEEK_API_KEY}"
```

`proxy_url` (routes this provider's requests through an http/https/socks5
proxy, or forces a direct connection with the sentinel `"none"` even when
`HTTP_PROXY`/`HTTPS_PROXY` are set) has **no `provider add` flag**. Set it in
`sennit.json` only:

```json
{ "providers": { "deepseek": { "proxy_url": "socks5://localhost:1080" } } }
```

`proxy_url` goes through shell expansion in `sennit.json` (see [Shell
expansion](#shell-expansion-in-sennitjson)), so `"$CORP_PROXY"` works too.

### Model discovery

A custom provider (one with a `base_url` outside the built-in catwalk
catalog) auto-discovers its models from `/v1/models` on load when `models` is
empty, or always when `discover_models: true` — the discovered list is merged
onto the provider and persisted into the data-directory config
(`~/.local/share/sennit/sennit.json`) so later loads skip the HTTP round trip.
Force a full re-discovery (overwriting the persisted list) with:

```bash
sennit models refresh              # every custom provider
sennit models refresh my-local-llm # one provider
```

> [!IMPORTANT] Never guess a model ID. Before writing `provider/model-id`
> anywhere (agent frontmatter, `model add`, `sennit.json`), check it's real:
> `sennit_info` with `{"models_for": "<provider-id>"}`, or `sennit models
> <filter>` in bash. Router providers can carry thousands of models — a
> plausible-looking ID is not the same as a listed one.

### models

```bash
model add <provider>/<id> [flags]  # register a custom model (provider must exist)
model remove <provider>/<id>       # alias: rm
model [<provider>/<id>] [flags]    # set the model; no arg prints the current one
```

- `<provider>/<id>` is the same form `sennit models` prints. A missing slash is
  an error. `model add` requires the provider to already exist.
- `model add` flags: `--name`, `--context-window N`, `--default-max-tokens N`,
  `--can-reason BOOL`, `--supports-images BOOL`, `--price-input F`,
  `--price-output F`, `--price-cache-create F`, `--price-cache-hit F`,
  `--reasoning-effort low|medium|high`.
- `model` flags: `--think`, `--reasoning-effort`, `--max-tokens N`,
  `--temperature F`, `--top-p F`, `--top-k N`, `--frequency-penalty F`,
  `--presence-penalty F`, `--provider-options JSON`.
- `model` with no argument prints the current selection as `provider/id`,
  usable in `$(model)`.

There is a single configured model — Sennit picks a smaller/cheaper model
automatically for internal work like titles and summarization, and that
choice is not user-configurable.

Same rule as above: verify with `sennit_info {"models_for": "<provider>"}` or
`sennit models` before running `model <provider>/<id>` — an unresolvable
selection falls back silently to the default model.

`model add` flags cover the common `catwalk.Model` fields. Two fields have no
flag and are `sennit.json`-only: `reasoning_levels` (list of efforts the model
advertises) and `id`/`name` beyond the required minimum. A hand-written model
entry needs at least:

```json
{
  "id": "my-model", "name": "My Model",
  "context_window": 128000, "default_max_tokens": 8192,
  "cost_per_1m_in": 0, "cost_per_1m_out": 0,
  "cost_per_1m_in_cached": 0, "cost_per_1m_out_cached": 0,
  "can_reason": false, "supports_attachments": false
}
```

### mcp

```bash
mcp add <name> --type stdio|sse|http [flags]   # default type is stdio
mcp remove <name>                              # alias: rm
```

Flags: `--command CMD`, `--args ARG` (repeatable), `--env KEY VALUE`
(repeatable), `--url URL`, `--header KEY VALUE` (repeatable), `--timeout N`,
`--disabled BOOL`, `--disabled-tools TOOL` (repeatable), `--enabled-tools TOOL`
(repeatable), `--oauth BOOL`, `--oauth-client-id ID`, `--oauth-client-secret SECRET`,
`--oauth-callback-port PORT`.

```bash
mcp add github --type http \
  --url "https://api.githubcopilot.com/mcp/" \
  --header Authorization "Bearer $GH_PAT"

mcp add filesystem --command node --args /path/to/mcp-server.js
```

### lsp

```bash
lsp add <name> --command CMD [flags]
lsp remove <name>                     # alias: rm
```

Flags: `--args ARG` (repeatable), `--env KEY VALUE` (repeatable),
`--filetypes TYPE` (repeatable), `--root-markers MARKER` (repeatable),
`--timeout N`, `--disabled BOOL`, `--init-options JSON`, `--options JSON`.

```bash
lsp add go --command gopls --env GOPATH "$HOME/go"
lsp add typescript --command typescript-language-server --args --stdio
```

### hooks

```bash
hook add <event> --command CMD [--name NAME] [--matcher REGEX] [--timeout N]
hook remove <event> [--name NAME]    # alias: rm; without --name clears the event
```

Only named hooks can be removed individually — give a hook `--name` if you
intend to remove it later. See [Hooks runtime](#hooks-runtime) for how hooks
execute (stdin payload, env vars, decisions).

```bash
hook add PreToolUse --matcher "^bash$" --command ".sennit/hooks/no-haskell.sh" --name no-haskell
```

### permissions

```bash
permissions allow <tool> [<tool> ...]   # tools that skip permission prompts
permissions deny <tool> [<tool> ...]    # hide tools from the agent entirely
permissions bypass on|off               # DANGEROUS: skip every permission prompt
```

`deny` is the inverse of `allow`: it writes `options.disabled_tools`. A denied
tool is hidden from the agent, not merely prompted for.

`bypass on` writes `permissions.bypass = true`, which auto-approves every
permission request from process start — the persisted equivalent of always
running with `--yolo`. It is dangerous: the agent runs every tool, including
destructive ones, without asking. `sennit doctor` flags it as a warning. The
session-only `ctrl+y` toggle / `/yolo` command still work independently on
top of this and are not written to config.

### options

```bash
option <key> [value]
option reset <list-key>    # clear a list option back to empty
```

- **Boolean keys** (value optional, defaults `true`): `debug`, `debug-lsp`,
  `auto-lsp`, `progress`.
- **Boolean keys phrased positively** (stored as the negated field): `metrics`,
  `auto-summarize`, `default-providers`. Example: `option metrics false`
  disables metrics.
- **String keys**: `data-directory`, `initialize-as`, `notifications`.
- **Integer keys**: `history-retention-days` (age, in days, after which `sennit
  gc` deletes old sessions/threads; default 90, 0 keeps history forever — see
  [Maintenance](#maintenance)).
- **Attribution keys**: `attribution-trailer-style` (`none`, `assisted-by`) and
  `attribution-generated-with` (boolean). Old configs with `co_authored_by:
  true` now migrate to `assisted-by`.
- **UI settings**: `option ui compact BOOL`, `option ui diff unified|split`,
  `option ui transparent BOOL`, `option ui scrollbar default|always|never`,
  `option ui completions-max-depth N`, `option ui completions-max-items N`,
  `option ui keybinding ACTION KEY...`. Keybinding actions are global names
  such as `commands` or grouped names such as `editor.newline` and
  `chat.page_up`; each declaration replaces that action's defaults. macOS
  defaults use `super+` (Command) where other platforms use `ctrl+`.
- **List keys** (singular, one value per call, repeatable): `context-path`,
  `global-context-path`, `skill-path`, `disable-skill`. Use `option reset <key>`
  to wipe inherited values (e.g. after `source`).

```bash
option progress false
option skill-path ./skills
option disable-skill sennit-config
option attribution-trailer-style assisted-by
option attribution-generated-with true
option ui compact true
option ui diff unified
option ui keybinding commands super+p
```

> [!IMPORTANT] `.sennit/skills` is loaded by default and does NOT need
> `skill-path`. Skills written for other tools (`.claude/skills`,
> `.opencode/skills`, ...) are **not** auto-discovered — bring them in with
> `sennit import claude --skills` / `sennit import opencode --skills`, which
> copies and validates them into `.sennit/skills` instead.

> [!IMPORTANT] Sennit only auto-loads its own context conventions
> (`AGENTS.md`/`SENNIT.md` and casing/`.local` variants). Other tools' files
> (`CLAUDE.md`, `.cursorrules`, `.github/copilot-instructions.md`, `GEMINI.md`,
> ...) are **not** read unless you opt in explicitly:
>
> ```bash
> option context-path CLAUDE.md
> option context-path .cursorrules
> ```

### options.web_search

The `web_search` tool's backend is configurable via `options.web_search` in
`sennit.json` (no sennitrc builtin yet — edit the JSON directly). Omitting the
section, or setting `provider` to `"duckduckgo"`, keeps the default: a keyless
scraper of DuckDuckGo Lite.

```json
{
  "options": {
    "web_search": {
      "provider": "tavily",
      "api_key": "$TAVILY_API_KEY",
      "base_url": "https://api.tavily.com/search",
      "proxy_url": "http://localhost:8080"
    }
  }
}
```

- **`provider`**: `"duckduckgo"` (default) or `"tavily"`.
- **`api_key`**: required for `tavily`; shell-expanded the same as provider
  `api_key` (`$VAR`, `${VAR:-default}`, `$(cmd)`). Unused by `duckduckgo`.
- **`base_url`**: optional override of the provider's default endpoint, for
  self-hosted or proxy-compatible search APIs.
- **`proxy_url`**: optional, routes search requests through a proxy
  (`http`/`https`/`socks5`); set to `"none"` to force a direct connection
  even when `HTTP_PROXY`/`HTTPS_PROXY` are set. Web search has no LLM
  provider of its own to inherit a proxy from, so it's configured here
  instead — unlike provider `proxy_url`, this only affects `web_search`, not
  `fetch`/`web_fetch`.
- An auth or quota error from the configured provider is returned to the
  model as a tool error (not a crashed step) so it can retry or fall back.

## Hooks runtime

Hooks are user-defined shell commands that fire on agent events. Currently only
`PreToolUse` is supported, which runs before a tool executes. This behavior is
the same however the hook is defined (`hook add` or JSON).

### How hooks work

1. When a tool is about to be called, all `PreToolUse` hooks with a matching
   `matcher` (or no matcher) run in parallel.
2. Duplicate commands are deduplicated — each unique command runs at most once.
3. The hook receives JSON on **stdin** and hook-specific **environment
   variables**.

Event names are case-insensitive and accept snake_case: `PreToolUse`,
`pretooluse`, `pre_tool_use`, `PRE_TOOL_USE` all work.

### Hook input (stdin)

```json
{
  "event": "PreToolUse",
  "session_id": "abc-123",
  "cwd": "/path/to/project",
  "tool_name": "bash",
  "tool_input": { "command": "ls -la" }
}
```

### Hook environment variables

| Variable                     | Description                                       |
| ---------------------------- | ------------------------------------------------- |
| `SENNIT_EVENT`                | Event name (e.g. `PreToolUse`)                    |
| `SENNIT_TOOL_NAME`            | Name of the tool being called                     |
| `SENNIT_SESSION_ID`           | Current session ID                                |
| `SENNIT_CWD`                  | Current working directory                         |
| `SENNIT_PROJECT_DIR`          | Project root directory                            |
| `SENNIT_TOOL_INPUT_COMMAND`   | Value of `command` from tool input (if present)   |
| `SENNIT_TOOL_INPUT_FILE_PATH` | Value of `file_path` from tool input (if present) |

### Hook output

**Exit code 0** — hook succeeded. Stdout is parsed as JSON:

```json
{ "decision": "allow", "context": "optional context appended to tool result" }
```

- `decision`: `allow` to explicitly allow, `deny` to block, `none` (or omit).
- `reason`: explanation (used when denying).
- `context`: extra context appended to the tool result.
- `updated_input`: replacement JSON for the tool input; last non-empty wins.

**Exit code 2** — the tool call is blocked; stderr is the deny reason.

**Any other exit code** — non-blocking error; the tool call proceeds.

### Decision aggregation

- **Deny wins over allow** — any deny blocks the call.
- **Allow wins over none** — a lone allow lets it proceed.
- Deny reasons and context strings are concatenated (newline-separated).
- For `updated_input`, the last non-empty value wins.

### Claude Code compatibility

Sennit also accepts the Claude Code hook output format, so existing hooks work
unchanged:

```json
{
  "hookSpecificOutput": {
    "permissionDecision": "allow",
    "permissionDecisionReason": "Auto-approved",
    "updatedInput": { "command": "echo rewritten" }
  }
}
```

## Subagents

Subagents are named roles the main agent can delegate to as a tool call.
Markdown files under `.sennit/agents/*.md` are the only way to define one.

> [!IMPORTANT] Before writing a `model:` field into an agent file, confirm
> the ID exists — `sennit_info {"models_for": "<provider>"}` or `sennit
> models <filter>`. Don't invent one from memory; an unresolvable `model:`
> is dropped with a warning and the subagent silently falls back to the
> main model, which is easy to miss. After writing/editing agent files, run
> `sennit doctor` — it flags exactly this (an agent pinned to a model that
> doesn't exist).

> [!WARNING] **NEVER** write agent definitions into `sennit.json` (or any
> JSON config) — a JSON `agents` block is silently ignored (flagged by
> `sennit doctor`). Create or edit `.sennit/agents/<name>.md` instead.

### Markdown files

Drop a file in `.sennit/agents/*.md` (also read: an `agents/` directory next
to the global config, lower priority). Files written for another tool
(`.claude/agents/`, `.opencode/agent/`) are **not** auto-discovered — run
`sennit import claude --agents` or `sennit import opencode --agents` to copy
and validate them into `.sennit/agents` first.

```markdown
---
name: reviewer
description: Reviews Go code for correctness and idiom.
model: anthropic/claude-sonnet-4   # optional; provider/model-id
reasoning_effort: low              # optional; low | medium | high
tools: [read, grep, glob]
---

You are a Go code reviewer. Report real defects, not style opinions.
```

- The file name (or `name`) becomes the delegation tool's name; the body is
  the system prompt (required, non-empty).
- `model` is optional and, when set, must resolve to a `provider/model-id`
  among configured providers; an unresolvable value is dropped with a
  warning and the agent falls back to the app's main model.
- `.sennit/agents` files are expected to already name Sennit's own tools
  (`read`, `grep`, `bash`, ...) — regular discovery does not translate
  Claude Code names anymore. `sennit import` does that translation once, at
  import time, and reports any tool name it couldn't map.
- opencode's `permission:` blocks are **not enforced**, imported or not —
  restrict an agent via `tools` or the config's `permissions` section
  instead.

## Importing from Claude Code or opencode

```sh
sennit import claude|opencode [--skills] [--agents] [--dry-run] [--global] [--force]
```

Sennit does not auto-discover another tool's config directories (see
[Limitations of imported agent definitions](../../../../TECHDEBT.md) in
TECHDEBT.md for why). `sennit import` is the supported way to bring files in:

- `--skills` copies `<tool>/skills/<name>/SKILL.md` (and any other files in
  that skill's directory) into `.sennit/skills/<name>/`, after parsing and
  validating it against the same Agent Skills spec Sennit's own skills follow.
  A skill that fails validation (bad name, oversized description, ...) is
  skipped with a reason, not partially imported.
- `--agents` copies `<tool>/agents/*.md` (or opencode's `.opencode/agent/`)
  into `.sennit/agents/*.md`, translating:
  - `model` — resolved against your configured providers the same way a
    hand-written `model:` is; an unresolvable value is dropped with a
    warning and left as a `# original model: ... — not available` comment in
    the written frontmatter, instead of silently vanishing.
  - `reasoning_effort` (or opencode's `effort`) — mapped onto
    `low`/`medium`/`high`; a value like `max` is mapped to the closest one
    (`high`) with a warning, and anything unrecognized is dropped with a
    warning.
  - `tools` — Claude Code names (`Read`, `Grep`, `Bash`, ...) are translated
    to Sennit's; a name that maps to neither a known Claude Code name nor a
    Sennit tool is dropped and reported, not kept as-is.
  - `temperature`/`top_p` — Sennit agents have no such field; the original
    value is kept as a frontmatter comment and reported as a warning.
  - opencode's `permission:` block — dropped with a warning; it was never
    enforced even when opencode's directory was auto-discovered. Restrict an
    imported agent via its `tools` list instead.
- `--global` reads/writes the user-level directories (`~/.claude/...`,
  `~/.config/opencode/...` → the global `.sennit` directories) instead of the
  project ones.
- `--dry-run` prints the report without writing anything.
- Without `--force`, a destination file that already exists is left alone
  and reported as skipped — re-running an import is safe.

The command prints one row per skill/agent: name, `imported` / `adjusted` /
`skipped`, and the reason or warnings behind that status.

## User-invocable skills

Skills can be invoked as commands. Add `user-invocable: true` to the skill's
YAML frontmatter:

```yaml
---
name: my-skill
description: A skill that can be invoked as a command.
user-invocable: true
---
```

- Global skills appear as `user:skill-name`; project skills as
  `project:skill-name`.
- Add `disable-model-invocation: true` to keep a skill user-only (hidden from
  the model's available-skills list but still manually invocable).

## Maintenance

The shared database (`~/.config/sennit/sennit.db`) is not pruned
automatically — `sennit gc` is a CLI command a human (or a cron job) runs, not
something the agent does for itself, and there is no `/gc` slash command.

```sh
sennit gc [--days N] [--dry-run] [--project] [--json]
```

- Deletes sessions (and their messages/files/read-file records) whose last
  activity (`updated_at`) is older than `options.history_retention_days`
  (default 90; set via `option history-retention-days N`). `0` disables
  retention entirely and turns `sennit gc` into a no-op.
- Deleting a session also deletes any agent-tool/title sub-session parented
  to it, regardless of the sub-session's own age; old sub-sessions under a
  kept parent are deleted independently, on their own age.
- Also deletes finished threads (`completed`, `merged`, `conflict`,
  `merge_blocked`, `failed`, `interrupted`) past the same window —
  `pending`/`running`/`merging` threads are never touched, regardless of age.
- Runs `VACUUM` and a WAL checkpoint afterward to actually shrink
  `sennit.db` on disk.
- Defaults to the entire shared database (every project); pass `--project`
  to scope to the current working directory's project only.
- `--days N` overrides `options.history_retention_days` for one run;
  `--dry-run` reports counts and current database size without deleting
  anything.
- Rotated log files (`~/.config/sennit/logs/*.log.gz`) are unrelated: they
  are pruned by lumberjack's own `MaxAge` (30 days) independently of
  `sennit gc`.

## Environment variables

- `SENNIT_VERSION` — exported into `sennitrc` at load; the running version (or
  `devel` for local builds).
- `SENNIT_GLOBAL_CONFIG` — override global config location.
- `SENNIT_GLOBAL_DATA` — override data directory location.
- `SENNIT_SKILLS_DIR` — override default skills directory.

## Legacy JSON format

`sennit.json` is the original static format. It still works and merges with
`sennitrc`. Basic structure:

```json
{
  "$schema": "https://charm.land/sennit.json",
  "model": {},
  "providers": {},
  "mcp": {},
  "lsp": {},
  "hooks": {},
  "options": {},
  "permissions": {}
}
```

The `$schema` property enables IDE autocomplete but is optional.

### sennitrc ↔ sennit.json mapping

| sennitrc                             | sennit.json                                             |
| ------------------------------------ | ------------------------------------------------------ |
| `provider add openai --api-key "$K"` | `providers.openai = {"api_key": "$K"}`                 |
| `model add openai/gpt-x --name X`    | append to `providers.openai.models[]`                  |
| `model openai/gpt-x`                 | `model = {"provider":"openai","model":"gpt-x"}`        |
| `mcp add gh --type http --url U`     | `mcp.gh = {"type":"http","url":"U"}`                   |
| `lsp add go --command gopls`         | `lsp.go = {"command":"gopls"}`                         |
| `hook add PreToolUse --command C`    | append to `hooks.PreToolUse[]`                         |
| `permissions allow read ls`          | `permissions.allowed_tools = ["read","ls"]`            |
| `permissions deny bash`              | `options.disabled_tools = ["bash"]`                    |
| `permissions bypass on`              | `permissions.bypass = true`                            |
| `option skill-path ./skills`         | `options.skills_paths = ["./skills"]`                  |
| `option history-retention-days 30`   | `options.history_retention_days = 30`                  |
| *(no sennitrc equivalent)*            | `providers.<id>.proxy_url = "http://host:8080"`        |
| `option metrics false`               | `options.disable_metrics = true`                       |
| `option attribution-trailer-style none` | `options.attribution.trailer_style = "none"`        |
| `option attribution-generated-with false` | `options.attribution.generated_with = false`       |
| *(no sennitrc equivalent)*            | `options.web_search = {"provider":"tavily","api_key":"$K"}` |

### Shell expansion in sennit.json

In JSON, only selected string fields are run through the embedded shell at load
time (in `sennitrc`, everything is native Bash so this table does not apply):

| Surface                                                         | Expansion                          |
| --------------------------------------------------------------- | ---------------------------------- |
| Provider `api_key`, `base_url`, `api_endpoint`, `proxy_url`, `extra_headers` | yes                    |
| `options.web_search` `api_key`, `proxy_url`                     | yes                                |
| Provider `extra_body`                                           | **no** (JSON passthrough)          |
| MCP `command`, `args`, `env`, `headers`, `url`                  | yes                                |
| LSP `command`, `args`, `env`                                    | yes                                |
| Hook `command`                                                  | runs via `sh -c`, not the resolver |

Supported constructs: `$VAR`, `${VAR}`, `${VAR:-default}`, `${VAR:+alt}`,
`${VAR:?message}`, `$(command)`. An unset variable expands to empty; a failing
`$(command)` is a hard error. A header that resolves to empty is dropped from
the request.

### Security note

Both formats are trusted code. `sennitrc` runs entirely, and any `$(...)` in
`sennit.json` runs at load time, with the invoking user's shell privileges,
before the UI appears. Don't launch Sennit in a directory whose config you
haven't reviewed.
