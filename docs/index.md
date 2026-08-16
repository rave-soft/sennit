# Sennit

A terminal-first AI coding agent with real multi-agent orchestration. Roles,
skills and hooks are plain files you can read; configuration is Bash.

[Get started](getting-started.md)
[View on GitHub](https://github.com/rave-soft/sennit)

---

## What it is

Sennit runs specialised agents — a reviewer, a DBA, a security auditor, an
implementer — as parallel delegations inside one process, each with its own
prompt, tools, model slot and reasoning effort. Nothing about a role is hidden
in a binary or a JSON blob: an agent is a markdown file with front matter, a
skill is a `SKILL.md`, a hook is a shell command, and the config is a script
you can read top to bottom.

```sh
sennit                                  # open the TUI in this project
sennit run "explain internal/agent"     # one non-interactive prompt
```

## Start here

| If you want to… | Read |
|:--|:--|
| install it and get a first session running | [Getting started](getting-started.md) |
| point it at your own models or a local server | [Providers and models](configuration/providers.md) |
| write a specialised role it can delegate to | [Agents](extending/agents.md) |
| package a repeatable procedure it can load on demand | [Skills](extending/skills.md) |
| gate or rewrite what it is allowed to run | [Hooks](extending/hooks.md) and [Permissions](configuration/permissions.md) |
| understand when work runs in parallel, and where | [Steering, tasks and threads](concepts/delegation.md) |
| look up a flag, a slash command, or a tool | [Reference](reference/index.md) |

## The shape of the thing

Four ideas carry most of Sennit, and each has its own page.

**Configuration is Bash.** A `sennitrc` runs at startup and a set of builtin
commands (`provider`, `model`, `mcp`, `lsp`, `hook`, `permissions`, `option`)
build the config as the script executes. A shared team config is a `source`, a
secret is `$(op read …)`, and a machine-specific setting is an `if`. See
[Configuration](configuration/index.md).

**Agents are files.** Drop `reviewer.md` into `.sennit/agents/` and the main
agent gains a `reviewer` tool it can delegate to, with the body of the file as
its system prompt. See [Agents](extending/agents.md).

**Work can run beside the current turn — three different ways.** A message
sent mid-turn is *steered* into that turn; a *background task* is a cheap
delegation sharing your working directory; a *thread* gets its own git branch
and worktree. They trade cost against isolation very differently. See
[Steering, tasks and threads](concepts/delegation.md).

**History is one database.** Sessions, messages and file history for every
project live in a single SQLite database under your config directory, tagged by
project path — not one database per project. See
[Sessions and data storage](concepts/sessions.md).

## Origin and license

Sennit is a fork of [Crush](https://github.com/charmbracelet/crush) by
Charmbracelet, Inc. Nearly all of the foundation — the TUI, the provider layer,
the tool implementations, LSP and MCP support — is their work.

It is distributed under the same license as the upstream project, the
Functional Source License 1.1 with MIT Future License (FSL-1.1-MIT), which
permits use, modification and redistribution for any purpose other than a
Competing Use; each version converts to MIT two years after its upstream
release. See [`NOTICE`](https://github.com/rave-soft/sennit/blob/main/NOTICE)
for attribution and trademark details.

Sennit is not affiliated with or endorsed by Charmbracelet, Inc.
