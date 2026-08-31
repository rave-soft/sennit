# Environment and paths

## Finding out, rather than guessing

```sh
sennit dirs       # config dir, data dir, and every config file found here
sennit projects   # every project with data in the shared database
sennit logs -f    # tail the log
```

`sennit dirs` is the authority for your machine; the tables below describe the
defaults it resolves from.

## Directories

| What | Unix-like | Windows |
|:--|:--|:--|
| Config | `~/.config/sennit/` | `%USERPROFILE%\.config\sennit\` |
| State data | `~/.local/share/sennit/` | `%LOCALAPPDATA%\sennit\` |
| Session database | `~/.config/sennit/sennit.db` | `%USERPROFILE%\.config\sennit\sennit.db` |
| Logs | `~/.config/sennit/logs/sennit-<pid>.log` | …`\logs\sennit-<pid>.log` |
| Project state | `<project>/.sennit/` | `<project>\.sennit\` |

`$XDG_CONFIG_HOME` and `$XDG_DATA_HOME` are honoured where set.

The state directory holds machine-owned JSON that Sennit writes for itself —
model cache, recent selections, credentials. It is not meant to be edited by
hand, and Sennit deliberately never discovers or executes a `sennitrc` from
there.

A project's `.sennit/` holds only that project's config overrides, its skills
and agents, thread worktrees, and a single-instance lock file. Session history
is **not** there; it lives in the shared database. See
[Sessions and data storage](../concepts/sessions.md).

## Files Sennit looks for in a project

| File | Purpose |
|:--|:--|
| `.sennit/sennitrc`, `.sennitrc`, `sennitrc` | config |
| `.sennit/agents/*.md` | [agent definitions](../extending/agents.md) |
| `.sennit/skills/*/SKILL.md` | [skills](../extending/skills.md) |
| `.sennit/commands/*.md` | [custom slash commands](../extending/commands.md) |
| `AGENTS.md`, `SENNIT.md`, … | [context files](context.md) |
| `.sennitignore` | paths Sennit's file tools skip |
| `.sennit.json`, `sennit.json` | [legacy JSON config](json.md) |

## Environment variables

### Overriding locations

| Variable | Effect |
|:--|:--|
| `SENNIT_GLOBAL_CONFIG` | path to the global config file; its directory also becomes the config directory (database, logs) |
| `SENNIT_GLOBAL_DATA` | directory for machine-owned state data |
| `SENNIT_SKILLS_DIR` | replaces the global skill directories entirely |

These are the knobs for running an isolated instance — a test rig, a container,
a second profile — without touching your real configuration.

### Behaviour

| Variable | Effect |
|:--|:--|
| `SENNIT_DISABLE_DEFAULT_PROVIDERS` | ignore the built-in provider catalogue (same as `option default-providers false`) |
| `SENNIT_DISABLE_ANTHROPIC_CACHE` | disable Anthropic prompt caching |
| `SENNIT_CORE_UTILS` | use Go implementations of core shell utilities instead of the system ones. Defaults to on for Windows, off elsewhere |
| `SENNIT_SKIP_DATADIR_LOCK` | skip the single-instance lock on the data directory |
| `SENNIT_PROFILE` | serve pprof at `localhost:6060` |
| `SENNIT_UI_DEBUG` | set to `true` for TUI layout debugging |

### Set by Sennit

| Variable | Where |
|:--|:--|
| `SENNIT=1` | in shells Sennit spawns, so scripts can tell they are running under it |
| `SENNIT_VERSION` | inside `sennitrc`, for version-gating config |
| `SENNIT_TOOL_INPUT_*`, and the rest of the hook payload | in [hook](../extending/hooks.md) processes |

Version-gating a config looks like this:

```bash
if [[ $SENNIT_VERSION == "0.85."* ]]; then
    option debug true
fi
```

### API keys

Sennit does not read provider API keys from the environment on its own —
naming them in the config is explicit, and lets the value come from anywhere:

```bash
provider add anthropic --api-key "$ANTHROPIC_API_KEY"
provider add openai    --api-key "$(op read op://work/openai/key)"
```

An `.env` file in the working directory is loaded at startup, so keys can live
there too.

## Global flags

Every command accepts these:

| Flag | Effect |
|:--|:--|
| `-c`, `--cwd` | run against another directory |
| `-D`, `--data-dir` | use a different project data directory |
| `-d`, `--debug` | debug logging |

See the [CLI reference](../reference/cli.md).
