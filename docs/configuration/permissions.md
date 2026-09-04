# Permissions

By default, every tool call that writes, executes, or reaches outside your
working directory asks first. A few read-only tools — `read`, `ls`, `glob`,
`grep`, `ripgrep` — only ask when the path they're given falls outside the
working directory; inside it, they run without a prompt. `git_status`,
`git_diff`, `git_log`, and the read-only `lsp_*` tools never ask, regardless
of path. Permissions are how you stop being asked about the boring cases that
remain, and how you take dangerous ones off the table entirely.

## Three states

| State | Config | Effect |
|:--|:--|:--|
| **Ask** | the default | a prompt before each call |
| **Allow** | `permissions allow <tool>` | runs without prompting |
| **Deny** | `permissions deny <tool>` | the tool is not offered to the model at all |

```bash
permissions allow read ls grep glob ripgrep
permissions deny bash
```

The difference between *deny* and *never approving* matters: a denied tool is
hidden, so the model doesn't attempt it, doesn't reason about it, and doesn't
route around a refusal. It simply isn't there.

A reasonable starting point is to allow the read-only tools — the full set is
listed in the [tools reference](../reference/tools.md#the-read-only-set) —
and keep prompting for anything that writes or executes.

> [!WARNING]
> For `read`, `ls`, `glob`, `grep`, and `ripgrep`, the prompt this removes is
> the *only* check standing between the model and files outside your working
> directory — `~/.ssh`, a sibling repository, anything else readable by your
> user. Allowing these tools means the model can read any file on disk you
> can, without asking again.

Tool names are listed in the [tools reference](../reference/tools.md).
A name that doesn't match any known tool is reported by `sennit doctor`, so a
typo in `permissions allow` doesn't silently do nothing.

## Yolo mode

`ctrl+y` in the TUI, or `--yolo` on the command line, auto-approves everything
for the session.

> [!WARNING]
> Yolo mode approves `bash`, `write` and `edit` too. It is for a sandbox, a
> throwaway container, or work you are watching closely — not for a session you
> walk away from in a repository you care about.

`permissions bypass on` sets the same thing persistently, as
`permissions.bypass` in config, instead of per-session with `--yolo`:

```bash
permissions bypass on
permissions bypass off
```

The same warning applies, more so: it survives across restarts until you turn
it back off. `sennit doctor` flags it as a standing problem for exactly this
reason.

## Hooks decide too

`permissions allow/deny` is a static list. A [hook](../extending/hooks.md)
is a shell command that sees the actual arguments and decides per call — which
is how you express things a list cannot:

```bash
# Auto-approve read-only bash, prompt for everything else.
hook add PreToolUse --matcher "^bash$" \
  --command "./hooks/safe-bash.sh" --name safe-bash

# Refuse destructive commands outright.
hook add PreToolUse --matcher "^bash$" \
  --command "./hooks/no-rm-rf.sh" --name no-rm-rf
```

A `PreToolUse` hook can allow, deny or leave the call to the normal permission
flow, and it can rewrite the arguments on the way through. The
[hooks page](../extending/hooks.md) has the full protocol and
worked examples.

## Delegated work is not exempt

Background tasks and threads run tools through the same permission service as
the foreground turn. Delegating does not launder a tool call into an approved
one — a task that wants to run `bash` prompts exactly as the main agent would.

To rule out unattended concurrent work entirely, turn dispatch off rather than
relying on prompts:

```jsonc
// sennit.json
{ "options": { "background_agents": false } }
```

## Restricting a single agent

An agent defined in `.sennit/agents/` can be given a `tools:` list, which is
narrower than the global permission set — a reviewer that can read and grep but
holds no `edit`, `write` or `bash` at all:

```markdown
---
name: reviewer
description: Reviews Go code for correctness and idiom.
tools: [read, grep, glob, ls]
---
```

Note that a foreign agent file's `permission:` block (opencode's format) is
**not** enforced when imported. Restrict through `tools:` or `permissions`
instead. See [Agents](../extending/agents.md).
