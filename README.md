# Sennit

A terminal-first AI coding agent with real multi-agent orchestration.

Documentation: **<https://rave-soft.github.io/sennit/>**

Sennit runs specialised agents — a reviewer, a DBA, a security auditor, an
implementer — as parallel delegations inside one process, each with its own
prompt, tools, model slot and reasoning effort. Agents are plain markdown
files, so a role is something you write and read, not a JSON blob.

## Agents

Drop a markdown file into `.sennit/agents/`:

```markdown
---
name: reviewer
description: Reviews Go code for correctness and idiom.
model: anthropic/claude-sonnet-4   # optional; provider/model-id. Omit to use the app's main model.
reasoning_effort: low     # low | medium | high
tools: [read, grep, glob]
---

You are a Go code reviewer. Report real defects, not style opinions.
```

The file name (or the `name` field) becomes the tool the main agent calls to
delegate. The body is the system prompt.

`model` is optional. When set it must be a `provider/model-id` (e.g.
`anthropic/claude-sonnet-4`) to pin an agent to a model of its own; a bare
model id also works if it's unambiguous across your configured providers.
Omitting it makes the agent use the app's main model. A value that doesn't
resolve to a configured provider/model is ignored with a warning and the
agent falls back to the main model. `reasoning_effort` overrides the
model's effort for this agent alone, and applies the same way regardless of
whether `model` is set or omitted; an effort the model doesn't offer is
ignored at call time in favor of the model's own default.

Sennit only auto-discovers `.sennit/agents/` and an `agents/` directory next to
the global config — not `.claude/agents/` or `.opencode/agent/`. Those are a
different tool's files; trusting them implicitly means trusting whatever a
project happened to drop there. Bring them in explicitly instead:

```sh
sennit import claude --agents      # or: sennit import opencode --agents
```

This copies each file into `.sennit/agents/`, translating what it can (tool
names, `model:`, `reasoning_effort:`) and reporting what it can't (see
`sennit import --help`). Markdown files are the only source of user-defined
agents: an `agents` block in a JSON config is ignored, and Sennit reports it
as a config problem telling you to move each entry to
`.sennit/agents/<name>.md`.

Fields Sennit does not understand are ignored rather than rejected — including
opencode's `permission:` blocks, which are **not** enforced. Restrict an agent
through its `tools` list or Sennit's own `permissions` commands instead.

## Skills

Agent Skills are discovered from `.sennit/skills/` and the usual global
`sennit` locations only. As with agents, a skill written for Claude Code or
opencode (`.claude/skills/`, `.opencode/skills/`, ...) needs an explicit
import rather than being auto-discovered:

```sh
sennit import claude --skills      # or: sennit import opencode --skills
```

See `sennit import --help` for `--dry-run`, `--global`, and `--force`. Extra
skill directories can be added explicitly with `option skill-path <dir>`, and
an unwanted skill hidden with `option disable-skill <name>`.

## Configuration

Config is Bash. Sennit runs a `sennitrc` at startup — globally from
`~/.config/sennit/sennitrc`, per project from `.sennit/sennitrc`,
`.sennitrc` or `sennitrc` — and a set of builtin commands (`provider`,
`model`, `mcp`, `lsp`, `hook`, `permissions`, `option`) build the config as
the script runs. Any OpenAI-compatible endpoint works as a provider,
including a local `llama.cpp`, `ollama` or `lmstudio` server:

```bash
# .sennit/sennitrc
provider add local \
  --type openai-compat \
  --base-url "http://127.0.0.1:8080/v1" \
  --api-key "not-needed"

permissions allow read grep
option skill-path ./skills
```

Because it's a shell script, a shared team config is a `source`, secrets can
come from your password manager, and machine-specific settings are just an
`if`. The full command reference is in [docs/config](docs/config/README.md).

`sennit.json` (and `.sennit.json`, in the same locations) still works but is
deprecated: it follows [`schema.json`](schema.json) and receives no new
features. Everything found is merged, project settings override global ones,
and in one directory `sennitrc` overrides the JSON — with a warning when both
are present.

