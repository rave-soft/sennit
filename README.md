# Sennit

A terminal-first AI coding agent with real multi-agent orchestration.

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
`sennit import --help`). Entries under `agents` in the JSON config still work
and take precedence over files of the same name.

Fields Sennit does not understand are ignored rather than rejected — including
opencode's `permission:` blocks, which are **not** enforced. Restrict an agent
through its `tools` list or the `permissions` section of the config instead.

## Skills

Agent Skills are discovered from `.sennit/skills/` and the usual global
`sennit` locations only. As with agents, a skill written for Claude Code or
opencode (`.claude/skills/`, `.opencode/skills/`, ...) needs an explicit
import rather than being auto-discovered:

```sh
sennit import claude --skills      # or: sennit import opencode --skills
```

See `sennit import --help` for `--dry-run`, `--global`, and `--force`.

## Configuration

Config lives in `sennit.json` (project or global) and follows `schema.json`.
The canonical project location is `.sennit/sennit.json` — it has the highest
priority of any project config and is where `sennit config set` and
agent-driven writes land; a bare `sennit.json`/`.sennit.json` at the project
root is still supported at lower priority.
Any OpenAI-compatible endpoint works as a provider, including a local
`llama.cpp`, `ollama` or `lmstudio` server:

```json
{
  "providers": {
    "local": {
      "type": "llamacpp",
      "base_url": "http://127.0.0.1:8080/v1",
      "api_key": "not-needed"
    }
  }
}
```

A custom provider with no `models` list has its model catalog auto-discovered
from `/models` on first load, then persisted into the data-dir config so later
starts skip the network round trip. Run `sennit models refresh [provider-id]`
to force a re-discovery on demand (all custom providers if no ID is given);
explicit `models` entries in a project or global `sennit.json` always take
precedence over the persisted list.

## Data Storage

Sessions, messages, and history for every project are kept in a single
SQLite database at `~/.config/sennit/sennit.db` (or `$SENNIT_GLOBAL_CONFIG`'s
directory, when set), with each row tagged by the project's absolute path —
there is no more one database per project. Logs are similarly unified at
`~/.config/sennit/logs/sennit.log`. A project's own `.sennit/` directory now
only holds its config overrides and a single-instance lock file. Nothing
imports a per-project database from before that move: Sennit ships no
compatibility layer, so any such file is simply ignored and can be deleted.

## Building

```bash
go build -o sennit .
```

## Upgrading from Braid

Sennit is Braid, renamed. There is no compatibility layer — see
[docs/MIGRATION.md](docs/MIGRATION.md) for the full old-to-new contract table
and the manual steps to carry over an existing profile.

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
