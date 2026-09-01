# Agents

An agent is a markdown file. Drop it into `.sennit/agents/` and the main agent
can delegate to it by name — `agent` with `subagent_type: <name>` — and the
body of the file becomes that agent's system prompt. The `agent` tool's own
description carries the roster, so a newly added file is offered without a
restart.

```markdown
---
name: reviewer
description: Reviews Go code for correctness and idiom.
model: anthropic/claude-sonnet-4   # optional
reasoning_effort: low              # low | medium | high
tools: [read, grep, glob, ls]
---

You are a Go code reviewer. Report real defects, not style opinions.
Every finding must name a file and line and describe a concrete failure —
inputs or state that produce a wrong result. If you cannot describe how it
fails, it is not a finding.
```

That is the whole mechanism. A role is something you write and read, not a
JSON blob.

## Where they are discovered

| Directory | Scope |
|:--|:--|
| `<project>/.sennit/agents/*.md` | this project |
| `~/.config/sennit/agents/*.md` | every project |

The user-level directory has the last word, matching how the rest of the config
treats global settings: a global `reviewer.md` overrides a project one of the
same name.

Other tools' directories — `.claude/agents`, `.opencode/agent` — are **not**
scanned. Bring them in explicitly:

```sh
sennit import claude --agents            # or: --global, --dry-run, --force
```

## Front matter

| Field | Meaning |
|:--|:--|
| `name` | the `subagent_type` the main agent calls. Defaults to the filename |
| `description` | what the main agent reads to decide whether to delegate — it is what the `agent` tool lists this agent as. This is the single most important field |
| `model` | `provider/model-id` to pin this agent to its own model. Omit to inherit the app's main model |
| `reasoning_effort` | `low`, `medium` or `high`, overriding the model's effort for this agent alone |
| `tools` | restricts the agent to these tools |
| `disabled` | `true` to keep the file but stop offering the agent |

### `model`

A `provider/model-id` (`anthropic/claude-sonnet-4`). A bare model id works too
if it is unambiguous across your configured providers. A value that doesn't
resolve is ignored with a warning and the agent falls back to the main model —
`sennit doctor` reports it.

Omitting `model` is usually right. Pin one when the role genuinely wants a
different tier: a cheap fast model for mechanical checks, an expensive one for
architecture.

### `reasoning_effort`

Applies the same way whether `model` is set or omitted. An effort the model
doesn't offer is ignored at call time in favour of the model's own default.

### `tools`

Three syntaxes are accepted, so imported files keep working:

```yaml
tools: [read, grep, glob]        # YAML list
tools: read, grep, glob          # comma-separated (Claude Code style)
tools: {read: true, bash: false} # enabled map (opencode style)
```

Names come from the [tools reference](../reference/tools.md).
Omit the field to give the agent the default set.

> [!IMPORTANT]
> Fields Sennit does not understand are ignored rather than rejected —
> including opencode's `permission:` blocks, which are **not** enforced.
> Restrict an agent through its `tools` list or the `permissions` section of
> the config instead.

## Delegation and memory

A named agent is a continuing counterpart, not a stranger on every call.
Delegating to `reviewer` twice under the same session replays the first
exchange into the second: you can send it review findings and it knows what it
wrote.

Continuity is scoped by *who* and *where*. Two named agents under one parent
keep separate conversations, and the same agent keeps separate conversations
under different parents — which is what keeps a thread's delegations inside
that thread.

Each delegation still gets its own session, so each call is its own block in
the transcript. The carried memory is bounded: once the replayed transcript
grows past its budget, the oldest whole delegations are shed and the newest are
kept.

The anonymous delegations — `agent` with no `subagent_type`, and
`agentic_fetch` — stay stateless on purpose. They are one-off, often several at
a time on unrelated work, and stitching them into one growing conversation
would cost context without buying continuity.

See [Steering, tasks and threads](../concepts/delegation.md)
for how delegated work relates to background tasks and threads.

## Writing one that gets used

The `description` is what the main agent sees when deciding whether to
delegate. A vague description means the agent is never called, or called for
the wrong thing.

- **Say when, not just what.** "Reviews Go code" is weaker than "Use after
  writing or changing Go code, before committing. Reviews for correctness bugs
  and idiom."
- **Give the body a stopping condition.** An agent with no notion of "done"
  either stops too early or grinds. Say what a finished result looks like.
- **Restrict tools to what the role needs.** A reviewer with `edit` will
  eventually edit. Removing the tool is more reliable than instructing it not
  to.
- **Set the output contract.** The agent's final message is what the main agent
  receives — nothing else. State the shape you want it in.

## A worked set

```
.sennit/agents/
  reviewer.md      read-only; correctness and idiom; returns findings
  dba.md           read-only + bash; schema and query review
  security.md      read-only; auth, injection, secrets handling
  implementer.md   full tools; makes a described change and reports the diff
```

Running them as parallel delegations inside one process — each with its own
prompt, tools, model slot and effort — is what "real multi-agent
orchestration" means here.

## Related

- [Permissions](../configuration/permissions.md) — how
  `tools:` interacts with global permissions.
- [Skills](skills.md) — for procedures rather than roles.
- [`sennit stat`](../reference/cli.md) — per-agent token and
  time breakdown.
