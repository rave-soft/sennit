# Braid

A terminal-first AI coding agent with real multi-agent orchestration.

Braid runs specialised agents — a reviewer, a DBA, a security auditor, an
implementer — as parallel delegations inside one process, each with its own
prompt, tools, model slot and reasoning effort. Agents are plain markdown
files, so a role is something you write and read, not a JSON blob.

## Agents

Drop a markdown file into `.braid/agents/`:

```markdown
---
name: reviewer
description: Reviews Go code for correctness and idiom.
model: anthropic/claude-sonnet-4   # optional; provider/model-id. Omit to use the app's main model.
reasoning_effort: low     # low | medium | high
tools: [view, grep, glob]
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

Braid also reads agents from `.claude/agents/` and `.opencode/agent/`, and from
an `agents/` directory next to the global config. Tool names from Claude Code
(`Read`, `Grep`, `Bash`, …) are translated automatically, so existing role
files work without being rewritten. Entries under `agents` in the JSON config
still work and take precedence over files of the same name. Those foreign
files' `model:` values are honored the same way — a `provider/model-id`
string that resolves against your configured providers is used; a tool-name
like `opus` that isn't one of your models still falls back to the main model.

Fields Braid does not understand are ignored rather than rejected — including
opencode's `permission:` blocks, which are **not** enforced. Restrict an agent
through its `tools` list or the `permissions` section of the config instead.

## Skills

Agent Skills are discovered from `.braid/skills/`, `.agents/skills/`,
`.claude/skills/`, `.cursor/skills/` and `.opencode/skills/`, plus the usual
global locations.

## Configuration

Config lives in `braid.json` (project or global) and follows `schema.json`.
The canonical project location is `.braid/braid.json` — it has the highest
priority of any project config and is where `braid config set` and
agent-driven writes land; a bare `braid.json`/`.braid.json` at the project
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
starts skip the network round trip. Run `braid models refresh [provider-id]`
to force a re-discovery on demand (all custom providers if no ID is given);
explicit `models` entries in a project or global `braid.json` always take
precedence over the persisted list.

## Building

```bash
go build -o braid .
```

## Origin and license

Braid is a fork of [Crush](https://github.com/charmbracelet/crush) by
Charmbracelet, Inc. Nearly all of the foundation — the TUI, the provider layer,
the tool implementations, LSP and MCP support — is their work.

Braid is distributed under the same license as the upstream project, the
Functional Source License 1.1 with MIT Future License
([FSL-1.1-MIT](LICENSE.md)). It permits use, modification and redistribution
for any purpose other than a Competing Use, and each version converts to MIT
two years after its upstream release.

See [NOTICE](NOTICE) for attribution and trademark details. Braid is not
affiliated with or endorsed by Charmbracelet, Inc.
