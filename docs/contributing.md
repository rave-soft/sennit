# Contributing

Sennit is Go, and the repository is
[rave-soft/sennit](https://github.com/rave-soft/sennit).

## Building

Go 1.26 or newer.

```sh
git clone https://github.com/rave-soft/sennit.git
cd sennit
go build -o sennit .
```

There is a `Makefile` and a `Taskfile.yaml`; either works.

```sh
make build          # or: task build
make test           # or: task test
make race           # tests under the race detector
make lint           # golangci-lint
make fmt            # or: task fmt
make schema         # regenerate schema.json from the code
make check          # tidy + build + lint + schema-check
make ci             # everything CI runs
```

`make help` lists the rest. A Nix flake is provided for a pinned toolchain.

## Layout

```
main.go             entry point
internal/agent/     the agent loop, delegation, tools
  tools/            every built-in tool, with its description in a .md beside it
internal/config/    config loading, agents, skills discovery, providers
internal/shellconfig/ the sennitrc builtin commands
internal/ui/        the TUI
internal/lsp/       language server clients
internal/db/        SQLite storage (sqlc-generated)
internal/hooks/     hook execution
docs/               the source for several pages on this site
```

A tool's description lives in a `.md` or `.md.tpl` file next to its
implementation, which is what makes the [tools reference](reference/tools.md)
checkable against the code.

## Conventions

`AGENTS.md` in the repository root is the working agreement — build commands,
layout, and the conventions the codebase actually follows. Read it before a
first change; it is also what Sennit itself loads as context when working on
its own source.

Notable ones:

- `schema.json` is generated. Change the struct tags in `internal/config`, then
  run `make schema`. `make schema-check` fails CI if it is stale.
- Database queries are `sqlc`-generated; edit the `.sql` files and run
  `task sqlc`.
- `internal/config` must not import `internal/agent/tools` — that would cycle.
  Tool names are duplicated as literals where needed, deliberately.

## Documentation

Some pages on this site are imported from `docs/` on `main` with only front
matter and link fixes applied:

| Site page | Source |
|:--|:--|
| [sennitrc reference](configuration/sennitrc.md) | `docs/config/README.md` |
| [Hooks](extending/hooks.md) | `docs/hooks/README.md` |
| [Steering, tasks and threads](concepts/delegation.md) | `docs/delegation/README.md` |

Edit those on `main` and re-import, rather than editing both copies.

Everything else lives only on the `gh-pages` branch, which is an orphan branch
holding just the site. It shares no history with `main` and must never be
merged in either direction.

```sh
git worktree add ../sennit-gh-pages gh-pages
cd ../sennit-gh-pages
bundle install
bundle exec jekyll serve --livereload
```

The site is Jekyll with the [just-the-docs](https://just-the-docs.com) remote
theme, built by GitHub Pages itself — pushing markdown is the deploy. Pages are
ordered by `nav_order` in each file's front matter; sections are directories
whose `index.md` sets `has_children: true`.

## License

Sennit is a fork of [Crush](https://github.com/charmbracelet/crush) by
Charmbracelet, Inc. and is distributed under the same license, the Functional
Source License 1.1 with MIT Future License (FSL-1.1-MIT). Contributions are
made under that license. See
[`LICENSE.md`](https://github.com/rave-soft/sennit/blob/main/LICENSE.md) and
[`NOTICE`](https://github.com/rave-soft/sennit/blob/main/NOTICE).
