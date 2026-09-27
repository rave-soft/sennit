# Command line

`sennit` with no arguments opens the TUI in the current directory.

```
sennit [command] [--flags]
```

## Global flags

Accepted by every command.

| Flag | Meaning |
|:--|:--|
| `-c`, `--cwd` | run against another directory |
| `-D`, `--data-dir` | use a custom Sennit data directory |
| `-d`, `--debug` | debug logging |
| `-h`, `--help` | help |
| `--trust-project` | trust the current project, enabling its `.sennit/sennitrc`/`sennit.json` config (see [Project trust](../configuration/sennitrc.md#project-trust)) |
| `--remote <ssh://[user@]host[:port]/path>` | connect to a project's daemon on another machine over SSH instead of a local one (see [the background daemon](../concepts/daemon.md)) |
| `--remote-bin <name>` | remote `sennit` binary name for `--remote`/`attach`, if it isn't `sennit` on the remote `PATH` |
| `--ssh-opt <opt>` | extra `-o key=value` for the `ssh` connection behind `--remote`/`attach` (repeatable) |

The root command takes five more:

| Flag | Meaning |
|:--|:--|
| `-s`, `--session <id>` | resume a session by ID |
| `-C`, `--continue` | resume the most recent session |
| `-v`, `--version` | print the version |
| `-y`, `--yolo` | auto-accept all permissions |
| `--daemon` | connect to (or start) this project's daemon instead of running in-process |

```sh
sennit                              # TUI here
sennit --cwd /path/to/project       # TUI elsewhere
sennit --continue                   # resume the last session
sennit --yolo                       # no permission prompts (careful)
sennit --daemon                     # connect to (or start) this project's daemon
sennit --remote ssh://host/path     # same, for a project's daemon on another machine
```

## `run` — one non-interactive prompt

```
sennit run [prompt...] [--flags]
```

The prompt comes from the arguments, from stdin, or both. Output goes to
stdout, so it pipes.

| Flag | Meaning |
|:--|:--|
| `-m`, `--model` | model for this run; `model` or `provider/model` |
| `-q`, `--quiet` | hide the spinner |
| `-v`, `--verbose` | show logs; also hides the spinner, like `--quiet` |
| `-s`, `--session` | continue a session by ID |
| `-C`, `--continue` | continue the most recent session |
| `--detach` | start (or find) this project's daemon, hand it the turn, print the session ID, and exit immediately |

```sh
sennit run "Guess my 5 favorite Pokémon"
curl https://example.com | sennit run "Summarize this website"
sennit run "Generate a hot README for this project" > MY_HOT_README.md
sennit run --continue "Follow up on your last response"
sennit run --detach "Refactor internal/agent for clarity"
```

Without `--detach`, `run` uses this project's daemon if one is already
running, and runs in-process otherwise; either way it waits for the turn
to finish. `--detach` never waits; see
[the background daemon](../concepts/daemon.md).

## `attach`, `ps`, `daemon` — the background daemon

```sh
sennit attach [--session ID] [ssh://[user@]host[:port]/path]
sennit ps [--json]
sennit daemon status [--json]
sennit daemon stop [--force]
sennit daemon restart [--force]
sennit daemon logs [-f]
```

`attach` opens the TUI against this project's already-running daemon (or
a remote one, given an `ssh://` target); unlike `sennit --daemon` it never
starts one; a project with none running is an error. `ps` reports busy
sessions, pending permission/question prompts, and live threads or tasks,
without ever starting a daemon either. `daemon status` reports whether
one is running (pid, socket, version, log path); `stop`/`restart` refuse
a busy daemon unless given `--force`; `logs` shows its log file,
`-f`/`--follow` to keep tailing it.

Full explanation, idle exit, and remote sessions over SSH:
[the background daemon and remote sessions](../concepts/daemon.md).

## `models` — what is available

```
sennit models [search]
sennit models refresh [provider-id]
```

Lists every model from known providers, with unconfigured providers marked.
`refresh` forces model re-discovery for custom providers, and re-reads the
Codex model list when signed in (`sennit models refresh codex` for that
alone) — Codex's list is per-account and would otherwise only be fetched at
sign-in.

## `session` — browse and manage

```
sennit session list
sennit session last
sennit session show <id>
sennit session rename <id> <title>
sennit session delete <id>
```

Every subcommand accepts `--json`.

## `stat` — usage statistics

```
sennit stat [--flags]
```

| Flag | Meaning |
|:--|:--|
| `--by` | one section only: `models`, `agents`, `projects`, `skills`, `latency` |
| `--since` | window: `7d`, `30d` (default), `all` |
| `--all-projects` | with `--by projects`, aggregate across every project |
| `--json` | machine-readable output |

Also available as `sennit stats`. Read the accuracy caveats in
[Sessions and data storage](../concepts/sessions.md) before
trusting a per-model number.

`--by latency` is the one section not in the default view. It reports two
internal handoffs as distributions (events, P50, P95, max) rather than
totals: `steering_fold`, how long a steering message waited between being
queued and being folded into a step, and `completion_delivery`, how long
a finished background delegation waited before its result reached the
parent session. Both waits are dominated by how busy the parent was, so a
long tail on a session full of long turns is expected — what a regression
looks like is the P50 rising.

The TUI's `/stats` shows the same aggregation (both run on
`internal/stats`, so they cannot disagree), with tabs for three scopes:
the current session and everything it delegated, the current project, and
every project at once. It also reports how delegations ended —
"landed" meaning the delegation reached a completed state, which is what the
database records; no review verdict is stored anywhere.

