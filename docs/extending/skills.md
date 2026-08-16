# Skills

A skill is a folder with a `SKILL.md` in it: a procedure the agent loads when
it becomes relevant. Sennit implements the [Agent Skills][spec] open standard,
so skills written against the spec work as-is.

[spec]: https://agentskills.io

```
.sennit/skills/
  release-notes/
    SKILL.md
    template.md
    changelog.py
```

```markdown
---
name: release-notes
description: >-
  Draft release notes from the commit range since the last tag. Use when
  asked to prepare a release, write a changelog, or summarise what changed
  since a version.
---

# Release notes

1. `git describe --tags --abbrev=0` for the previous tag.
2. `git log <tag>..HEAD --no-merges` for the commits.
3. Group by conventional-commit type; drop chore and ci entries.
4. Fill in `template.md`, one line per user-visible change.
```

## Why this instead of a context file

Only the `name` and `description` of every skill sit in context. The body is
loaded when the agent decides the skill applies. So a repository can carry
twenty detailed procedures without paying for twenty procedures on every turn —
the way a
[context file](../configuration/context.md) would.

That makes the description the load-bearing field. It is the only thing the
agent sees when deciding.

## Where they are discovered

| Directory | Scope |
|:--|:--|
| `<project>/.sennit/skills/` | this project |
| `<git worktree root>/.sennit/skills/` | the whole repo, when you are in a subdirectory |
| `~/.config/sennit/skills/` | every project |

Working-directory paths come first, so a local skill takes precedence over a
monorepo-level one of the same name.

Add more:

```bash
option skill-path ./skills
option skill-path ~/work/shared-skills
```

> [!IMPORTANT]
> Skills written for another tool (`.claude/skills`, `.opencode/skills`,
> `.cursor/skills`, …) are **not** auto-discovered. Import them —
> `sennit import claude --skills` — which copies and validates them into
> `.sennit/skills`, or point at the directory with `skill-path`.

Sennit also ships builtin skills: `sennit-config` (it can configure itself),
`sennit-hooks`, `threads`, `tasks` and `jq`. Hide one you don't want:

```bash
option disable-skill jq
```

## Front matter

| Field | Required | Meaning |
|:--|:--|:--|
| `name` | yes | alphanumeric and hyphens, ≤64 chars, and **must match the directory name** |
| `description` | yes | ≤1024 chars; what the agent reads to decide relevance |
| `user-invocable` | no | also expose it as a `/` command in the TUI |
| `disable-model-invocation` | no | hide it from the agent; only you can invoke it |
| `license` | no | free text |
| `compatibility` | no | ≤500 chars |
| `metadata` | no | a string-to-string map, for your own use |

The name-must-match-directory rule is a common first stumble; a mismatch makes
the skill fail validation and it simply won't appear.

Setting both `user-invocable: true` and `disable-model-invocation: true` gives
you a skill that behaves like a
[custom command](commands.md) — yours to run, never chosen by the model.

## Bundling files

Everything beside `SKILL.md` is part of the skill: templates, scripts,
reference data, schemas. Refer to them by relative path from the skill body and
the agent can read them. Files inside a discovered skill directory can be read
without a permission prompt.

This is what makes a skill more than a prompt snippet — a script the agent runs
rather than reimplements is both cheaper and more reliable than instructions
describing the same work.

## Writing a good description

The description is a routing decision, so write it for the router:

- **Name the triggers.** "Use when the user asks about deployments, rollbacks,
  or a failed release." Concrete nouns beat a summary of the body.
- **Say what it produces**, so the agent can tell whether it wants that.
- **Don't describe the steps** — that's the body's job, and the body is free.

Keep the body scannable and imperative. Numbered steps, exact commands, the
real file paths. A skill that says "consider validating the input" does nothing;
one that says "run `task lint` and fix every finding before proceeding" does.

## Live reload

Sennit watches skill directories. Adding, editing or deleting a `SKILL.md` is
picked up without restarting — useful while you are iterating on one.

A skill that fails validation is reported rather than silently dropped; the
TUI's `/doctor` and the `sennit_info` tool show discovery state.

## Related

- [Agents](agents.md) — when the work wants its own context window.
- [Commands](commands.md) — when *you* want the shortcut.
- [Hooks](hooks.md) — when it should happen automatically rather than by
  choice.
