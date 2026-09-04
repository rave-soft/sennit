# MCP servers

The Model Context Protocol connects Sennit to external tool servers. An MCP
server contributes tools, resources and prompts; Sennit exposes its tools to
the agent as `mcp_<server>_<tool>`, its prompts as `/` commands, and its
resources through the `list_mcp_resources` and `read_mcp_resource` tools.

## Adding one

Three transports: `stdio` (a local process), `http`, and `sse`.

```bash
# A local process over stdio — the default type
mcp add filesystem \
  --command npx \
  --args -y --args @modelcontextprotocol/server-filesystem --args "$HOME/notes"

# A remote HTTP server
mcp add github \
  --type http \
  --url "https://api.githubcopilot.com/mcp/" \
  --header Authorization "Bearer $GH_PAT"
```

`--args` and `--header` and `--env` are repeatable, once per value. A header
whose value resolves to the empty string is dropped rather than sent empty, so
an env-gated header is safe to leave in a shared config.

## Narrowing what a server offers

A server with forty tools costs forty tool descriptions in context on every
turn. Trim it:

```bash
mcp add github --type http --url "…" \
  --enabled-tools search_issues \
  --enabled-tools get_issue \
  --enabled-tools create_issue
```

`--enabled-tools` is an allowlist — only those tools are offered.
`--disabled-tools` is the subtractive form. Both are repeatable.

To turn a server off without deleting its configuration:

```bash
mcp add github --disabled true
```

## OAuth

HTTP servers that authenticate with OAuth 2.1 need no manual token handling:

```bash
mcp add linear --type http --url "https://mcp.linear.app/mcp" --oauth true
```

Sennit runs the flow on first connect and stores the token. For a
pre-registered client, or a server that needs a fixed redirect port:

```bash
mcp add internal --type http --url "https://mcp.example.com/" \
  --oauth true \
  --oauth-client-id "$MCP_CLIENT_ID" \
  --oauth-client-secret "$MCP_CLIENT_SECRET" \
  --oauth-callback-port 8976
```

Removing a server also drops its stored token.

## Startup and failure

`--timeout <seconds>` bounds how long Sennit waits for a server to come up. A
server that fails to start does not block the session — it is reported and the
rest continue. Connection health is only observable from a running session:
`sennit doctor` never starts MCP servers, so use the TUI's `/doctor` command or
the `sennit_info` tool to see actual connection state.

For an OAuth server, once the browser step is reached `--timeout` no longer
applies: Sennit sets it aside and waits up to a fixed 5 minutes for the
login (and any second factor or approval step) to complete at the
localhost redirect. That five-minute wait is not configurable.

## The Docker MCP catalog

If Docker's MCP toolkit is available on your machine, Sennit offers to enable
its catalog — the **enable docker mcp** entry in the command palette, and
**disable docker mcp** to turn it back off. That is a convenience wrapper
around the same MCP mechanism, stored in your global config.

## Prompts and resources

Prompts a server exposes appear in the `/` command list as
`<server>:<prompt>`, with their declared arguments prompted for. See
[Custom commands](commands.md).

Resources are reachable through two built-in tools rather than being loaded
eagerly: `list_mcp_resources` enumerates them and `read_mcp_resource` fetches
one. Deny those two tools to keep resources out of reach entirely.

## Permissions

MCP tools go through the same permission flow as built-in ones, under their
full `mcp_<server>_<tool>` name:

```bash
permissions allow mcp_github_search_issues
permissions deny mcp_github_create_issue
```

A [hook](hooks.md) with a matcher can gate a whole server at once:

```bash
hook add PreToolUse --matcher "^mcp_" --command "./hooks/log-mcp.sh"
```

> [!WARNING]
> An MCP server is code you are running and a channel the agent can send your
> data down. Treat adding one like adding a dependency: know what it is, and
> prefer `--enabled-tools` over handing over the whole surface.
