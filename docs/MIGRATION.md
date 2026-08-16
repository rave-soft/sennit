# Migrating from Braid

Sennit is Braid, renamed. Same codebase, same behavior, new name — but the
rename shipped **without a compatibility layer**. There is no `braid` alias
binary, no dual-read of old config files, no `BRAID_*` environment fallback,
and no automatic data migration. If you have an existing Braid install,
upgrading to Sennit will break anything that depended on the old names, and
you'll need to move your data over by hand.

This document lists every renamed contract, what breaks, and how to carry
your data forward.

## Old → new contract table

| Contract | Old (Braid) | New (Sennit) |
| --- | --- | --- |
| CLI binary | `braid` | `sennit` |
| Env var prefix | `BRAID_*` | `SENNIT_*` |
| Hook marker vars | `BRAID=1`, `AGENT=braid`, `AI_AGENT=braid` | `SENNIT=1`, `AGENT=sennit`, `AI_AGENT=sennit` |
| Hook payload vars | `BRAID_EVENT`, `BRAID_TOOL_NAME`, `BRAID_SESSION_ID`, `BRAID_CWD`, `BRAID_PROJECT_DIR`, `BRAID_TOOL_INPUT_*` | `SENNIT_EVENT`, `SENNIT_TOOL_NAME`, `SENNIT_SESSION_ID`, `SENNIT_CWD`, `SENNIT_PROJECT_DIR`, `SENNIT_TOOL_INPUT_*` |
| Project config dir | `.braid/` | `.sennit/` |
| Global config dir | `~/.config/braid/` (`%USERPROFILE%\.config\braid\` on Windows) | `~/.config/sennit/` (`%USERPROFILE%\.config\sennit\` on Windows) |
| Shell config file | `braidrc` / `.braidrc` | `sennitrc` / `.sennitrc` |
| JSON config file | `braid.json` / `.braid.json` | `sennit.json` / `.sennit.json` |
| Ignore file | `.braidignore` | `.sennitignore` |
| Context file | `BRAID.md` (and `.local` variants) | `SENNIT.md` (and `.local` variants) |
| Skills URI scheme | `braid://skills/...` | `sennit://skills/...` |
| Builtin skills | `braid-config`, `braid-hooks` | `sennit-config`, `sennit-hooks` |
| Tool names | `braid_info`, `braid_logs` | `sennit_info`, `sennit_logs` |
| Global data/state dir | `~/.local/share/braid/` (`%LOCALAPPDATA%\braid\` on Windows) | `~/.local/share/sennit/` (`%LOCALAPPDATA%\sennit\` on Windows) |
| Session/history database | `braid.db` | `sennit.db` |
| Log file | `logs/braid.log` | `logs/sennit.log` |
| Single-instance lock | `braid.lock` | `sennit.lock` |
| Global profile location | `~/.config/braid/` | `~/.config/sennit/` |

Config discovery precedence, deep-merge order, and hook aggregation rules are
unchanged — only the names moved.

## What breaks

- **Hook scripts reading `BRAID_*`.** Any hook that reads `$BRAID_TOOL_INPUT_COMMAND`,
  checks `$AGENT = braid`, or otherwise inspects a `BRAID_*` variable will see
  those variables unset under Sennit. The script won't error; it'll silently
  behave as if the variable were empty. Update hook scripts to the `SENNIT_*`
  names in the table above before upgrading, or they'll pass through without
  doing what you expect.
- **Automation calling the `braid` binary.** Cron jobs, CI steps, systemd
  units, shell aliases, or wrapper scripts that invoke `braid` will fail with
  "command not found" once you install Sennit — there is no `braid` shim.
  Point them at `sennit` instead.
- **Editors or wrappers pointing at `braid.json`.** Editor extensions, LSP
  configs, or scripts that read or write `braid.json`/`.braid.json` directly
  (rather than through the CLI) won't find or won't affect Sennit's config,
  since Sennit reads `sennit.json`/`.sennit.json` instead.
- **Anything parsing `braid://` locations.** Code or scripts that resolve
  `braid://skills/...` URIs (for example, custom tooling built against the
  old skills API) will fail to resolve now that the scheme is `sennit://`.
- **Anything invoking the old tool names.** MCP clients or hook scripts that
  call the `braid_info` or `braid_logs` tools by name need to call
  `sennit_info` / `sennit_logs` instead.

## Manual data migration

Sennit never migrates data automatically, and it never touches or deletes
your old Braid profile. To carry your history, config, and providers forward,
move the files yourself:

### Global profile

```sh
# The destination must exist first. Either launch `sennit` once and quit —
# it creates the profile directory on startup — or create it by hand:
mkdir -p ~/.config/sennit/logs

# Config (providers, models, hooks, permissions):
cp ~/.config/braid/braidrc ~/.config/sennit/sennitrc       # if you used braidrc
cp ~/.config/braid/braid.json ~/.config/sennit/sennit.json # if you used JSON config

# Session/message history database:
cp ~/.config/braid/braid.db ~/.config/sennit/sennit.db

# Logs (optional — logs are not load-bearing, skip if you don't need history):
cp ~/.config/braid/logs/braid.log ~/.config/sennit/logs/sennit.log
```

On Windows, the equivalent source is `%USERPROFILE%\.config\braid\` and the
destination is `%USERPROFILE%\.config\sennit\`.

Rename the config file itself, not just its location — `sennitrc` and
`sennit.json` are the only names Sennit looks for; a file still named
`braidrc` sitting inside `.sennit/` will not be read.

### Project-local config

For each project that has a `.braid/` directory:

```sh
cp -r .braid .sennit
mv .sennit/braid.json .sennit/sennit.json    # if present
```

If the project also has a bare `braid.json`/`.braid.json` at its root
(lower-priority than `.braid/braid.json`), rename those too.

### Rolling back

Because Sennit never deletes or modifies the old `~/.config/braid/` profile,
rolling back is just running the old `braid` binary again — your Braid data
is exactly as you left it.

## Both profiles present

If `~/.config/braid/` and `~/.config/sennit/` both exist, Sennit reads only
the new one. It does not merge, fall back to, or even notice the old
profile — sessions, providers, and hooks that only live under
`~/.config/braid/` are simply invisible to Sennit, not lost. If a fresh
`sennit` launch looks like it "forgot everything," this is almost always why:
copy the data over using the steps above.
