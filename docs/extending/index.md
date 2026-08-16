# Extending Sennit

Five different ways to add capability, and they are not interchangeable.
Picking the wrong one is the most common source of "why doesn't it use this?"

| Mechanism | It is | Use when |
|:--|:--|:--|
| [Agents](agents.md) | a markdown file that becomes a delegation tool | you want a *role* with its own prompt, model and tool set |
| [Skills](skills.md) | a `SKILL.md` loaded on demand | you have a *procedure* the agent should follow when a situation arises |
| [Commands](commands.md) | a markdown prompt template you invoke with `/` | *you* want a shortcut for a prompt you retype |
| [Hooks](hooks.md) | a shell command run on tool events | you need to *gate, rewrite or observe* what the agent does |
| [MCP](mcp.md) / [LSP](lsp.md) | external servers | you need *new tools* or real language intelligence |

## The distinction that actually matters

**Agent vs. skill.** An agent is *who* does the work: a separate session with
its own prompt and its own context window, delegated to and reported back from.
A skill is *what to do*: instructions loaded into the current agent's context
when relevant. If the answer to "should this have its own context window?" is
yes, it's an agent. If it's "the agent just needs to know the steps", it's a
skill.

**Skill vs. context file.** Both are instructions, but a
[context file](../configuration/context.md) is loaded into
*every* turn and paid for every time. A skill contributes only its one-line
description until it is actually needed. Anything conditional belongs in a
skill.

**Command vs. skill.** A command is invoked by you, a skill is chosen by the
model. A skill can be marked `user-invocable` to be both.

## Importing from other tools

Sennit auto-discovers only its own directories — `.sennit/agents`,
`.sennit/skills`. A file written for Claude Code or opencode needs to be
brought in explicitly:

```sh
sennit import claude --agents --skills
sennit import opencode --skills --dry-run
```

The import copies into Sennit's directories, translating what it can (tool
names, `model:`, `reasoning_effort:`) and reporting what it can't. This is
deliberate: silently trusting whatever a repo dropped in `.claude/` means
running instructions nobody wrote for Sennit.
