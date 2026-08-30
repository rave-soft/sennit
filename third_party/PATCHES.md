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
inferred from commit stats. Ten source files differed as of the last
count; `providers/kronk/language_model.go`,
`providers/openrouter/language_model_hooks.go` and
`providers/azure/azure.go` have since gained local fixes too (see below)
and are not reflected in that count.

| File | Change |
|---|---|
| `agent.go`, `tool.go` | `InputSchema` on `FunctionTool`/`ToolInfo`, threaded through tool conversion so a complete JSON Schema survives the round-trip instead of being flattened to `properties`/`required`. `prepareTools` (`agent.go`) now calls `schema.Normalize` unconditionally after building `InputSchema` (clone or fallback) instead of only on the legacy fallback path - the `InputSchema` patch above had made `Normalize` dead code for every tool that supplies its own schema (Sennit's MCP tools always do), so a `type: array` property with no `items`, or a nullable `type` array, reached OpenAI/Codex verbatim and was rejected. |
| `retry.go` | `RetryOptions.OnRateLimit`: on the first 429 in a pass, the caller may rotate credentials and retry immediately instead of sleeping out the backoff. |
| `agent.go` | Plumbs `OnRateLimit` through `AgentCall`/`AgentStreamCall` so the public agent API can supply the hook. |
| `errors.go` | `IsTransientStreamError(errType, message)` - classifies an untyped SSE error envelope by message text. The ChatGPT/Codex backend sends errors with no `type`/`code`, so type-keyed classification never matched and a transient overload failed after one attempt. |
| `providers/anthropic/error.go`, `providers/openai/error.go`, `providers/openai/responses_language_model.go` | Route stream-error classification through `IsTransientStreamError`. `providers/anthropic/error.go` and `providers/openai/error.go`'s `toHeaderMap` now write the lowercased key directly (`out[strings.ToLower(k)] = v[l-1]`) instead of ranging over `in` while inserting a lowercase alias back into the same map - mutating a map during `range` over it is undefined by the Go spec, so `retry.go`'s `getRetryDelayInMs` (which looks up `headers["retry-after"]`/`headers["retry-after-ms"]`, always lowercase) honored a provider's `Retry-After` on a 429 nondeterministically. |
| `providers/anthropic/anthropic.go`, `providers/openai/language_model.go` | Carry the fuller tool schema (`InputSchema`, see `agent.go`/`tool.go` above) through to each provider's own request shape. `anthropic.go`'s `required` handling (around line 709) now also accepts a `[]any`-typed `required` field, not just `[]string` - `InputSchema` round-trips through `encoding/json`, which decodes a JSON array into `[]any` rather than `[]string`, so an MCP tool's schema silently lost its `required` list under the old type-switch. |
| `providers/google/google.go` | Route stream-error classification through `IsTransientStreamError`. Its stream loop (and `streamObjectWithJSONMode`) now overwrites usage with the latest `mapUsage(resp.UsageMetadata)` each chunk instead of summing `CacheReadTokens`/`OutputTokens`/`ReasoningTokens` across chunks - Gemini's `usageMetadata` is cumulative per chunk (`cachedContentTokenCount` is a prompt-side constant repeated in every chunk), so summing multiplied cached/output tokens by the chunk count. |
| `providers/azure/azure.go` | `New` now returns an error when no base URL is configured, instead of falling through to `openai.New`'s `cmp.Or(baseURL, DefaultURL)` fallback - an Azure provider with no base URL silently sent its `api-key` header to `https://api.openai.com/v1`, and the resulting 401 read to the user as a bad key rather than a missing endpoint. |
| `providers/kronk/language_model.go` | `Stream` now reads `resp.Usage`/`choice.FinishReason()` before the `choice.Delta == nil` guard, not after - kronk only sets `Delta` on chunks carrying tool calls, so a plain "stop" final chunk (`Delta == nil`, `Usage` set, `FinishReason` "stop") previously skipped both and every streamed text-only answer finished with zero usage and `FinishReasonUnknown`. `Generate` needed the same Delta-nil-vs-FinishReason restructure to track it correctly. Both `Generate` and `Stream` also now track whether a finish reason was ever seen and return `ctx.Err()` or `fantasy.NewIncompleteStreamError()` if the channel closes without one (e.g. cancellation mid-stream), matching the openai/anthropic adapters - previously channel closure was always treated as a successful, complete turn. |
| `providers/openrouter/language_model_hooks.go` | Two fixes. (1) `languageModelExtraContent`'s `responsesReasoningBlocks`/`googleReasoningBlocks` accumulation now grows the slice to `detail.Index+1` before indexing into it, instead of appending exactly one element regardless of the actual gap - a non-contiguous `reasoning_details[].index` (a first detail already at index 1, or a gap) indexed past the slice's length and panicked inside `Generate`. (2) `languageModelStreamExtra` now allocates `currentReasoningState.metadata`/`googleMetadata` lazily, right before each is first written, instead of assuming the Reasoning Start block already set the one matching field - that block only allocates the field for the *first* detail's own format, so a stream that starts on a different format (e.g. `anthropic-claude`) and only later switches to `openai-responses` or `google-gemini` dereferenced the still-nil field and panicked mid-stream. |
| `providers/vercel/language_model_hooks.go` | Four fixes. (1) `languageModelExtraContent`'s `responsesReasoningBlocks`/`googleReasoningBlocks` accumulation grows the slice to `detail.Index+1` before indexing, same bug and same fix as the openrouter patch above - both files should read the same. (2) `languagePrepareModelCall`'s BYOK handling now reaches through to the nested `extraFields["providerOptions"]["gateway"]` map and writes `byok` there (creating the nested map if the outer one exists but has no `gateway` key yet), instead of writing `byok` directly onto the outer `providerOptions` map when routing options were also set - that silently misplaced BYOK credentials outside Vercel's documented `providerOptions.gateway.byok` shape, so a user who set both routing options and BYOK had their credentials ignored and was billed through gateway credits instead. (3) `languageModelToPrompt`'s tool-result switch now handles `ToolResultContentTypeMedia` (routing through `openaipkg.ToolResultMediaMessages`, with the same `cache_control` handling the other arms apply) plus a `default:` warning arm, mirroring the openai provider's default - previously a media tool result (e.g. an image-returning MCP tool) fell through with no message and no warning, leaving an assistant `tool_calls` entry with no matching `tool` message and getting the whole conversation rejected with a 400 on the next turn. (4) `languageModelStreamExtra` now allocates `currentReasoningState.metadata`/`googleMetadata` lazily, right before each is first written, instead of assuming the Reasoning Start block already set them - that block only allocates a field when the *first* reasoning chunk's own `reasoning_details` matched it, so a start chunk carrying only the bare `reasoning` string (no `reasoning_details` at all) left both nil, and a later chunk carrying an `openai-responses` or `google-gemini` detail dereferenced the still-nil field and panicked mid-stream. Same fix, same shape, as the openrouter patch above. |

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
`retry_ratelimit_test.go`, `schema_transport_test.go`,
`providers/anthropic/schema_transport_test.go`,
`providers/google/schema_transport_test.go`,
`providers/openai/schema_transport_test.go`,
`providers/openai/error_transient_test.go`,
`providers/openai/error_headermap_test.go`,
`providers/vercel/language_model_hooks_test.go`,
`providers/openrouter/language_model_hooks_test.go`) so they never
collide with upstream's. This previously wasn't true of
`providers/openai/error_test.go`, which had accumulated a local test
(`TestToHeaderMap_LowercasesKeysDeterministically`) directly in the
upstream file; it has since moved to `error_headermap_test.go` above so
the invariant holds again.

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
pre-modified in its vendoring commit. Two files differed as of that
count; `pkg/transport/router.go` has since gained a local fix too (see
below) and is not reflected in that count:

| File | Change |
|---|---|
| `pkg/lsp/client.go` | Adds `MethodWorkspaceSymbol` and `Client.RequestWorkspaceSymbols`, which upstream v0.1.6 does not have. |
| `pkg/lsp/protocol/tsjson.go` | Fixes the `workspace/symbol` union decode. The result may be legacy `SymbolInformation` or modern `WorkspaceSymbol`; the generated code tried `SymbolInformation` first, and since the struct does not require `Range`, it wrongly accepted modern URI-only entries. Now it probes for a missing `location.range` first. |
| `pkg/transport/router.go` | `Route` now dereferences `req.Params` only when non-nil (`jsonrpc2.Request.Params` is a `*json.RawMessage`, legal to be nil for a request/notification with no params, e.g. `workspace/workspaceFolders`) instead of unconditionally `*req.Params`, which panicked the whole process with no recover in `jsonrpc2.Conn.readMessages`. Notification detection now branches on `req.Notif` instead of `req.ID == (jsonrpc2.ID{})`, which misclassified a server request with numeric id `0` as a notification (no handler ran, and `HandlerWithError` then replied `null`). |

Sennit's own test for the `router.go` fix lives in `pkg/transport/router_test.go`, a file of its own added by Sennit (not part of the powernap baseline).

`LICENSE` is kept in the tree; upstream carries it at the monorepo root,
not inside `powernap/`.
