# Configuration

Sennit is configured with Bash. A `sennitrc` runs at startup — globally from
`~/.config/sennit/sennitrc`, per project from `.sennit/sennitrc`, `.sennitrc`
or `sennitrc` — and a set of builtin commands build the config as the script
executes.

```bash
# .sennit/sennitrc
provider add local \
  --type openai-compat \
  --base-url "http://127.0.0.1:8080/v1" \
  --api-key "not-needed"

permissions allow read grep
option skill-path ./skills
```

Because it is a shell script, a shared team config is a `source`, a secret can
come from your password manager, and a machine-specific setting is an `if`.

> [!WARNING]
> `sennitrc` is trusted code: it runs with your shell privileges before the UI
> appears. Don't launch Sennit in a directory whose config you haven't read,
> and don't download configs you haven't read either.

Project-scoped config only runs after you explicitly trust the project with
`sennit --trust-project` — an untrusted project's config is skipped entirely,
not run. See [Project trust](sennitrc.md#project-trust).

## The seven builtin commands

| Command | Configures |
|:--|:--|
| `provider` | model providers — API keys, base URLs, headers |
| `model` | custom models, and which model is selected |
| `mcp` | Model Context Protocol servers |
| `lsp` | language servers |
| `hook` | shell commands that run on tool events |
| `permissions` | which tools skip prompting, and which are hidden |
| `option` | everything else: paths, attribution, the TUI |

Every flag of every one of them is in the [sennitrc reference](sennitrc.md).
The pages in this section cover the areas that need more than a flag list.

## Where config is read from

Lower numbers win.

| Priority | Unix-like | Windows |
|:--|:--|:--|
| 1 | `./.sennitrc` | `.\.sennitrc` |
| 2 | `./sennitrc` | `.\sennitrc` |
| 3 | `$XDG_CONFIG_HOME/sennit/sennitrc` | `%XDG_CONFIG_HOME%\sennit\sennitrc` |

Everything found is merged, project settings override global ones, and within
one directory `sennitrc` overrides `sennit.json` — with a warning logged when
both are present.

`sennit dirs` prints exactly which files were found for the current directory,
and `sennit doctor` reports problems in what was loaded.

## In this section

- [sennitrc reference](sennitrc.md) — every command and flag.
- [Providers and models](providers.md) — connecting to models, including
  local and OpenAI-compatible servers.
- [Accounts](accounts.md) — giving a provider more than one credential, and
  switching between them.
- [Context files](context.md) — `AGENTS.md`, `SENNIT.md`, and what gets
  loaded into every session.
- [Permissions](permissions.md) — allowing, denying and yolo mode.
- [Environment and paths](environment.md) — environment variables, config
  and data directories.
- [Legacy JSON](json.md) — the deprecated `sennit.json` format.
