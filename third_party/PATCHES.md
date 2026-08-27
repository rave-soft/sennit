# third_party patches

This file exists because `third_party/fantasy` and `third_party/powernap`
are not plain vendor copies: `go.mod` replaces both modules with these
local trees (lines ~203 and ~205), and Sennit has since patched real
behaviour into the fantasy copy. Nothing in the tree otherwise records
which upstream release either copy started from or what has diverged
since. Without this document, bumping either dependency means diffing
two unrelated snapshots with no recorded common ancestor - a three-way
merge with no base. This file is that base: the version each copy was
taken from (with evidence), and every local commit that changed vendored
code since.

Update this file whenever a commit touches `third_party/`.

## Module: charm.land/fantasy

- Upstream: `charm.land/fantasy` (Charmbracelet, Apache-2.0 per
  `third_party/fantasy/LICENSE` and `NOTICE`).
- Baseline version: **v0.40.0**.
- Evidence:
  - `third_party/fantasy/version.txt` embeds `0.40.0`, read by
    `third_party/fantasy/version.go` into the exported `fantasy.Version`.
    This file is part of the vendored tree itself, so it is the
    strongest signal available.
  - `go.mod`'s `require charm.land/fantasy v0.40.0` line (line 10) was
    introduced by commit `f40e7eec` ("Braid", 2026-08-09) as a normal
    module dependency, resolved from the real module proxy - there was
    no `replace` directive at that point, so this version was actually
    fetched, not just an aspirational pin.
  - The `replace charm.land/fantasy => ./third_party/fantasy` directive
    was added later, in commit `d920b3f4`, which is also the commit that
    added the entire `third_party/fantasy` tree as new files in one
    shot (`git show --stat d920b3f4` - 41 files, all additions). The
    `require` line's version did not change between `f40e7eec` and
    `d920b3f4` (`git log -p f40e7eec..d920b3f4 -- go.mod` shows no diff
    to that line), so the vendored snapshot and the last-resolved module
    version agree.
  - **Not recoverable from this repo**: the exact upstream git commit
    SHA for v0.40.0. There is no `.git` metadata, no commit-hash comment,
    and no `go.sum`-derived pseudo-version (the require line uses a
    plain semver tag, not a pseudo-version with an embedded hash) to
    pin it further. A maintainer wanting the precise upstream commit
    would need to check out `charm.land/fantasy` at tag/release
    `v0.40.0` from its own history (or ask Charmbracelet) and diff that
    against this tree file-by-file.
  - **Caveat**: commit `d920b3f4` vendored the tree and applied its own
    fix (see below) *in the same commit* - the schema-preservation
    change touches `agent.go`, `content.go`, `content_json.go`,
    `object/object.go`, `provider_registry.go`, and `tool.go` inside the
    same diff that introduces those files. There is no git history for
    a "clean v0.40.0, then patched" state; the first commit of the tree
    already includes the patch. Anyone diffing against a fresh
    `v0.40.0` checkout must expect that difference in addition to the
    three later patches listed below.

### Local patches to charm.land/fantasy

| Commit | Files (non-test) | What it does | Upstreamable? |
|---|---|---|---|
| `d920b3f4` fix: preserve complete MCP tool schemas in provider requests | `agent.go`, `content.go`, `content_json.go`, `object/object.go`, `provider_registry.go`, `tool.go` (plus new `third_party/fantasy/schema_transport_test.go`) | Adds an `InputSchema map[string]any` field to `FunctionTool`/`ToolInfo` and threads it through Anthropic/OpenAI/Google tool conversion (via a new `cloneInputSchema` helper) so a complete JSON Schema survives round-trip instead of being flattened to just `properties`/`required`. Bundled with the vendoring commit itself (see caveat above), so it cannot be isolated from the v0.40.0 baseline by git history alone - only by diffing against an actual v0.40.0 checkout. | Yes - looks like a generically useful upstream fix (fuller schema fidelity for MCP-style tools), not Sennit-specific. |
| `b0ba1d66` feat: let a caller rotate credentials on the first 429 instead of sleeping | `retry.go` (+279-line `retry_test.go`) | Adds `RetryOptions.OnRateLimit` (`OnRateLimitFunc`) and an unexported `rateLimitHookFired` guard. On the first HTTP 429 within a retry pass, `OnRateLimit` is invoked before the backoff delay elapses; if it returns nil the caller is assumed to have rotated credentials and the next attempt runs immediately (still counted against `MaxRetries`), otherwise normal backoff proceeds. Comments in the diff explicitly call this out as a "Sennit fork addition" throughout. | Plausibly upstreamable as an opt-in hook (mirrors the existing `OnAuthRefresh` pattern), but it is explicitly framed in the code as fork-specific, so treat as Sennit-only until confirmed otherwise. |
| `86792d9b` feat: switch a provider to another account when the current one runs out | `agent.go` (+137-line `agent_test.go`) | Plumbs the new `OnRateLimit` field through `AgentCall` and `AgentStreamCall` into the `RetryOptions` built inside `Generate` and `Stream`, so callers of the public `agent` API (not just `RetryWithExponentialBackoffRespectingRetryHeaders` directly) can supply the hook added in `b0ba1d66`. Pure wiring, no new logic. | Same status as `b0ba1d66` - it only makes that hook reachable; upstream it only if `OnRateLimit` itself is upstreamed. |
| `a1b53f84` fix: retry a provider overload that arrives without a type to classify | `errors.go`, `providers/anthropic/error.go`, `providers/openai/error.go`, `providers/openai/responses_language_model.go` (+ new `errors_test.go`, `providers/openai/error_test.go`) | Adds `IsTransientStreamError(errType, message string) bool` and a small table of lowercase message fragments (`"overloaded"`, `"try again later"`, `"temporarily unavailable"`, `"service unavailable"`, `"internal server error"`). The ChatGPT/Codex backend sends an in-band SSE error with no `type`/`code`, only a message, so `TransientStreamErrorTypes` (keyed by type) never matched and the turn failed after one attempt. Anthropic's and OpenAI's stream-error paths, and the Responses API's `error`/`response.failed` handling (previously a bare non-retryable `fantasy.Error`), now all classify through this one function, so a transient overload retries under the existing 5s/10s/20s backoff and a real failure still fails fast. Rationale is drawn directly from the commit message. | Yes - this is a bug fix for a real backend quirk (message-only error envelopes), not fork-specific behaviour; a good candidate to send upstream. |

