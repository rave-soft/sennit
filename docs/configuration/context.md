# Context files

A context file is a plain markdown file loaded into every session for a
project: build commands, layout, conventions, the things you would otherwise
retype at the start of every conversation. It is the cheapest way to make the
agent behave like it has worked on this codebase before.

## What is loaded automatically

Sennit reads only its own conventions, without any opt-in. In the project
directory:

```
sennit.md          sennit.local.md
Sennit.md          Sennit.local.md
SENNIT.md          SENNIT.local.md
AGENTS.md          agents.md          Agents.md
```

The `*.local.md` variants exist for personal notes you don't want committed —
add them to `.gitignore` and keep the shared file clean.

Globally, two more are read:

```
~/.config/sennit/SENNIT.md
~/.config/AGENTS.md
```

Files belonging to other tools — `CLAUDE.md`, `.cursorrules`,
`.github/copilot-instructions.md` — are **not** loaded automatically. That is
deliberate: reading a file another tool dropped into the repo means trusting
instructions nobody wrote for Sennit. Opt in explicitly when you want it:

```bash
option context-path CLAUDE.md
option context-path docs/conventions.md
option global-context-path ~/notes/how-i-like-code.md
```

Paths added this way are appended to the defaults. To drop everything a
sourced base config added and start over:

```bash
option reset context-path
option context-path docs/LLMs.md
```

## Generating one

`/init` in the TUI writes a context file describing the project — it reads the
repo and drafts build commands, layout and conventions. The default filename is
`AGENTS.md`; change it with:

```bash
option initialize-as SENNIT.md
```

The result is an ordinary file. Edit it, commit it, review it in pull requests
like anything else.

## Writing a good one

The file is prepended to the system prompt of every session in the project, so
it is paid for on every turn. Keep it to what the agent cannot work out
cheaply on its own:

- **Commands** — how to build, test, lint, and run one test. This is the single
  highest-value section; without it the agent guesses.
- **Layout** — what lives where, and which directories are generated.
- **Conventions** the code does not make obvious: error handling style, the
  logging approach, what is deliberately not abstracted.
- **Constraints** — "never edit `internal/db/sqlc`, it is generated", "this
  package must not import that one".

Leave out what the agent can read from the code in one tool call, and leave out
anything that will drift out of date faster than you will update it. A stale
context file is worse than none, because it is asserted with the same
confidence as a true one.

## Related

- [Skills](../extending/skills.md) — for procedural knowledge
  that shouldn't be in context on every turn. A skill's description sits in
  context; its body is loaded only when needed.
- [Agents](../extending/agents.md) — for a role with its own
  system prompt.
