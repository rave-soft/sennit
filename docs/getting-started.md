# Getting started

## Install

### From source

Sennit is a single Go binary with no runtime dependencies. Go 1.26 or newer is
required (see [`go.mod`](https://github.com/rave-soft/sennit/blob/main/go.mod)).

```sh
git clone https://github.com/rave-soft/sennit.git
cd sennit
go build -o sennit .
```

Or, without a checkout:

```sh
go install github.com/rave-soft/sennit@latest
```

Move the resulting binary somewhere on your `PATH`.

### From a release

> [!NOTE]
> The release pipeline is configured but no tagged release has been published
> yet. Until one is, build from source. The channels below are what
> [`.goreleaser.yml`](https://github.com/rave-soft/sennit/blob/main/.goreleaser.yml)
> publishes on a tag.

| Platform | Channel |
|:--|:--|
| macOS / Linux | Homebrew — `brew install rave-soft/tap/sennit` |
| Windows | Scoop — bucket `rave-soft/scoop-bucket`; or winget |
| Arch Linux | AUR — `sennit-bin` |
| Debian / RPM / Alpine | `.deb`, `.rpm`, `.apk` packages |
| Nix | NUR — `rave-soft/nur` |
| Node | `npm install -g @rave-soft/sennit` |
| Anything | Archives attached to the GitHub release |

Packaged builds also install shell completions (bash, zsh, fish) and a
`sennit(1)` man page. From a source build you can generate completions
yourself:

```sh
sennit completion zsh > "${fpath[1]}/_sennit"
```

## Configure a provider

Sennit needs at least one model provider. The fastest path is the TUI: start
it, open the command palette and pick **providers**.

```sh
sennit
```

To do it in config instead, create `~/.config/sennit/sennitrc`:

```bash
provider add anthropic --api-key "$ANTHROPIC_API_KEY"
model anthropic/claude-sonnet-4-20250514
```

Any OpenAI-compatible endpoint works too, including a local `llama.cpp`,
`ollama` or `lmstudio` server:

```bash
provider add local \
  --type openai-compat \
  --base-url "http://127.0.0.1:8080/v1" \
  --api-key "not-needed"
```

Check what resolved:

```sh
sennit models     # every model Sennit can see, by provider
sennit doctor     # problems in the loaded config
```

[Providers and models](configuration/providers.md) covers discovery, custom
models, pricing metadata and pinning.

## First session

Run `sennit` with no arguments in a project directory and the TUI opens. Type a
prompt and send it. A few things worth knowing on the first run:

- **`/` opens the command list**, and `ctrl+g` toggles the help footer. The
  full list is in the [TUI reference](reference/tui.md).
- **Tool calls ask for permission** the first time. `ctrl+y` toggles *yolo*
  mode, which stops asking — convenient and exactly as dangerous as it sounds.
  A durable version of the same thing is
  [`permissions allow`](configuration/permissions.md).
- **Sending a message while it works does not interrupt it.** The message is
  folded into the running turn. To actually stop, press <kbd>Esc</kbd> twice.
  See [Steering, tasks and threads](concepts/delegation.md).
- **`ctrl+s` switches sessions**, and every session is saved. From the shell,
  `sennit --continue` resumes the most recent one.

Non-interactively:

```sh
sennit run "what does internal/agent/coordinator.go coordinate?"
cat README.md | sennit run "make this more glamorous" > GLAMOROUS_README.md
```

## Give it project context

`/init` (or the `init` command in the palette) writes an `AGENTS.md` describing
the project — build commands, layout, conventions — which Sennit then loads as
context in every session for that project. Edit it by hand afterwards; it is an
ordinary file.

Sennit auto-loads only its own conventions (`AGENTS.md`, `SENNIT.md` and their
casing/`.local` variants). Another tool's file is one line of config away:

```bash
option context-path CLAUDE.md
```

See [Context files](configuration/context.md).

## Where things live

```sh
sennit dirs       # config and data directories, and the config files found
sennit projects   # every project Sennit has data for
sennit logs -f    # tail the log
```

By default: config at `~/.config/sennit/`, the shared session database at
`~/.config/sennit/sennit.db`, logs at `~/.config/sennit/logs/sennit.log`, and a
per-project `.sennit/` holding only that project's config overrides and a lock
file. [Sessions and data storage](concepts/sessions.md) has the details, plus
how to prune history with `sennit gc`.

## Next steps

- [Configuration](configuration/index.md) — the `sennitrc` command reference.
- [Agents](extending/agents.md) — write a role and delegate to it.
- [Skills](extending/skills.md) — package a procedure it loads on demand.
- [Hooks](extending/hooks.md) — block, approve or rewrite tool calls.
