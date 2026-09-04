# Legacy JSON

`sennit.json` is the original configuration format. It still works and will
keep working for the foreseeable future, but it is **deprecated**: new options
are added to [`sennitrc`](sennitrc.md) only.

```jsonc
{
  "$schema": "https://raw.githubusercontent.com/rave-soft/sennit/main/schema.json",
  "providers": {
    "anthropic": { "api_key": "$ANTHROPIC_API_KEY" }
  },
  "model": { "provider": "anthropic", "model": "claude-sonnet-4-20250514" },
  "permissions": { "allowed_tools": ["read", "ls", "grep"] },
  "options": {
    "context_paths": ["CLAUDE.md"],
    "background_agents": true,
    "history_retention_days": 90
  }
}
```

The full field list is in
[`schema.json`](https://github.com/rave-soft/sennit/blob/main/schema.json), and
`sennit schema` regenerates it from the source of truth in the code.

## Where it is read from

The same directories as `sennitrc`, as `.sennit.json` or `sennit.json`.
Everything found is merged and project settings override global ones. Within a
single directory, `sennitrc` wins over JSON and Sennit logs a warning when both
are present.

## Differences that will bite you

**Shell expansion is selective.** In JSON, only certain string fields — API
keys, URLs, MCP and LSP commands and arguments, headers — are shell-expanded at
load time. In `sennitrc` there is no such list, because it is all just Bash.

**An `agents` block is ignored.** User-defined agents are markdown files only.
Sennit reports an `agents` block as a config problem and tells you to move each
entry to `.sennit/agents/<name>.md`. See
[Agents](../extending/agents.md).

**Some options have no `option` equivalent yet.** A few settings are still
JSON-only, notably `options.web_search`, `options.threads.worktree_dir` and
`options.background_agents`. Mixing the two formats in one directory is
supported (with the warning noted above), so a JSON file holding just those is
a workable stopgap.

**It is trusted code too.** Both formats are read before the UI appears, and
the expanded fields run through your shell. The trust boundary is the same;
don't launch Sennit in a directory whose config you haven't read. As with
`sennitrc`, a project-scoped `sennit.json` is only read once you run
`sennit --trust-project` — see [Project trust](sennitrc.md#project-trust).

## Converting

Sennit ships a builtin config skill and can convert the file for you — just ask
it to, in the TUI. Reviewing the result is still on you.

## Field mapping

Common JSON fields and their `sennitrc` equivalents:

| JSON | sennitrc |
|:--|:--|
| `providers.<id>.api_key` | `provider add <id> --api-key …` |
| `models.<provider>/<id>` | `model add <provider>/<id> …` |
| `model` | `model <provider>/<id>` |
| `mcp.<name>` | `mcp add <name> …` |
| `lsp.<name>` | `lsp add <name> --command …` |
| `permissions.allowed_tools` | `permissions allow …` |
| `options.disabled_tools` | `permissions deny …` |
| `options.context_paths` | `option context-path …` |
| `options.skills_paths` | `option skill-path …` |
| `options.disabled_skills` | `option disable-skill …` |
| `options.data_directory` | `option data-directory …` |
| `options.initialize_as` | `option initialize-as …` |
| `options.notifications` | `option notifications …` |
| `options.auto_lsp` | `option auto-lsp …` |
| `options.progress` | `option progress …` |
| `options.debug` | `option debug …` |
| `options.attribution.trailer_style` | `option attribution-trailer-style …` |
| `options.tui.compact_mode` | `option ui compact …` |
| `options.tui.diff_mode` | `option ui diff …` |
| `options.tui.keybindings.<action>` | `option ui keybinding <action> <keys…>` |

`options.attribution.co_authored_by: true` in an old config now maps to
`trailer_style: assisted-by`.