Providers and the selected model are the exception: they are read only from the
global config. A `provider`/`model` command or a `providers`/`model` block in a
project config is ignored (and reported by `sennit doctor`), so cloning a repo
can't repoint your session at someone else's endpoint.

A custom provider with no `models` list has its model catalog auto-discovered
from `/models` on first load, then cached in the data directory so later
starts skip the network round trip. Run `sennit models refresh [provider-id]`
to force a re-discovery on demand (all custom providers if no ID is given);
models you register by hand always take precedence over the cached list.

## Hooks

Hooks are shell commands Sennit runs at points in the agent lifecycle — today
`PreToolUse`. A hook can block a tool call, rewrite its input, inject context,
or auto-approve a tool without a prompt, and it can be written in any
language. They are configured with `hook add` in `sennitrc` and are
Claude Code-compatible.

```bash
hook add PreToolUse --matcher "^bash$" \
  --command "./hooks/no-force-push.sh" --name no-force-push
```

See [docs/hooks](docs/hooks/README.md) for the payload format, exit codes and
worked examples.

## Steering, tasks and threads

Three ways work happens alongside the current turn: a message sent
mid-turn is **steered** into that turn rather than starting a new one; a
**background task** is a delegation with no isolation, sharing the working
directory and reporting back automatically; a **thread** is fully isolated —
its own git worktree, branch, app instance and merge policy — for work that
would otherwise collide with what's already running. `sennit threads` manages
the last of these from the CLI. See
[docs/delegation](docs/delegation/README.md) for the trade-offs and limits.

## Command line

Sennit with no arguments opens the TUI; `--session`/`--continue` resume an
earlier one and `--cwd` picks the project.

```sh
sennit run "explain internal/agent"   # single non-interactive prompt (pipeable)
sennit models [refresh]               # list models; re-discover custom providers
sennit session list|show|last         # browse sessions
sennit threads list|create|merge      # manage work threads
sennit stat                           # usage statistics
sennit doctor                         # check the loaded config for problems
sennit dirs / projects / logs         # where things live, and what's in them
sennit gc                             # purge old history, reclaim database space
sennit login|logout [platform]        # provider credentials
sennit import claude|opencode         # bring in another tool's agents/skills
```

`--yolo` auto-accepts every permission prompt, and `--data-dir` points the
project's state elsewhere.

## Data Storage

Sessions, messages, and history for every project are kept in a single
SQLite database at `~/.config/sennit/sennit.db` (or `$SENNIT_GLOBAL_CONFIG`'s
directory, when set), with each row tagged by the project's absolute path —
there is no more one database per project. Logs are similarly unified at
`~/.config/sennit/logs/sennit.log` (`sennit dirs` prints both, `sennit logs`
tails the latter, `sennit gc` prunes old history). A project's own `.sennit/`
directory holds only what you author — `sennitrc`, `agents/`, `skills/` — plus
a single-instance lock file. Nothing
imports a per-project database from before that move: Sennit ships no
compatibility layer, so any such file is simply ignored and can be deleted.

## Building

```bash
go build -o sennit .
```

The repo also ships a [Taskfile](Taskfile.yaml): `task build`, `task run`,
`task test`, `task lint`, `task fmt`, `task install`. `task schema`
regenerates `schema.json` and `task sqlc` the database layer, so both are
rebuilt from source rather than edited by hand.

## Origin and license

Sennit is a fork of [Crush](https://github.com/charmbracelet/crush) by
Charmbracelet, Inc. Nearly all of the foundation — the TUI, the provider layer,
the tool implementations, LSP and MCP support — is their work.

Sennit is distributed under the same license as the upstream project, the
Functional Source License 1.1 with MIT Future License
([FSL-1.1-MIT](LICENSE.md)). It permits use, modification and redistribution
for any purpose other than a Competing Use, and each version converts to MIT
two years after its upstream release.

See [NOTICE](NOTICE) for attribution and trademark details. Sennit is not
affiliated with or endorsed by Charmbracelet, Inc.
