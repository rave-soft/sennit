# third_party patches

`go.mod` replaces `charm.land/fantasy` and
`github.com/charmbracelet/x/powernap` with the local trees here, and
Sennit has patched real behaviour into both. Both are now **git
subtrees**, so the merge base is recorded in git history itself rather
than inferred - `git log` shows the exact upstream commit each tree was
taken from, and `git subtree pull` can do a real three-way merge.

Update this file whenever a commit touches `third_party/`.

## Module: charm.land/fantasy

- Upstream: https://github.com/charmbracelet/fantasy (Apache-2.0).
- Baseline: **v0.40.0**, upstream commit `58d4a10d`.
- Recorded by: `git log --grep="git-subtree-dir: third_party/fantasy"`.

### Upgrading

```
git subtree pull --prefix=third_party/fantasy \
    https://github.com/charmbracelet/fantasy v0.41.3 --squash
```

Conflicts will land in the files listed below; resolve them keeping the
local behaviour unless upstream has since implemented the same thing.

### Local patches

Verified by diffing this tree against a real v0.40.0 checkout, not
inferred from commit stats. Ten source files differ:

| File | Change |
|---|---|
| `agent.go`, `tool.go` | `InputSchema` on `FunctionTool`/`ToolInfo`, threaded through tool conversion so a complete JSON Schema survives the round-trip instead of being flattened to `properties`/`required`. |
| `retry.go` | `RetryOptions.OnRateLimit`: on the first 429 in a pass, the caller may rotate credentials and retry immediately instead of sleeping out the backoff. |
| `agent.go` | Plumbs `OnRateLimit` through `AgentCall`/`AgentStreamCall` so the public agent API can supply the hook. |
| `errors.go` | `IsTransientStreamError(errType, message)` - classifies an untyped SSE error envelope by message text. The ChatGPT/Codex backend sends errors with no `type`/`code`, so type-keyed classification never matched and a transient overload failed after one attempt. |
| `providers/anthropic/{anthropic,error}.go`, `providers/openai/{error,language_model,responses_language_model}.go`, `providers/google/google.go` | Route stream-error classification through `IsTransientStreamError`, and carry the fuller tool schema. |

Corrections to the previous version of this file, which was written
before a real baseline existed: `content.go`, `content_json.go`,
`object/object.go` and `provider_registry.go` were listed as patched.
They are **identical to upstream** - they appeared only because the
vendoring commit added every file at once, so `git show --stat` listed
them all.

### Divergences from upstream tests

Upstream's own tests are vendored now, and four subtests in
`providers/openai/openai_test.go` assert that a mid-stream
`server_error` is **not** retryable. That is exactly the contract the
`IsTransientStreamError` patch changes, so their expectations are
adapted (via `requireRetryableProviderError`) rather than deleted -
whoever merges a new upstream version needs to see this conflict, not
have it hidden. `providers/anthropic/anthropic_test.go` carries a
one-line gofmt fix so it passes this repo's pre-commit hook.

`providertests/` is deliberately **not** vendored: it is upstream's VCR
integration suite, needs real provider API keys
(`FANTASY_AZURE_API_KEY` and friends), and hangs without them. A
`subtree pull` therefore reports modify/delete conflicts for every file
under `providertests/`; resolve them by deleting, which a rebased
upgrade was verified to require and nothing else:

```
git rm -r --cached third_party/fantasy/providertests
rm -rf third_party/fantasy/providertests
```

Sennit's own tests for the patched behaviour live in files of their own
(`agent_ratelimit_test.go`, `errors_transient_test.go`,
`retry_ratelimit_test.go`, `schema_transport_test.go`) so they never
collide with upstream's.

## Module: github.com/charmbracelet/x/powernap

- Upstream: https://github.com/charmbracelet/x (MIT), subdirectory
  `powernap`.
- Baseline: tag **powernap/v0.1.6**, upstream monorepo commit
  `009e633`.
- Recorded by: `git log --grep="git-subtree-dir: third_party/powernap"`.

powernap is a subdirectory of a monorepo, so it cannot be pulled
directly. The subtree was created from a synthetic history produced by
`git subtree split`, and an upgrade has to repeat that step:

```
git clone https://github.com/charmbracelet/x /tmp/x
cd /tmp/x && git checkout powernap/v0.1.7
git subtree split --prefix=powernap -b powernap-only
cd <sennit> && git subtree pull --prefix=third_party/powernap \
    /tmp/x powernap-only --squash
```

The split is deterministic for a given history, so re-running it at the
old tag reproduces the recorded baseline commit.

### Local patches

The previous version of this file stated powernap had **no local
patches**. That was wrong, and only diffing against real upstream
revealed it - this repo's history could not, because the tree arrived
pre-modified in its vendoring commit. Two files differ:

| File | Change |
|---|---|
| `pkg/lsp/client.go` | Adds `MethodWorkspaceSymbol` and `Client.RequestWorkspaceSymbols`, which upstream v0.1.6 does not have. |
| `pkg/lsp/protocol/tsjson.go` | Fixes the `workspace/symbol` union decode. The result may be legacy `SymbolInformation` or modern `WorkspaceSymbol`; the generated code tried `SymbolInformation` first, and since the struct does not require `Range`, it wrongly accepted modern URI-only entries. Now it probes for a missing `location.range` first. |

`LICENSE` is kept in the tree; upstream carries it at the monorepo root,
not inside `powernap/`.
