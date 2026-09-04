# Sennit Development Guide

## Project Overview

Sennit is a terminal-based AI coding assistant built in Go by
rave-soft. It connects to LLMs and gives them tools to read,
write, and execute code. It supports multiple providers (Anthropic, OpenAI,
Gemini, Bedrock, Copilot, Hyper, MiniMax, Vercel, and more), integrates with
LSPs for code intelligence, and supports extensibility via MCP servers and
agent skills.

The module path is `github.com/rave-soft/sennit`.

## Architecture

```
main.go                            CLI entry point (cobra via internal/cmd)
internal/
  app/app.go                       Top-level wiring: DB, config, agents, LSP, MCP, events
  cmd/                             CLI commands (root, run, login, models, stats, sessions)
  config/
    config.go                      Config struct, context file paths, agent definitions
    load.go                        sennitrc and sennit.json loading and validation
    provider.go                    Provider configuration and model resolution
  shellconfig/                      Bash-powered config format (sennitrc builtins)
  agent/
    agent.go                       SessionAgent: runs LLM conversations per session
    coordinator.go                 Coordinator: manages named agents ("coder", "task")
    hooked_tool.go                 Decorator that runs PreToolUse hooks before tool execution
    prompts.go                     Loads Go-template system prompts
    templates/                     System prompt templates (coder.md.tpl, task.md.tpl, etc.)
    tools/                         All built-in tools (bash, edit, read, grep, glob, etc.)
      mcp/                         MCP client integration
  hooks/                           Hook engine: runs user shell commands on hook events
    hooks.go                       Decision types, aggregation logic, event constants
    runner.go                      Parallel hook execution, timeout, dedup
    input.go                       Stdin payload builder, env vars, stdout parsing (Sennit + Claude Code compat)
  session/session.go               Session CRUD backed by SQLite
  message/                         Message model and content types
  db/                              SQLite via sqlc, with migrations
    sql/                           Raw SQL queries (consumed by sqlc)
    migrations/                    Schema migrations
  lsp/                             LSP client manager, auto-discovery, on-demand startup
  ui/                              Bubble Tea v2 TUI (see internal/ui/AGENTS.md)
  permission/                      Tool permission checking and allow-lists
  skills/                          Skill file discovery and loading
  shell/                           Bash command execution with background job support
  pubsub/                          Internal pub/sub for cross-component messaging
  filetracker/                     Tracks files touched per session
  history/                         Prompt history
```

### Key Dependency Roles

- **`charm.land/fantasy`**: LLM provider abstraction layer. Handles protocol
  differences between Anthropic, OpenAI, Gemini, etc. Used in `internal/app`
  and `internal/agent`.
- **`charm.land/bubbletea/v2`**: TUI framework powering the interactive UI.
- **`charm.land/lipgloss/v2`**: Terminal styling.
- **`charm.land/glamour/v2`**: Markdown rendering in the terminal.
- **`charm.land/catwalk`**: Catalog of providers and models (`catwalk.Provider`),
  loaded into `internal/config/store.go`.
- **`sqlc`**: Generates Go code from SQL queries in `internal/db/sql/`.

### Key Patterns

- **Config is a store, not global state**: loaded once into a
  `*config.ConfigStore` and read through its published `*config.Config`
  snapshots (`store.Config()`); mutators clone-and-swap rather than
  editing the live value.
- **Tools are self-documenting**: each tool has a `.go` implementation and a
  description file in `internal/agent/tools/` - a plain `.md` for a static
  description, or a `.md.tpl` (more of them than plain `.md` files) when the
  description depends on config, such as a tool's own limits.
