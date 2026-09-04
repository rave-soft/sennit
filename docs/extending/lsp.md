# Language servers

Sennit talks LSP. A configured language server gives the agent real symbol
information instead of grep results: definitions, references, call hierarchies,
rename across a project, and live diagnostics after every edit.

This is the difference between "I searched for the name and found 12 matches"
and "here is the definition and its 3 callers".

## Automatic setup

By default Sennit configures language servers itself, detecting them from root
markers in the project — `go.mod`, `package.json`, `Cargo.toml` and so on — and
starting the matching server if it is installed. Nothing to configure for the
common cases.

Turn it off if you want full control:

```bash
option auto-lsp false
```

## Configuring one explicitly

```bash
lsp add go --command gopls --env GOPATH "$HOME/go"

lsp add typescript \
  --command typescript-language-server \
  --args --stdio \
  --filetypes ts --filetypes tsx --filetypes js \
  --root-markers package.json --root-markers tsconfig.json
```

| Flag | Meaning |
|:--|:--|
| `--command` | the executable (required) |
| `--args` | one argument, repeatable |
| `--env` | one `key value` pair, repeatable |
| `--filetypes` | file type to attach to, repeatable |
| `--root-markers` | file marking a project root, repeatable |
| `--timeout` | startup timeout in seconds |
| `--init-options` | JSON passed as LSP `initializationOptions` |
| `--options` | JSON server settings |
| `--disabled` | keep the config, don't start it |

Known servers get their defaults filled in from a built-in catalogue, so
`lsp add go --command gopls` is usually enough — filetypes and root markers
come for free.

`--init-options` and `--options` are the escape hatch for server-specific
configuration:

```bash
lsp add gopls --command gopls \
  --options '{"staticcheck": true, "gofumpt": true}'
```

## What the agent gets

| Tool | Does |
|:--|:--|
| `lsp_definition` | jump to where a symbol is defined |
| `lsp_references` | every use of a symbol |
| `lsp_symbols` | outline symbols in a file |
| `lsp_workspace_symbols` | search symbols across the project |
| `lsp_hover` | show type, signature, and documentation |
| `lsp_call_hierarchy` | callers and callees |
| `lsp_rename` | rename a symbol everywhere |
| `lsp_replace_symbol` | replace a symbol's whole definition |
| `lsp_diagnostics` | current errors and warnings |
| `lsp_restart` | restart a server that has wedged |

`lsp_definition`, `lsp_symbols`, `lsp_workspace_symbols`, `lsp_hover` and
`lsp_call_hierarchy` are read-only and safe to allow without prompting.
`lsp_references` is not in that set — it still asks. See the
[tools reference](../reference/tools.md#the-read-only-set) for the full
read-only list across all tools.

```bash
permissions allow lsp_definition lsp_symbols lsp_workspace_symbols lsp_hover lsp_call_hierarchy
```

Diagnostics also surface automatically after an edit, so the agent sees the
compile error it just introduced rather than discovering it at the next build.

## Debugging

```bash
option debug-lsp true
```

Then `sennit logs -f`. The TUI's `/doctor` shows which servers are actually
running for the current session. A server that fails to start is reported and
does not block the session.

`lsp_restart` handles the case where a server is running but has stopped
responding — usually faster than restarting Sennit.