## `doctor` — check the config

```
sennit doctor [--json]
```

Reports agents pinned to a model that doesn't resolve, a `reasoning_effort` set
on a model that can't reason, providers dropped for a missing API key, a main
model that fell back to a default, and `disabled_tools`/`allowed_tools` typos.

Exit code 1 if any problem is severity *error*, 0 otherwise — including when
only warnings were found.

It only inspects the loaded config and makes no network calls. For a live
check, use `sennit models refresh`, the TUI's **Test Connection**, or the
TUI's `/doctor` (which can see MCP connection health, since it runs inside a
session).

## `gc` — purge history

```
sennit gc [--flags]
```

| Flag | Meaning |
|:--|:--|
| `--dry-run` | report what would be deleted, delete nothing |
| `--days` | override `options.history_retention_days`; `0` keeps forever |
| `--project` | scope to the current project instead of the whole database |
| `--json` | machine-readable output |

Deletes old sessions and finished delegations, then `VACUUM`s the database and
checkpoints its WAL. Defaults to the whole shared database across every
project.

## `import` — bring in another tool's files

```
sennit import claude|opencode [--skills] [--agents] [--flags]
```

| Flag | Meaning |
|:--|:--|
| `--skills` | import skills |
| `--agents` | import agents |
| `--global` | read/write the user-level directories instead of the project ones |
| `--dry-run` | report what would happen, write nothing |
| `--force` | overwrite files that already exist at the destination |

Neither `--skills` nor `--agents` is on by default; pass at least one.
Existing destination files are left alone unless `--force`.

## `login` / `logout`

```
sennit login [platform] [-f] [--proxy <url>]
sennit logout [platform] [-f]
```

Available platforms: `copilot` and `codex`. `logout` with no argument lists
what you are logged in to.

| Flag | Meaning |
|:--|:--|
| `-f`, `--force` | re-authenticate even when already logged in |
| `--proxy` | Codex only: the proxy to reach OpenAI through, saved with the provider so model requests take the same route (`none` forces a direct connection) |

`login` re-authenticates the account a provider already uses; it is not how
you add a second one. For Codex it also reuses an existing Codex CLI login
on disk when there is one, so the browser step is skipped — which means it
always lands on whichever account that CLI is signed in as. To sign in as a
different account, use `sennit accounts add codex`, which always opens the
browser. See [Accounts](../configuration/accounts.md).

## `accounts` — manage a provider's stored credentials

Where `login`/`logout` manage the single credential a provider used to have,
`accounts` manages however many it has today — several Codex logins, several
API keys for the same endpoint.

```sh
sennit accounts list [provider]                       # list accounts, aliased "ls"
sennit accounts use <provider> <account>               # switch the active account
sennit accounts add <provider> [--api-key <key>]       # add another account
sennit accounts remove <provider> <account>             # drop one, aliased "rm"
sennit accounts proxy <provider> [<account>] <url|none|-> # set or clear a proxy
```

## `dirs`, `projects`, `logs`

```sh
sennit dirs                  # config and data dirs, and configs found here
sennit projects [--json]     # every project with data
sennit logs [-f] [-t N]      # view logs; -f follows, -t tails N lines (1000)
```

## `schema` and `completion`

```sh
sennit schema                # regenerate the JSON config schema
sennit completion zsh        # bash | zsh | fish | powershell
```

Packaged builds install completions and a `sennit(1)` man page already.
