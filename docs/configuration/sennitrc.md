# Config

> [!NOTE]
> This document was designed for both humans and agents.

> [!TIP]
>
> Sennit can configure itself via a builtin config skill. That is to say,
> can generally just tell Sennit want you want to configure using natural
> language.
>
> If you're migrating from the old JSON format, you can also ask Sennit to
> convert the config for you.

Sennit is configured with Bash via a set of Sennit-specific builtin commands. By
default, global config lives at `~/.config/sennit/sennitrc` on Unix-like systems
and `%USERPROFILE%\.config\sennit\sennitrc` on Windows. It works like a `.bashrc`:
it runs when Sennit starts and configures the agent.

```bash
# Add Ollama.
provider add ollama --type ollama --base-url "http://localhost:11434/v1"

# Register a model on Ollama.
model add ollama/llama3.3 --name "Llama 3.3" --context-window 128000

# Auto-approve some tools.
permissions allow read edit

# Add an MCP server
mcp add github \
  --type http \
  --url "https://api.githubcopilot.com/mcp/" \
  --header Authorization "Bearer $GITHUB_TOKEN"
```

Since it’s Bash, so you can use logic, `source` other files, and so on. It’s
really handy.

```bash
# Change config based on the machine you're on.
if [[ $HOSTNAME == "babysquid" ]]; then
    option skill-path "$HOME/squid-skills"
fi

# Load some extra config
source "$XDG_CONFIG_HOME/squid-config.sh"

# Get API keys from your password manager.
provider add my-secret-provider \
  --type openai-compat \
  --base-url "https://api.example.com/v1" \
  --api-key "$(op read my-secret-key)"
```

## Why Bash?

Two reasons:

1. Sennit ships with a first-class Bash interpreter, so we get the logic for
   free.
2. Ultimately, Sennit needs to be able to configure itself, and command-based
   config allows both users and the agent to use the same tools.

## What about JSON?