- **Tool failures: text response vs. Go error**: a model-recoverable failure
  (bad or missing argument, invalid enum value, a target URL/ID that doesn't
  resolve) returns `fantasy.NewTextErrorResponse(...)` so the model sees it
  as a normal tool result and can retry; an infrastructure failure (missing
  session ID, disk/network faults in Sennit's own I/O, a wiring bug) returns
  a `%w`-wrapped Go `error`, which aborts the whole tool-call batch instead
  of continuing the conversation. Use `invalidParam(name)` for the "X is
  required" case and `missingSessionID(action)` for the missing-session-ID
  case so the wording stays consistent (`internal/agent/tools/tools.go`).
- **Proto boundary**: `internal/proto` may alias leaf data types with no
  behavior or transitive dependencies (for example, `message`, `session`, and
  `skills`). Types with behavior, runtime references, or transitive
  dependencies require explicit DTOs in `proto`, with narrowly scoped
  conversion at the boundary. Do not add a general conversion framework.
  - **There is no remote transport in this tree.** `proto` was shaped for a
    client/server split that no longer exists: every `Workspace`
    implementation is in-process, and events reach the TUI through
    `internal/pubsub` channels, not over a wire. Read any "wire contract"
    language in `proto`'s own comments as historical. Audited 2026-09-02.
  - `proto.Thread` is the one live DTO. It is a real struct, named
    throughout `Workspace`'s thread methods, and
    `internal/app/threadspawn/protoconv.go` converts `thread.Thread` into
    it for `internal/workspace/appws`. Keep it.
  - `tools.*PermissionsParams` **stays aliased, but the direction is
    `proto` → `tools`, not the other way round.** These structs are defined
    in `proto` (a leaf package with a light dependency graph) and
    `internal/agent/tools` aliases them back, e.g.
    `type BashPermissionsParams = proto.BashPermissionsParams`. That
    direction was chosen (2026-08-28) so `proto` no longer has to import
    `tools` — the last remaining path by which `internal/ui` linked
    `internal/db` — while the permission dialog still holds a
    `permission.PermissionRequest` built in-process and asserts on
    `proto.*PermissionsParams`, which succeeds only because the alias makes
    it the same Go type as the `tools.*` value the agent constructed. The
    alias is load-bearing for **type identity**: replacing it with a
    look-alike struct copy would break the dialog's per-tool rendering (see
    `ui/dialog/permissions.go`'s registry and
    `TestDiffContentRenderer_GuardStopsBeforeToDiff`). Compile-time identity
    assertions in `internal/agent/tools/proto_identity_test.go` enforce it.
  - The dead types this section used to list — `proto.Message`,
    `proto.RunComplete`, `proto.AgentEvent`, `proto.PermissionRequest`,
    `proto.PermissionNotification`, `proto.ConfigProviderKeyRequest` — and
    `proto/permission.go` along with them, **have been deleted**. The
    `case` arms in `internal/herdr/translate.go` now match on the domain
    types (`message.Message`, `notify.RunComplete`,
    `permission.PermissionRequest`, `permission.PermissionNotification`),
    and none of them is unreachable.
  - `proto.LSPClientInfo` is a live frontend DTO. `workspace.LSPClientInfo`
    aliases it, and `internal/workspace/appws` converts runtime LSP state
    into this data-only shape before the UI receives it.
  - `proto.Session` was unused and has been removed. `proto.Todo` remains an
    alias of `session.Todo` in `proto/lsp.go` to preserve its type identity.
  - So: add a DTO to `proto` only for something that genuinely crosses the
    workspace/UI boundary as data, the way `proto.Thread` does. Do not add one
    because a type "might be sent somewhere" — nothing is sent anywhere.
- **System prompts are Go templates**: `internal/agent/templates/*.md.tpl`
  with runtime data injected.
- **Context files**: Sennit reads AGENTS.md, SENNIT.md, CLAUDE.md, GEMINI.md
  (and `.local` variants) from the working directory for project-specific
  instructions.
- **Bash config format**: Sennit's primary config format is `sennitrc` — a
  Bash script using builtins (`provider`, `model`, `mcp`, `lsp`,
  `permissions`, `hook`, `options`) to define config. `sennit.json` is still
  supported but is deprecated in favor of `sennitrc` and may be removed in a
  future release. Shell config files are discovered alongside JSON configs
  and deep-merged through the same pipeline. Providers and the selected model
  (`providers`, `model`, `recent_models`) are global-only: those keys are
  stripped from every project-scoped layer before the merge — see
  `internal/config/globalonly.go`. Builtins are registered via
  `shell.RegisterBuiltin` and gated by a `ConfigBuilder` on the context —
  they are no-ops during normal bash tool execution. See
  `internal/shellconfig/`.
- **Persistence**: SQLite + sqlc. All queries live in `internal/db/sql/`,
  generated code in `internal/db/`. Migrations in `internal/db/migrations/`.
- **Timestamps**: SQLite timestamps are Unix seconds (`strftime('%s','now')`);
  `Finish.Time` is seconds. Exception: `StartTime`/`EndTime` in the bash tool
  are milliseconds.
- **Pub/sub**: `internal/pubsub` for decoupled communication between agent,
  UI, and services.
- **Hooks**: User-defined shell commands in `sennitrc` (or `sennit.json`)
  that fire before tool execution. The engine (`internal/hooks/`) is
  independent of fantasy and agent — it takes inputs, runs commands,
  returns decisions. The `hookedTool` decorator in
  `internal/agent/hooked_tool.go` wraps tools at the coordinator level.
  Hooks run before permission checks. See `HOOKS.md` for the user-facing
  protocol.
- **CGO disabled**: builds with `CGO_ENABLED=0` and
  `GOEXPERIMENT=greenteagc`.

## Build/Test/Lint Commands

- **Build**: `go build .` or `go run .`
- **Test**: `task test` or `go test ./...` (run single test:
  `go test ./internal/agent/prompt -run TestNewPrompt`)
