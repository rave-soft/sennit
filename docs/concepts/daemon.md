# The background daemon and remote sessions

Sennit normally runs in-process: start it, work, and when you close the
terminal the turn stops with it. `--daemon` runs the backend headlessly
instead, so a turn survives closing the terminal or a dropped SSH
connection, and several windows can share one project's agent instead of
each starting its own.

In-process stays the default. No config option turns the daemon on; only
an explicit flag or command does:

```sh
sennit --daemon              # connect to (or start) this project's daemon
sennit run --detach "..."    # hand a project's daemon a turn and exit
sennit attach                # open the TUI against a running daemon
sennit daemon restart        # explicit lifecycle management
```

## Starting and attaching

`sennit --daemon` connects to this project's daemon if one is already
running, or starts one and connects. A project's daemon is identified by
its git common directory, so every worktree of the same repository shares
it, and one running daemon is enough for everyone working on that project
on the same machine.

`sennit attach [--session ID]` opens the TUI against a daemon that's
already running, without ever starting one: a project with none running
is an error here, telling you to use `sennit --daemon` instead. Close the
TUI (or lose the SSH session it's running over) and the daemon keeps
going; the next `sennit attach` picks the same sessions back up, still
busy or not, where they were left.

`sennit run --detach "prompt"` hands a project's daemon a single turn and
exits immediately, printing the session ID. Without `--detach`, `run`
uses the daemon if one is already running for the project, and runs
in-process otherwise.

## Checking on it

```sh
sennit ps                          # busy sessions, pending prompts, live threads/tasks
sennit daemon status [--json]      # is it running, pid, socket, version, log path
sennit daemon logs [-f]            # this project's daemon log
sennit daemon stop [--force]       # stop it (refuses if busy, unless --force)
sennit daemon restart [--force]    # stop and start again
```

`sennit ps` and `sennit daemon status` never start a daemon; a project
with none running is reported as such, not an error.

## Idle exit

With nothing left to do, a daemon exits on its own after
`options.daemon.idle_timeout` (default `10m`, set with `option
daemon-idle-timeout <duration>` in `sennitrc`, e.g. `option
daemon-idle-timeout 30m`). A negative or zero value disables idle exit
entirely: the daemon then runs until stopped or signaled.

"Nothing left to do" means no connected client, no busy session, no live
thread or task, and no pending permission request or question. A
permission prompt or question with no client connected to
answer it does not time out: the turn simply waits, and the daemon does
not exit while it's waiting, however long that takes. Connect with
`sennit attach` (or `sennit ps` to see what's pending) and it's answered
the same as if you'd never left.

## Remote sessions over SSH

```sh
sennit attach ssh://[user@]host[:port]/path
sennit --remote ssh://[user@]host[:port]/path
sennit ps --remote ssh://[user@]host[:port]/path
```

Reaching a daemon on another machine runs `ssh` (the system binary, so
your own `~/.ssh/config`, keys, and `ProxyJump` all apply) to invoke
`sennit daemon bridge --cwd <path>` on the remote host, and speaks gRPC
over that SSH session's stdio. This means:

- **`sennit` must be installed and on `PATH`** on the remote host (or
  named explicitly with `--remote-bin` if it isn't called `sennit`
  there). `sennit daemon bridge` itself starts the project's daemon on
  the remote machine if none is running yet.
- **Config is the daemon machine's.** Providers, models, MCP servers,
  LSPs, hooks and permissions all come from the remote project's own
  config; connecting from elsewhere does not substitute your local setup
  for them.
- **The TUI still looks like yours.** Theme, keybindings, and the rest of
  the client-only settings are read from your own machine's global
  config, not the remote one, so the interface looks the way you've set
  it up regardless of which project's daemon you're attached to.
- **Paths and the window title show the remote host**, so it's clear
  which machine a path or a running command refers to.
- **`--remote-bin`** names the remote binary if it isn't `sennit` on
  `PATH`, and **`--ssh-opt`** (repeatable) adds an extra `-o key=value` to
  the `ssh` invocation.

### Sign-in from a remote session

Signing in to a provider that uses a browser redirect (Codex, an MCP
server's OAuth) opens the browser on your own machine as usual; the
callback it receives is relayed back to the daemon over the same
connection, so the flow completes on the remote machine's config without
needing a browser there. AWS SSO's device-code flow runs entirely on the
daemon machine and only needs you to open the URL it prints.

## What stays local, either way

A few things never go through the daemon at all, in-process or remote:

- **The CLI utilities** (`doctor`, `models`, `gc`, `import`, `logs`,
  `session`, `stat`) open the local database and config directly. Run
  them over SSH on the machine whose data you want to inspect.
- Attachments, clipboard paste, `$EDITOR`, desktop notifications, and
  opening a browser all happen on whichever machine the TUI itself is
  running on.

See the [command line reference](../reference/cli.md) for every flag, and
the [`option` reference](../configuration/sennitrc.md#option) for
`daemon-idle-timeout`.