JSON is still supported but is deprecated and, while it's supported, it won't
be receiving new features. For more see [Legacy JSON](#legacy-json).

## Config versioning

Not breaking the config API is really important to us! That said, you can
target specific Sennit versions with `$SENNIT_VERSION`:

```bash
if [[ $SENNIT_VERSION == "0.85.*" ]]; then
    option debug true
fi
```

## Security

Just like `sennit.json`, `sennitrc` is a trusted file. Guard it carefully and
don't download random configs without reading them first.

## Project trust

Project-scoped config (`.sennit/sennitrc`, `.sennitrc`, `sennitrc`, and their
JSON equivalents) is only read if the project is trusted. The search starts
at the current working directory and walks up to the git working tree root
(or the working directory itself, if it isn't inside a git repository), so a
config file at the root of your repository is picked up and gated the same
way from any subdirectory you happen to be in. Trust is recorded per
directory, as a marker file under the global config directory
(`internal/config/trust.go`), and there is no interactive "trust this
project?" prompt — the only way to grant it is:

```sh
sennit --trust-project
```

Until then, project-scoped config is skipped entirely: not merged with a
warning, not partially applied — absent. `sennit doctor` and the config
loader both report an untrusted project as a problem, with a hint to restart
with `--trust-project` after you've reviewed the files. Global config
(`~/.config/sennit/sennitrc`) is never gated by this — it is trusted because
you, not the project, control it.

## Where config lives

Sennit looks for config in the following places, with lower numbers taking
precedence:

| Priority | Unix-like                          | Windows                              |
| -------- | ----------------------------------- | ------------------------------------- |
| 1        | `./.sennitrc`                       | `.\.sennitrc`                         |
| 2        | `./sennitrc`                        | `.\sennitrc`                          |
| 3        | `$XDG_CONFIG_HOME/sennit/sennitrc`  | `%XDG_CONFIG_HOME%\sennit\sennitrc`   |

Legacy JSON uses `.sennit.json` / `sennit.json` in the same directories as the
above. Everything found is merged, with project settings overriding global ones
and `sennitrc` overriding JSON in the same directory. If a folder has both, they
merge and Sennit logs a warning. The one exception is providers and models,
which a project config cannot set at all — see
[Providers and models are global-only](#providers-and-models-are-global-only).

Data directories (`~/.local/share/sennit` on Unix-like systems and
`%LOCALAPPDATA%\sennit` on Windows) contain machine-owned JSON state. Sennit does
not discover or execute a `sennitrc` from those locations.

> [!NOTE]
> Sennit also stores state data in `$XDG_DATA_HOME/sennit`
> (`%LOCALAPPDATA%\sennit` on Windows). This is application state, and should
> not be edited by hand.

### Providers and models are global-only

Providers and the selected model are a property of your machine, not of a
checked-out repository, so Sennit reads them **only** from the global layers:
`$XDG_CONFIG_HOME/sennit/sennitrc`, `$XDG_CONFIG_HOME/sennit/sennit.json`, the
data-directory JSON, and `/etc/sennit/sennit.json`.

A `provider` or `model` command in a project `sennitrc`, or a `providers` /
`model` / `recent_models` block in a project `sennit.json` (including
`.sennit/sennit.json`), is dropped before the merge and reported by
`sennit doctor`. That way cloning a repository can never repoint your session at
someone else's endpoint or swap the model out from under you.

Everything else — permissions, MCP and LSP servers, hooks, options, env — still
merges per project as described above. Sennit's own writes (the model picker,
`sennit login`, adding a provider from the TUI) always go to the global config.

## Command Reference

The sections below read like CLI help. Entity commands use `add` to create or
update something and `remove` (or `rm`) to delete it. Booleans accept
`true/false/1/0/yes/no`, in any case.

```text
Available Commands:
  provider      Manage model providers
  model         Manage models and model selection
  mcp           Manage MCP servers
  lsp           Manage language servers
  hook          Manage hooks
  permissions   Configure tool permissions
  option        Configure general Sennit behavior
```

### provider

Manage model providers. Only effective in a global config — see
[Providers and models are global-only](#providers-and-models-are-global-only).

```text
Usage:
  provider [command]

Available Commands:
  add       Add or update a provider
  remove    Remove a provider and its custom models
  rm        Alias for remove
```

#### `provider add`

Add a provider, or update an existing provider with the same ID.

```text
Usage:
  provider add <id> [flags]

Flags:
      --name string                 display name
      --type string                 provider type (openai, openai-compat, anthropic, ollama, …)
      --api-key string              API key
      --base-url string             API base URL
      --disable bool                disable without removing
      --flat-rate bool              use flat-rate billing
      --discover-models bool        auto-discover and merge provider models
      --system-prompt-prefix string text prepended to the system prompt
      --extra-header key value      add an HTTP header (repeatable)
      --extra-body JSON             merge a JSON object into request bodies
      --provider-options JSON       merge a provider-specific JSON object
```

```bash
provider add deepseek \
  --type openai-compat \
  --base-url "https://api.deepseek.com/v1" \
  --api-key "${DEEPSEEK_API_KEY:?set DEEPSEEK_API_KEY}"
```

Headers whose value resolves to the empty string (an unset `$VAR`, a
`$(...)` that prints nothing, or a literal `""`) are dropped from the
outgoing request. This makes env-gated headers safe:

```bash
provider add openai \
  --extra-header OpenAI-Organization "$OPENAI_ORG_ID"
```

If `OPENAI_ORG_ID` is unset, the header is simply not sent.

#### `provider remove`

Remove a provider and all custom models registered on it.

```text
Usage:
  provider remove <id>
  provider rm <id>
```

### model

Manage custom models and the selected model. Model references use the same
`<provider>/<id>` form printed by `sennit models`. Like `provider`, only
effective in a global config — see
[Providers and models are global-only](#providers-and-models-are-global-only).

```text
Usage:
  model [command]

Available Commands:
  add       Register a custom model on an existing provider
  remove    Remove a custom model
  rm        Alias for remove
  (none)    Set or print the selected model
```

#### `model add`

Register a custom model on an existing provider.

```text
Usage:
  model add <provider>/<id> [flags]

Flags:
      --name string                 display name
      --context-window int          context window in tokens
      --default-max-tokens int      default maximum output tokens
      --can-reason bool             model supports reasoning
      --supports-images bool        model accepts image input
      --price-input float           input price per 1M tokens
      --price-output float          output price per 1M tokens
      --price-cache-create float    cache-creation price per 1M tokens
      --price-cache-hit float       cache-hit price per 1M tokens
      --reasoning-effort string     low, medium, or high
```

#### `model remove`

Remove a custom model from its provider.

```text
Usage:
  model remove <provider>/<id>
  model rm <provider>/<id>
```

#### `model`

Set the selected model. With no model argument, print the current selection.
Internal work like titles and summarization runs on this same selected
model — there is no separate smaller/cheaper model for it.

```text
Usage:
  model [<provider>/<id>] [flags]

Flags:
      --think                       enable thinking mode
      --reasoning-effort string     low, medium, or high
      --max-tokens int              maximum output tokens
      --temperature float           sampling temperature
      --top-p float                 top-p sampling (0–1)
      --top-k int                   top-k sampling
      --frequency-penalty float     frequency penalty
      --presence-penalty float      presence penalty
      --provider-options JSON       merge a provider-specific JSON object
```

```bash
model openai/gpt-4o --think
echo "coding with: $(model)"   # prints: openai/gpt-4o
```

### mcp

Manage Model Context Protocol servers.

```text
Usage:
  mcp [command]

Available Commands:
  add       Add or update an MCP server
  remove    Remove an MCP server
  rm        Alias for remove
```

#### `mcp add`

Add an MCP server, or update an existing server with the same name.

```text
Usage:
  mcp add <name> [flags]

Flags:
      --type string              stdio, sse, or http (default "stdio")
      --command string           executable for stdio servers
      --args string              command argument (repeatable)
      --env key value            environment variable (repeatable)
      --url string               URL for HTTP/SSE servers
      --header key value         HTTP header (repeatable)
      --timeout int              startup timeout in seconds
      --disabled bool            disable without removing
      --disabled-tools string       deny a server tool (repeatable)
      --enabled-tools string        allow only these server tools (repeatable)
      --oauth bool                  enable OAuth 2.1 flow (HTTP only)
      --oauth-client-id string      pre-registered OAuth client ID
      --oauth-client-secret string  pre-registered OAuth client secret
      --oauth-callback-port int     fixed localhost port for the OAuth callback
```

```bash
mcp add github --type http \
  --url "https://api.githubcopilot.com/mcp/" \
  --header Authorization "Bearer $GH_PAT"
```

As with providers, a header whose value resolves to the empty string is
dropped from the outgoing request.

#### `mcp remove`

Remove an MCP server.

```text
Usage:
  mcp remove <name>
  mcp rm <name>
```

### lsp

Manage language servers.

```text
Usage:
  lsp [command]

Available Commands:
  add       Add or update a language server
  remove    Remove a language server
  rm        Alias for remove
```

#### `lsp add`

Add a language server, or update an existing server with the same name.

```text
Usage:
  lsp add <name> --command <command> [flags]

Flags:
      --args string              command argument (repeatable)
      --env key value            environment variable (repeatable)
      --filetypes string         file type to attach to (repeatable)
      --root-markers string      root marker file (repeatable)
      --timeout int              startup timeout in seconds
      --disabled bool            disable without removing
      --init-options JSON        initialization options
      --options JSON             server settings
```

```bash
lsp add go --command gopls --env GOPATH "$HOME/go"
```

#### `lsp remove`

Remove a language server.

```text
Usage:
  lsp remove <name>
  lsp rm <name>
```

### hook

Manage hooks. See the [hooks docs](../extending/hooks.md) for what they can do and how
they run.

```text
Usage:
  hook [command]

Available Commands:
  add       Add a hook to an event
  remove    Remove a named hook, or clear an event
  rm        Alias for remove
```

#### `hook add`

Add a shell command that runs when the given hook event fires.

```text
Usage:
  hook add <event> --command <command> [flags]

Flags:
      --command string           shell command to run (required)
      --name string              name used for later removal
      --matcher string           regex tested against the tool name
      --timeout int              timeout in seconds (default 30)
```

```bash
hook add PreToolUse --matcher "^bash$" \
  --command "./hooks/no-haskell.sh" --name no-haskell
```

#### `hook remove`

Remove hooks from an event. Without `--name`, remove every hook for the event.

```text
Usage:
  hook remove <event> [--name <name>]
  hook rm <event> [--name <name>]

Flags:
      --name string              remove hooks with this name
```

### permissions

Configure tool permissions. `allow` skips approval prompts; `deny` hides tools
from the agent entirely.

```text
Usage:
  permissions [command]

Available Commands:
  allow     Allow tools without prompting
  deny      Hide tools from the agent
```

#### `permissions allow`

Allow one or more tools to run without prompting.

```text
Usage:
  permissions allow <tool> [<tool> ...]
```

#### `permissions deny`

Hide one or more tools from the agent so they cannot be called.

```text
Usage:
  permissions deny <tool> [<tool> ...]
```

```bash
permissions allow read ls grep edit
permissions deny bash
```

### option

Configure general Sennit behavior, paths, attribution, and the terminal UI.
Boolean values are optional and default to `true`.

```text
Usage:
  option <key> [value]
  option [command]

Available Commands:
  reset     Clear every value from a list option
  ui        Configure terminal UI behavior

Boolean Keys:
  debug                          enable debug logging
  debug-lsp                      enable LSP debug logging
  auto-lsp                       automatically configure language servers
  progress                       show progress indicators
  metrics                        send anonymous usage metrics
  auto-summarize                 automatically summarize long conversations
  auto-summarize-idle            summarize a large session once it goes quiet
  default-providers              include built-in providers
  attribution-generated-with     add the Generated with Sennit line

String Keys:
  data-directory string            directory for project data and state
  initialize-as string             context filename created by sennit init
  notifications string             notification style: auto, native, osc, bell,
                                   or disabled
  attribution-trailer-style string attribution trailer: none or assisted-by
                                   (old configs with co_authored_by: true now
                                   map to assisted-by)

Integer Keys:
  history-retention-days int       days of history "sennit gc" keeps
  auto-summarize-idle-tokens int   context size an idle session must exceed
                                   before it is summarized (prompt tokens)

Duration Keys:
  auto-summarize-idle-after string how long a session must sit idle before
                                   it is summarized, e.g. 4m or 90s

List Keys:
  context-path string             append a project context path
  global-context-path string      append a global context path
  skill-path string               append a skill directory
  disable-skill string            hide a skill from the agent
```

```bash
option progress false
option skill-path ./skills
option attribution-trailer-style assisted-by
```

#### `option reset`

Clear every value previously added to a list option. Values added after the
reset are kept.

```text
Usage:
  option reset <key>

Available Keys:
  context-path          clear project context paths
  global-context-path   clear global context paths
  skill-path            clear additional skill directories
  disable-skill         clear disabled skill names
```

#### `option ui`

Configure terminal UI presentation, keyboard shortcuts, and completion-list
limits. On macOS, default `ctrl+` shortcuts use `super+` (Command) instead.

```text
Usage:
  option ui <key> <value>

Available Keys:
  compact bool                  use the compact chat layout
  diff unified|split            choose unified or side-by-side diffs
  transparent bool              use the terminal background
  scrollbar string              control chat scrollbar visibility: default,
                                always, or never
  spinner string                motion of the working indicator: scramble,
                                pulse, dots, or none
  completions-max-depth int     maximum directory depth shown by completions
  completions-max-items int     maximum items returned to completions
  keybinding action key...      replace all shortcuts for an action
```

```bash
option ui compact true
option ui diff unified
option ui transparent true
option ui scrollbar always
option ui spinner dots
option ui completions-max-depth 4
option ui completions-max-items 200
option ui keybinding commands super+p
option ui keybinding editor.newline shift+enter super+j
```

Keybinding action names use the groups `editor.*`, `chat.*`, and
`initialize.*`; global actions have no prefix. Examples include `quit`,
`commands`, `editor.send_message`, `chat.page_up`, and
`initialize.switch`. JSON configs use the equivalent
`options.tui.keybindings` object whose values are arrays of keys.

> [!IMPORTANT]
> Only Sennit's own skill directories load by default — you do NOT need
> `skill-path` for them: `.sennit/skills` in the working directory (and at
> the git worktree root, so monorepo-level skills are found) plus the global
> `~/.config/sennit/skills`.
>
> Skills written for another tool (`.claude/skills`, `.opencode/skills`,
> `.cursor/skills`, …) are **not** auto-discovered. Bring them in explicitly
> with `sennit import claude --skills`, which copies and validates them into
> `.sennit/skills`, or point at the directory yourself with `skill-path`.

## Composing configs

Because it's Bash, a shared base config is just a `source`:

```bash
# Unix-like: ~/.config/sennit/sennitrc
# Windows:   %USERPROFILE%\.config\sennit\sennitrc
source ~/team/sennit-base.sh    # sets up providers, a few skills

# …but on this machine, drop a skill path the base added and add my own.
option reset skill-path
option skill-path ~/my/skills
```

`remove`, `rm`, and `option reset` all act on whatever was set earlier in the
script or pulled in via `source`. Later lines win, just like a shell.

## Legacy JSON

`sennit.json` is the original format and is now deprecated. We plan to support
it for the forseeable future, but new configuration options will only be added
to Bash-based config.

```jsonc
{
  "$schema": "https://rave-soft.github.io/sennit/schema.json",
  "providers": {
    "anthropic": { "api_key": "$ANTHROPIC_API_KEY" },
  },
  "model": { "provider": "anthropic", "model": "claude-sonnet-4-20250514" },
  "permissions": { "allowed_tools": ["read", "ls", "grep"] },
}
```

For a full reference, See the [JSON schema](https://github.com/rave-soft/sennit/blob/main/schema.json).

The `providers`, `model`, and `recent_models` keys are read only from a global
`sennit.json`; in a project file they are ignored (see
[Providers and models are global-only](#providers-and-models-are-global-only)).

In JSON, only selected string fields (API keys, URLs, MCP/LSP commands and args,
headers) are shell-expanded at load time. In `sennitrc` there's no such list —
it's all just Bash.

Both formats are trusted code: they run with your shell privileges before the UI
appears. Don't launch Sennit in a directory whose config you haven't read.

---