Total: 4 commits have touched `third_party/fantasy` since it was vendored
(including the vendoring commit itself, which bundled a patch).

## Module: github.com/charmbracelet/x/powernap

- Upstream: `github.com/charmbracelet/x/powernap` (Charmbracelet, MIT per
  `third_party/powernap/LICENSE`).
- Baseline version: **v0.1.6** (weaker evidence than fantasy - see below).
- Evidence:
  - `go.mod`'s `require github.com/charmbracelet/x/powernap v0.1.6` line
    (line 30) was introduced in the same commit as the fantasy require,
    `f40e7eec` ("Braid", 2026-08-09), again as a plain module dependency
    with no `replace` directive yet - so v0.1.6 was actually resolved
    from the proxy, not just written down.
  - The `replace github.com/charmbracelet/x/powernap => ./third_party/powernap`
    directive and the entire `third_party/powernap` tree were added
    together in commit `535c04f8` ("feat: add workspace symbol search
    and hover tools", 2026-08-25). `git show --stat 535c04f8` shows this
    is a pure addition for every file under `third_party/powernap` (28
    files, 14114 insertions, 0 deletions) - there is no earlier partial
    copy to compare against.
  - The require line's version did not change between `f40e7eec` and
    `535c04f8` (`git log -p f40e7eec..535c04f8 -- go.mod` shows no diff
    to that line), so, as with fantasy, the vendored snapshot and the
    last module-proxy-resolved version agree.
  - **Unlike fantasy, there is no embedded version file** inside
    `third_party/powernap` (no `version.go`/`version.txt` equivalent
    was found by searching the tree) and no `go.mod`/`go.sum` version
    string beyond the module path itself. v0.1.6 is inferred solely from
    the `go.mod` `require` line's history, not from anything inside the
    vendored tree.
  - **Not recoverable from this repo**: the exact upstream commit SHA,
    same as fantasy, for the same reasons (plain semver tag, no
    pseudo-version, no VCS metadata carried into the vendored copy). A
    maintainer would need to check out `powernap` at tag `v0.1.6`
    upstream and diff file-by-file to establish a true baseline.

### Local patches to github.com/charmbracelet/x/powernap

None. Commit `535c04f8` is the only commit in this repo's history that
touches `third_party/powernap`, and it is the vendoring commit itself
(pure additions, no modification of anything that existed before it in
this repo). No commit since has changed vendored powernap code, so - as
far as this repo's git history can show - the copy is still exactly
what was vendored on 2026-08-25. That does not rule out the vendored
copy already differing from a clean v0.1.6 checkout (e.g. if the files
were hand-edited before the first commit); this repo's history simply
cannot detect that, since the tree arrived pre-formed.

## How to upgrade either module

There is no recorded common ancestor to diff against inside this repo -
only the version string inferred above. Upgrading safely means:

1. Fetch the real upstream module at the baseline version identified
   above (`v0.40.0` for fantasy, `v0.1.6` for powernap) from its module
   proxy or source repo, and again at the new target version.
2. Diff baseline-vs-target upstream to get the *real* upstream changelog
   for that range (do not trust this repo's baseline version claim
   blindly for powernap - verify it still builds/behaves the same
   before trusting it as the merge base).
3. Diff baseline-vs-current-third_party (this repo) to recover the
   local patches as a patch set. For fantasy, the patches in the table
   above are already itemized by commit and can be re-applied as
   cherry-picks if the file layout has not moved; for powernap there is
   currently nothing to preserve.
4. Three-way merge: apply the local patch set on top of the new
   upstream target version, resolve conflicts, update `version.txt`
   (fantasy) if applicable, refresh `go.mod`/`go.sum` inside the
   vendored tree, and update this file's baseline version and evidence
   section for the module you moved.
5. Re-run `go build ./...` and the fantasy/powernap-touching package
   tests, then update the "Baseline version" and "Evidence" lines above
   to describe the new snapshot the same way this document describes
   the old one - do not leave this file describing a version that is no
   longer in the tree.
