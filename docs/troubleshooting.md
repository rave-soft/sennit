# Troubleshooting

## Start here

```sh
sennit doctor          # problems in the loaded config
sennit dirs            # which config files were actually found
sennit models          # which providers resolved
sennit logs -f         # what happened
```

Inside a session, `/doctor` shows more than the CLI version can: live MCP
connection health and LSP state, because it runs inside a workspace. The
`sennit_info` tool dumps the same state for the agent.

---

## Configuration

### My config isn't being applied

`sennit dirs` lists every config file discovered for the current directory. If
yours isn't listed, it isn't where Sennit looks — see
[where config lives](configuration/index.md#where-config-is-read-from).

If it *is* listed, remember the precedence: project overrides global, and
within one directory `sennitrc` overrides `sennit.json`. Having both in one
directory logs a warning; check the log.

### A `sennitrc` line does nothing

It is Bash. Run it and see:

```sh
bash -n .sennit/sennitrc          # syntax check
```

A failed command earlier in the script does not stop the rest, so a typo can
silently skip one setting. `sennit logs` records what each builtin command did.

### Settings from a sourced config won't go away

`remove`, `rm` and `option reset` act on whatever was set earlier in the script,
including through `source`. Later lines win:

```bash
source ~/team/sennit-base.sh
option reset skill-path
option skill-path ~/my/skills
```

---

## Providers and models

### A provider says "not configured"

No API key resolved. The key is whatever the config expands, so check the
expansion rather than the variable:

```bash
provider add openai --api-key "${OPENAI_API_KEY:?set OPENAI_API_KEY}"
```

The `:?` form fails loudly instead of registering a provider with an empty key.

### My custom provider's models don't appear

Either register them explicitly with `model add`, or turn on discovery and
refresh:

```bash
provider add local --type openai-compat --base-url "…" \
  --api-key "not-needed" --discover-models true
```

```sh
sennit models refresh local
```

Discovery results are cached; nothing re-scans on its own.

### An API key is set but calls fail

`sennit doctor` deliberately makes no network calls, so it cannot catch this.
Use `sennit models refresh <provider>` or the TUI's **Test Connection** in the
providers dialog, then read `sennit logs` for the actual HTTP error.

If the endpoint needs a header your config gates on an environment variable,
note that a header resolving to the empty string is **dropped**, not sent
empty — an unset variable makes the header quietly disappear.

### `reasoning_effort` seems ignored

An effort a model doesn't offer is ignored at call time in favour of the
model's own default. `sennit doctor` reports it as a config problem. Models
split into two camps: discrete levels (`--reasoning-effort`) and a single
toggle (`--think`).

---

## Agents and skills

### My agent doesn't get called

Three usual causes, in order of likelihood:

1. **The `description` is too vague.** It is the only thing the main agent sees
   when deciding. Name the triggering situation, not just the capability.
2. **The file is in the wrong place.** Only `.sennit/agents/` and
   `~/.config/sennit/agents/` are scanned. `.claude/agents` and
   `.opencode/agent` are not — run `sennit import claude --agents`.
3. **`mode: primary`** in an imported opencode file marks it as not-delegatable,
   so it is skipped.

### My agent falls back to the main model

Its `model:` didn't resolve to a configured provider. `sennit doctor` names it.
Use the full `provider/model-id` form as printed by `sennit models`.

### My skill never loads

- **The `name` must match the directory name.** A mismatch fails validation and
  the skill silently doesn't appear. This is the most common cause.
- `name` must be alphanumeric with single internal hyphens, ≤64 chars;
  `description` is required and ≤1024 chars.
- Only `.sennit/skills/` (working directory and git worktree root) and
  `~/.config/sennit/skills/` are scanned. Other tools' directories need
  `sennit import` or an explicit `option skill-path`.
- Check it isn't disabled: `option disable-skill <name>`.

Skill directories are watched, so a fix is picked up without restarting.

---

## Permissions and hooks

### It keeps asking about the same tool

```bash
permissions allow read ls grep glob ripgrep
```

If that appears to do nothing, check the name against the
[tools reference](reference/tools.md) — `sennit doctor` reports names that
match no known tool.

### A hook isn't firing

- The `--matcher` is a regex tested against the **tool name**. `^bash$` matches
  the bash tool; `bash` also matches `mcp_x_bash_thing`.
- Hooks time out at 30 seconds by default (`--timeout`).
- Exit code 2 blocks a tool call and uses stderr as the reason; other non-zero
  exits are errors, not blocks.
- Print diagnostics to stderr and read them in `sennit logs`.

The [hooks page](extending/hooks.md) has the full protocol.

---

## MCP and LSP

### An MCP server doesn't connect

`sennit doctor` never starts MCP servers, so it cannot see this. Use the TUI's
`/doctor` or the `sennit_info` tool from inside a session, and check
`sennit logs`.

Common causes: the command isn't on `PATH` for the environment Sennit runs in,
a `--timeout` too short for a server that installs on first run, or an OAuth
flow that was never completed.

A failing server does not block the session — the rest continue.

### Too many tools in context

An MCP server with a large surface costs a tool description per tool on every
turn. Narrow it:

```bash
mcp add github --type http --url "…" \
  --enabled-tools search_issues --enabled-tools get_issue
```

### LSP features aren't available

```bash
option debug-lsp true
```

then `sennit logs -f`. Check the server binary is installed and on `PATH`. If a
server is running but wedged, the agent can call `lsp_restart` — faster than
restarting Sennit.

---

## Sessions and the database

### "another instance is running"

One Sennit per project data directory at a time, enforced by a lock file in
`.sennit/`. If a previous run died without cleaning up, `SENNIT_SKIP_DATADIR_LOCK`
bypasses the check — make sure the other instance really is gone first.

### The database is large

```sh
sennit gc --dry-run
sennit gc
```

Nothing prunes history automatically. The retention window defaults to 90 days
and `0` means keep forever. See
[Sessions and data storage](concepts/sessions.md).

### An old project has a `sennit.db` in `.sennit/`

It is a leftover from before history moved to one shared database. Nothing
imports it and nothing reads it; delete it.

### "workspace already in use", mode=daemon

Plain `sennit` (no `--daemon`) refuses to start in-process while this
project's daemon holds the workspace lock: the error names `mode=daemon`
and the daemon's pid. Use `sennit attach` to open the TUI against that
daemon instead, or `sennit daemon stop` it first if you actually want to
run in-process.

### `sennit stat` numbers look wrong

They are approximate by construction for sessions that used more than one
model — those rows are marked with `~`. Message counts and time are exact. The
[caveats are documented](concepts/sessions.md#usage-statistics).

---

## Daemon and remote sessions

See [the background daemon and remote sessions](concepts/daemon.md) for how
this all fits together; a few specific problems:

### `sennit attach` says no daemon is running

`attach` never starts one, by design: it's the counterpart to `sennit
--daemon`, which does. Start it first, or use `sennit --daemon` directly
instead of `attach` if you don't need a separate step.

### A stale socket from a crashed daemon

`sennit --daemon` and `run --detach` detect a socket left behind by a
daemon that died (a `kill -9`, an out-of-memory kill) and start a fresh
one; nothing needs to be cleaned up by hand. `attach` treats such a socket
as no daemon running.
`sennit daemon status` reports "not running" for the same case rather
than hanging on a dead socket.

### The daemon won't stop

`sennit daemon stop`/`restart` refuse while the daemon is busy: a
running turn, or a pending permission/question with no client connected
to answer it. `sennit ps` shows what's holding it open; `--force` stops
it anyway.

### Where the daemon's log is

`sennit daemon status` prints its log path; `sennit daemon logs [-f]`
shows it directly, and covers both the daemon's own process log and its
startup log (useful if it failed before it got that far).

### A remote session can't connect

`sennit attach ssh://host/path` and `sennit --remote ssh://host/path` run
the system `ssh` and expect `sennit` on the remote host's `PATH`: pass
`--remote-bin` if it's installed under another name. An SSH-level failure
(bad host key, refused connection, wrong path) is reported with `ssh`'s
own stderr; the usual `ssh host` troubleshooting applies before Sennit is
even reached. A protocol version mismatch between client and remote
daemon is reported by name; update `sennit` on whichever side is older.

## Behaviour

### It didn't stop when I typed

By design. A message sent mid-turn is *steered* into the running turn, not
treated as an interrupt. Press <kbd>Esc</kbd> twice to actually stop. See
[Steering and delegation](concepts/delegation.md).

### It started work I didn't ask for, in the background

That is a delegation the agent started for itself. Turn dispatch off
entirely:

```jsonc
// sennit.json
{ "options": { "background_agents": false } }
```

One already running keeps running; the switch only blocks new dispatch.

### Conversations get truncated

Long sessions are summarized automatically near the context limit. Disable with
`option auto-summarize false`, or run **compact** yourself at a good breakpoint
rather than letting it happen mid-task.

---

## Still stuck

Collect `sennit doctor --json`, `sennit dirs`, and the relevant part of
`sennit logs`, then open an issue at
[github.com/rave-soft/sennit/issues](https://github.com/rave-soft/sennit/issues).

> [!WARNING]
> Logs and config can contain API keys and source code. Redact before posting.
