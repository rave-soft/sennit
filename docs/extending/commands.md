# Custom commands

A custom command is a markdown file whose content is sent as a prompt when you
invoke it with `/`. It is the shortcut for a prompt you retype — no front
matter, no schema, just the text.

```markdown
<!-- .sennit/commands/changelog.md -->
Summarise every commit since the last tag as user-facing release notes.
Group by area, drop chores, one line each. No marketing language.
```

Type `/project:changelog` in the TUI and that prompt is sent.

## Where they are discovered

| Directory | Prefix |
|:--|:--|
| `~/.config/sennit/commands/` | `user:` |
| `~/.sennit/commands/` | `user:` |
| `<project>/.sennit/commands/` | `project:` |

Subdirectories become part of the name, joined with `:` — so
`.sennit/commands/go/bench.md` is `/project:go:bench`.

## Arguments

Any `$UPPERCASE` token in the file becomes a required argument, and Sennit
prompts you for each one before sending:

```markdown
<!-- .sennit/commands/bisect.md -->
Find the commit that broke $SYMPTOM, between $GOOD and HEAD.
Use git bisect with `task test` as the test. Report the commit and why.
```

Invoking `/project:bisect` asks for `SYMPTOM` and `GOOD`, substitutes them, and
sends the result.

Names must be uppercase and may contain digits and underscores — `$FILE_PATH`
and `$N2` are arguments, `$path` is not.

## Commands from MCP servers

An [MCP server](mcp.md) that exposes prompts contributes them to the same
list, named `<server>:<prompt>`, with their declared arguments. Nothing to
configure — connect the server and they appear.

## Skills as commands

A skill with `user-invocable: true` in its front matter also shows up in the
command list. Use that when a procedure is worth both: the agent picks it up
when relevant, and you can force it when you already know you want it. See
[Skills](skills.md).

## Command or skill?

| | Custom command | Skill |
|:--|:--|:--|
| Invoked by | you | the model (and you, if `user-invocable`) |
| Content | a prompt, substituted and sent | instructions loaded into context |
| Extra files | no | yes — scripts, templates, data |
| Costs context when idle | nothing | one description line |

If you find yourself invoking the same command every time a certain situation
comes up, that is the signal to make it a skill instead: the model can then
notice the situation itself.
