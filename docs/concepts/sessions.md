# Sessions and data storage

## One database, every project

Sessions, messages, and file history for every project are kept in a single
SQLite database at `~/.config/sennit/sennit.db` — or in `$SENNIT_GLOBAL_CONFIG`'s
directory when that is set. Each row is tagged with the project's absolute
path.

Logs are unified the same way, at `~/.config/sennit/logs/sennit.log`.

A project's own `.sennit/` directory holds only its config overrides, its
agents and skills, thread worktrees, and a single-instance lock file. No
history.

> [!IMPORTANT]
> There is no per-project database any more, and no compatibility layer. A
> `sennit.db` left in an old project's `.sennit/` from before that change is
> ignored entirely — nothing imports it, and it can be deleted.

The single-instance lock means one Sennit per project data directory at a time.
`SENNIT_SKIP_DATADIR_LOCK` bypasses it if you know why you want to.

## Working with sessions

```sh
sennit --continue                     # resume the most recent session here
sennit --session <id>                 # resume a specific one
sennit run --continue "and now?"      # same, non-interactively
```

From the shell:

```sh
sennit session list                   # every session for this project
sennit session last                   # the most recent
sennit session show <id>              # full detail
sennit session rename <id> "title"
sennit session delete <id>
```

Every subcommand takes `--json` for machine-readable output.

In the TUI, `ctrl+s` opens the session switcher and `ctrl+n` starts a fresh
one.

## Sub-sessions

A delegation gets its own session, parented to the one that started it. That is
what makes each delegated call its own collapsible block in the transcript, and
what lets `sennit stat` break usage down per agent.

Deleting a parent session deletes its agent-tool and title sub-sessions too,
regardless of their own age.

## Summarization

Long conversations are summarized automatically when they approach the model's
context limit; the **compact** command does it on demand. Turn the automatic
behaviour off with:

```bash
option auto-summarize false
```

Sennit uses a smaller, cheaper model for summarization and session titles. That
choice is not configurable.

## Usage statistics

```sh
sennit stat                       # models, agents, projects, skills
sennit stat --by agents
sennit stat --since 7d            # 7d, 30d (default), or all
sennit stat --by projects --all-projects
sennit stat --json
```

Two caveats are baked into this data, and the command says so itself:

- **Token counts are stored per session, not per message.** A session that used
  one model attributes exactly. A session that used several splits its tokens
  across them in proportion to each model's share of the assistant messages;
  those rows are marked approximate (`~`). Message counts and time are always
  exact, since they come from per-message timestamps.
- **Subagent sessions are grouped by title.** Delegations through the generic
  `agent` tool all collapse into one "New Agent Session" bucket; named agents
  get their own row.

## Pruning history

Nothing is deleted automatically. `sennit gc` is the tool, and it is opt-in:

```sh
sennit gc --dry-run           # what would go
sennit gc                     # apply the configured retention window
sennit gc --days 30           # override it for this run
sennit gc --project           # only this project, not the whole database
```

It deletes sessions (with their messages, files and read-file records) whose
last activity is older than the window, deletes finished threads of the same
age — completed, merged, conflict, merge_blocked, failed, interrupted, never
pending/running/merging — then `VACUUM`s the database and checkpoints its WAL.

The window defaults to `options.history_retention_days`, 90 days when unset. `0`
means keep forever and makes `sennit gc` a no-op.

```jsonc
// sennit.json
{ "options": { "history_retention_days": 30 } }
```

By default `gc` operates on the whole shared database across every project,
because that is what reclaiming disk space means when the database is shared.
`--project` scopes it to the current working directory.

Running it from cron is the intended pattern; nothing enforces retention on its
own.

## Threads on disk

A [thread](delegation.md) gets a real git worktree
and branch. Worktrees default to `<repo>/.sennit/threads/<name>`, which the
repository's own git ignores, so a thread is not mistaken for a second copy of
the project.

```jsonc
// sennit.json
{ "options": { "threads": { "worktree_dir": "/var/tmp/sennit-threads" } } }
```

A relative path there resolves against the *parent of the repository root*, not
the working directory; an absolute path is used as-is.

Cancelling a thread leaves its worktree and branch on disk so you can still
inspect or resume the work. `sennit threads remove <name>` tears it down.

## Finding things

```sh
sennit dirs        # config and data directories, and configs found here
sennit projects    # every project with data, as a table or --json
sennit logs -f     # tail; -t N for the last N lines
```