- **Update Golden Files**: `go test ./... -update` (regenerates `.golden`
  files when test output changes)
  - Update specific package:
    `go test ./internal/ui/model -update` (in this case, we're updating
    the TUI model's golden files)
- **Lint**: `task lint:fix`
- **Format**: `task fmt` (`gofumpt -w .`)
- **Modernize**: `task modernize` (runs `modernize` which makes code
  simplifications)
- **Dev**: `task dev` (runs with profiling enabled)

## Code Style Guidelines

- **Imports**: Use `goimports` formatting, group stdlib, external, internal
  packages.
- **Formatting**: Use gofumpt (stricter than gofmt), enabled in
  golangci-lint.
- **Naming**: Standard Go conventions — PascalCase for exported, camelCase
  for unexported.
- **Types**: Prefer explicit types, use type aliases for clarity (e.g.,
  `type AgentName string`).
- **Error handling**: Return errors explicitly, use `fmt.Errorf` for
  wrapping.
- **Context**: Always pass `context.Context` as first parameter for
  operations.
- **Interfaces**: Define interfaces in consuming packages, keep them small
  and focused.
- **Structs**: Use struct embedding for composition, group related fields.
- **Constants**: Use typed constants with iota for enums, group in const
  blocks.
- **Testing**: Use testify's `require` package, parallel tests with
  `t.Parallel()`, `t.SetEnv()` to set environment variables. Always use
  `t.Tempdir()` when in need of a temporary directory. This directory does
  not need to be removed.
- **JSON tags**: Use snake_case for JSON field names.
- **File permissions**: Use octal notation (0o755, 0o644) for file
  permissions.
- **Log messages**: Log messages must start with a capital letter (e.g.,
  "Failed to save session" not "failed to save session").
  - This is enforced by `task lint:log` which runs as part of `task lint`.
- **Comments**: End comments in periods unless comments are at the end of the
  line.

## Testing without real providers

Tests must never reach a real provider. Point the config loader at a
throwaway global directory and give it a provider that exists only for the
test — `disable_default_providers` keeps the embedded catalog out of it,
so nothing resolves to a real endpoint:

```go
writeGlobalConfig(t, `{
  "options": {"disable_default_providers": true},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}
}`)

cfg, err := config.Load(workingDir, "", false)
```

See `internal/agent/common_test.go` for the helper and its callers. (The
`config.UseMockProviders` / `config.ResetProviders` globals this section
used to describe do not exist.)

## Formatting

- ALWAYS format any Go code you write.
  - First, try `gofumpt -w .`.
  - If `gofumpt` is not available, use `goimports`.
  - If `goimports` is not available, use `gofmt`.
  - You can also use `task fmt` to run `gofumpt -w .` on the entire project,
    as long as `gofumpt` is on the `PATH`.

## Comments

- Comments that live on their own lines should start with capital letters and
  end with periods. Wrap comments at 78 columns.

## Committing

- ALWAYS use semantic commits (`fix:`, `feat:`, `chore:`, `refactor:`,
  `docs:`, `sec:`, etc).
- Try to keep commits to one line, not including your attribution. Only use
  multi-line commits when additional context is truly necessary.

## Working on the TUI (UI)

Anytime you need to work on the TUI, read `internal/ui/AGENTS.md` before
starting work.

## Styling System

The styling system lives in `internal/ui/styles/` and is organized into
three layers:

- **`quickstyle.go`**: The stable base theme builder. `quickStyle(opts)`
  constructs a `Styles` struct from `quickStyleOpts` — a palette of
  design tokens (primary, secondary, fgBase, bgBase, success, error, etc.).
  `quickStyle` must be fully token-driven: never hardcode specific
  `charmtone.*` colors here (except Chroma syntax highlighting, which is
  pending tokenization). This lets any theme reuse the base without
  inheriting Charmtone-specific colors.
- **`themes.go`**: Defines concrete themes. Each theme function (e.g.
  `CharmtonePantera`) calls `quickStyle` with its palette, then applies
  theme-specific overrides as needed.
- **`styles.go`**: Defines the `Styles` struct and its documentation —
  the shape of what `quickStyle` produces.

**Adding theme-specific overrides**: When a style genuinely needs a
color that doesn't fit the token model (e.g. the bang prompt uses
Salt/Hazy/Larple), keep `quickStyle` on the closest semantic token and
override only the differing colors in the theme function:

```go
func CharmtonePantera() Styles {
	s := quickStyle(quickStyleOpts{ /* palette */ })

	// Override only the colors that differ from the token defaults.
	s.Editor.PromptBangIconFocused = s.Editor.PromptBangIconFocused.
		Foreground(charmtone.Salt).
		Background(charmtone.Hazy)

	return s
}
```

**Adding a new theme**: Add a function in `themes.go` that returns the
result of `quickStyle` with a `quickStyleOpts` palette (plus any needed
overrides), then wire it into `ThemeForProvider`.
- Pre-commit hook: `git config core.hooksPath .githooks` (fmt, tidy, build, lint; full -race suite runs in CI only).
