# Sessions and data storage

## One database, every project

Sessions, messages, and file history for every project are kept in a single
SQLite database at `~/.config/sennit/sennit.db` — or in `$SENNIT_GLOBAL_CONFIG`'s
directory when that is set. Each row is tagged with the project's absolute
path.

Logs are not: they live together in `~/.config/sennit/logs`, but each
running sennit writes its own `sennit-<pid>.log` there. Two sennits are
running more often than not — one works in one session, and a person
works on more than one thing — and interleaved in a single file their
delegations read as one process doing several things at once, which is
exactly what a real fault in the wake path looks like. `sennit logs`
follows whichever is newest; every record also carries its `pid`, so
concatenated logs stay readable. Logs untouched for 30 days are swept on
startup; panic dumps are left alone.

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

### Idle summarization

Waiting for the context limit means the compression happens at the worst
moment: you have just sent a message, and the session stops to replay the
whole conversation before it can start answering. So Sennit also summarizes a
session that has grown large and then gone quiet — by default, one that is
carrying more than 60,000 prompt tokens and has seen no work for four
minutes. The next thing you send starts on a compacted context, and the
request was paid for while nobody was waiting.

Both thresholds, and the pass itself, are configurable:

```bash
option auto-summarize-idle false          # turn the idle pass off
option auto-summarize-idle-tokens 100000  # only sessions this large
option auto-summarize-idle-after 10m      # after this much silence
```

Or in `sennit.json`:

```json
{
  "options": {
    "auto_summarize_idle": {
      "enabled": true,
      "context_tokens": 60000,
      "after": "4m"
    }
  }
}
```

`option auto-summarize false` turns this off as well — it means "do not
summarize behind my back", and this is exactly that. A session below the
token threshold is never touched however long it sits, a session with a turn
in flight is never touched at all, and the sweep runs on a coarse 30-second
tick, so a trip can be up to half a minute late.

Summarization and session titles run on the same model as the turn that
triggers them — there is no separate smaller/cheaper model for internal
work.

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
