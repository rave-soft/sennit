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

## Git

| Tool | Does | Read-only |
|:--|:--|:--|
| `git_status` | Read current worktree status with relative path filters and bounded results | yes |
| `git_diff` | Read staged, unstaged, or revision diffs with patch/stat output and a byte limit | yes |
| `git_log` | Read commit history with revision and relative path filters | yes |

These tools execute fixed git argv directly in the active worktree; they do not accept a repository path or invoke a shell.

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
| `lsp_workspace_symbols` | Search symbols across configured workspace language servers | yes |
| `lsp_hover` | Read type, signature, and documentation at a position or symbol | yes |
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
| `agentic_fetch` | Fetch and have a subagent extract or analyse the content | |
| `download` | Stream a URL straight to a file, binary-safe. Overwrites without warning | |

"Read-only" here means it doesn't modify local state; the network call still
goes through the normal permission flow.

## Delegation

See [Steering, tasks and threads](../concepts/delegation.md).

| Tool | Does |
|:--|:--|
| `agent` | Delegate to a subagent. `subagent_type` names one from `.sennit/agents/`; omit it for the general-purpose agent |
| `agent_list` | Every delegation you can act on — your background tasks, and the workspace's threads |
| `agent_result` | A delegation's status, and its final answer once finished |
| `agent_output` | A background task's transcript so far, without waiting |
| `agent_send` | Send a follow-up into a delegation's session |
| `agent_cancel` | Stop a running delegation |
| `ask_parent` | Message the session that created this delegation, waking it if idle |

The `agent_*` tools take a task's id or a thread's id or name, so one set
addresses both kinds. The older per-kind names (`task_list`, `thread_send`,
and the rest) still resolve to them in `tools:` lists and permission
configs.

They disappear when `options.background_agents` is `false`.

## Threads

| Tool | Does |
|:--|:--|
| `thread_create` | Create a thread: its own git worktree, branch, data directory and session |
| `thread_merge` | Merge a thread's branch into its base |
| `thread_remove` | Cancel it, remove the worktree, delete the record |

Listing, inspecting, steering and stopping a thread are the `agent_*` tools
above; only the worktree lifecycle is thread-specific. `agent_output` is the
one that is not: a thread's transcript lives in its own worktree session and
is not readable from the parent workspace.

> [!NOTE]
> A thread that is mid-turn does not read a follow-up until that turn ends —
> an agent inside a long sub-agent call can be minutes away from it — so
> `agent_send` cannot steer or time-box work already in flight. Its result
> says whether the message runs next or is waiting behind the turn in flight;
> read it rather than assuming delivery. Sending to a thread from the TUI's
> thread view is a separate path.

> [!NOTE]
> A thread's completion arrives on its own through the parent's completion
> inbox. Use `agent_result` to inspect an individual result; there is no
> blocking wait tool. When several threads are involved, inspect their statuses
> and continue when the corresponding completion messages arrive.

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
| `agent_trace` | Read a redacted lifecycle trace and aggregate outcomes for a session or run |

## The read-only set

These are the tools Sennit itself treats as read-only (`toolmeta.TaskReadOnlyNames`),
and a sensible thing to allow without prompting:

```bash
permissions allow read multi_read ls glob grep ripgrep \
  git_status git_diff git_log \
  lsp_definition lsp_symbols lsp_workspace_symbols lsp_hover lsp_call_hierarchy \
  fetch web_fetch web_search agent_trace
```

> [!WARNING]
> `read`, `ls`, `glob`, `grep`, and `ripgrep` only prompt when the path they're
> given falls outside your working directory — inside it, they never ask.
> Allowing them removes that one remaining check, so the model can read any
> file on disk your user can (`~/.ssh`, a sibling repository, ...) without
> being asked again. See [Permissions](../configuration/permissions.md).

## Turning tools off

```bash
permissions deny bash download
```

A denied tool is not offered to the model at all, rather than being refused when
called. `sennit doctor` flags a name that matches no known tool, so a typo
doesn't silently do nothing.
