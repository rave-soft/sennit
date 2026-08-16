# Sennit

A terminal-first AI coding agent with real multi-agent orchestration.

Sennit runs specialised agents — a reviewer, a DBA, a security auditor, an
implementer — as parallel delegations inside one process, each with its own
prompt, tools, model slot and reasoning effort. Agents are plain markdown
files, so a role is something you write and read, not a JSON blob.

## Documentation

- **[Config](config/README.md)** — the `sennitrc` command reference: providers,
  models, permissions, MCP servers, hooks and the deprecated JSON format.
- **[Hooks](hooks/README.md)** — the `PreToolUse` payload format, exit codes,
  matchers and the execution model.
- **[Steering, tasks and threads](delegation/README.md)** — the three ways work
  happens alongside a running turn, and what each one costs.

Future-work notes live next to each area
([config](config/FUTURE.md), [hooks](hooks/FUTURE.md)); they describe what is
planned, not what ships today.

## Getting started

Agents are markdown files under `.sennit/agents/`:

```markdown
---
name: reviewer
description: Reviews Go code for correctness and idiom.
model: anthropic/claude-sonnet-4   # optional; provider/model-id
reasoning_effort: low     # low | medium | high
tools: [read, grep, glob]
---

You are a Go code reviewer. Report real defects, not style opinions.
```

Configuration is Bash, evaluated at startup like a `.bashrc`:

```bash
provider add ollama --type ollama --base-url "http://localhost:11434/v1"
model add ollama/llama3.3 --name "Llama 3.3" --context-window 128000
permissions allow read edit
```

The [repository README](https://github.com/rave-soft/sennit#readme) covers
installation, the command line and building from source.

## License

Sennit is released under [FSL-1.1-MIT](https://github.com/rave-soft/sennit/blob/main/LICENSE.md).
See [NOTICE](https://github.com/rave-soft/sennit/blob/main/NOTICE) for
attribution and trademark details.
