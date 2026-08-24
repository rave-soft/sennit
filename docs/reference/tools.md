# Built-in tools

Every tool the agent can be given. These names are what you write in
[`permissions allow`/`deny`](../configuration/permissions.md),
in an [agent's `tools:` list](../extending/agents.md), and in
a [hook's `--matcher`](../extending/hooks.md).

## Files

| Tool | Does | Read-only |
|:--|:--|:--|
| `read` | Read a file with line numbers; supports offset and limit; renders PNG, JPEG, GIF and WebP | yes |
| `multi_read` | Read up to 20 file ranges sequentially with per-file status and one shared 200 KB response budget | yes |
| `write` | Create or overwrite a file, creating parent directories. Cannot append | |
| `edit` | Exact find-and-replace in one file. Tolerates whitespace-only mismatches by re-indenting to the file's style, and says when it did | |
| `multiedit` | Several find-and-replace edits to one file, applied in sequence | |
| `ls` | List files and directories as a tree; skips hidden and system directories | yes |
| `glob` | Find files by name pattern, newest first | yes |
| `grep` | Search file contents by regex or literal; respects `.gitignore` | yes |
| `ripgrep` | The same via ripgrep, with Rust regex syntax; respects `.gitignore` and `.sennitignore` | yes |

> [!NOTE]
> `read` was called `view` in older configs. The old name is still accepted
> and folded onto `read`, so an existing `permissions allow view` keeps
> working.

## Shell

| Tool | Does |
|:--|:--|
| `bash` | Run a shell command. Long-running commands move to the background and return a shell ID |
| `job_output` | Read stdout/stderr from a background shell; can block until it finishes |
| `job_kill` | Terminate a background shell |

## Language servers

Accurate where grep is approximate. See
[Language servers](../extending/lsp.md).

| Tool | Does | Read-only |
|:--|:--|:--|
| `lsp_definition` | Where a symbol is defined | yes |
| `lsp_references` | Every use of a symbol | |
| `lsp_symbols` | Outline of a file's symbols | yes |
| `lsp_call_hierarchy` | Callers and callees of a symbol | yes |
| `lsp_rename` | Semantic rename across files | |
| `lsp_replace_symbol` | Replace, insert or delete a whole symbol by name | |
| `lsp_diagnostics` | Errors, warnings and hints for a file or the project | |
| `lsp_restart` | Restart one or all LSP clients | |

## Network

| Tool | Does | Read-only |
|:--|:--|:--|
| `web_search` | Search the web; returns titles, URLs and snippets | yes |
| `web_fetch` | Fetch a URL as markdown; pages over 50 KB are saved to a temp file for `grep`/`read` | yes |
| `fetch` | Fetch raw content as text, markdown or HTML, without processing | yes |
| `agentic_fetch` | Fetch and have a subagent extract or analyse the content | yes |
| `download` | Stream a URL straight to a file, binary-safe. Overwrites without warning | |

"Read-only" here means it doesn't modify local state; the network call still
goes through the normal permission flow.

## Delegation

See [Steering, tasks and threads](../concepts/delegation.md).

| Tool | Does |
|:--|:--|
| `agent` | Delegate to a subagent — anonymous, or a named one from `.sennit/agents/`. Its `background` parameter starts a background task |
| `task_list` | Every background task in this workspace, with status and goal |
| `task_result` | A task's status, and its final answer once finished |
| `task_output` | A task's transcript so far, without waiting |
| `task_send` | Send a follow-up into a task's session |
| `task_cancel` | Stop a running task |
| `ask_parent` | Message the session that created this delegation, waking it if idle |

Each user-defined agent also registers under its own name.

The `task_*` tools disappear when `options.background_agents` is `false`.

## Threads

| Tool | Does |
|:--|:--|
| `thread_create` | Create a thread: its own git worktree, branch, data directory and session |
| `thread_list` | Every thread, with status, branch and summary |
| `thread_status` | One thread's result, error, and merge conflicts |
| `thread_send` | Queue a follow-up prompt for a thread, reporting whether it runs next or waits behind the turn in flight |
| `thread_merge` | Merge a thread's branch into its base |
| `thread_remove` | Cancel it, remove the worktree, delete the record |
| `thread_wait` | Block until the given threads settle |

> [!NOTE]
> `thread_send` is **not** in the default tool set. A thread that is mid-turn
> does not read a follow-up until that turn ends — an agent inside a long
> sub-agent call can be minutes away from it — so steering or time-boxing a
> running thread, the tool's most tempting use, is the one thing it cannot do.
> Enable it by naming it in an agent's `tools:` list when you do need it (for
> resuming an `interrupted` thread, or driving conflict resolution inside a
> thread's worktree). When enabled, its result says whether the message runs
> next or is waiting behind the turn in flight. Sending to a thread from the
> TUI's thread view is a separate path and is always available.

> [!NOTE]
> A thread's completion arrives on its own — the agent is woken with it as
> each thread settles — so waiting is rarely needed at all. `thread_wait` is
> in the default set for the case that is not covered that way: holding work
> until *several* threads have all settled together, before merging or
> reviewing their combined output. Without it, an agent in that position falls
> back to sleeping in `bash`, which costs a turn and tells it nothing. The
> wait gives up after ten minutes unless `timeout_seconds` says otherwise
> (negative for no timeout), and a message from the user ends it early.

## MCP

| Tool | Does |
|:--|:--|
| `list_mcp_resources` | Resource URIs available from a named MCP server |
| `read_mcp_resource` | Read one resource by URI |

Tools contributed by a server are named `mcp_<server>_<tool>` and can be
allowed, denied or matched by a hook under that full name.

## Interaction and state

| Tool | Does |
|:--|:--|
| `question` | Ask you a structured question and wait — clarification, confirmation, a choice |
| `todos` | Maintain the visible task list for multi-step work. Every call replaces the whole list |
| `sennit_info` | Sennit's runtime state: model, provider, LSP/MCP status, skills, hooks, permissions, disabled tools |
| `sennit_logs` | Read Sennit's own logs — for diagnosing provider errors, tool failures, LSP/MCP problems |

## The read-only set

These are the tools Sennit itself treats as read-only, and a sensible thing to
allow without prompting:

```bash
permissions allow read ls glob grep ripgrep \
  lsp_definition lsp_symbols lsp_call_hierarchy \
  fetch web_fetch web_search
```

## Turning tools off

```bash
permissions deny bash download
```

A denied tool is not offered to the model at all, rather than being refused when
called. `sennit doctor` flags a name that matches no known tool, so a typo
doesn't silently do nothing.
