# Technical debt

Only active items belong here. Remove an entry when it is resolved or deliberately
rejected; git history keeps the investigation.

## Gemini steering compatibility

Steering after tool results reaches Fantasy as separate `Tool` and `User`
messages. Its Gemini adapter maps both to adjacent `user` contents. Anthropic and
OpenAI-compatible providers accept this flow, but Gemini has not been verified.
Sennit cannot merge the messages without dropping either tool results or steering;
that conversion happens inside Fantasy.

Next step: run a real `user → assistant(tool_calls) → tool → user(steering)`
request against Gemini. If Gemini rejects it, fix Fantasy's Google adapter by
merging adjacent contents that map to the same Gemini role.

## GitHub Copilot identity

The Copilot provider uses the inherited VS Code/Copilot OAuth client ID and presents
requests as `GitHubCopilotChat`/VS Code, while signup identifies the editor as
Sennit. Keeping the provider is intentional, but the identity mismatch remains.

Next step: either register a Sennit-owned GitHub OAuth application and use an honest
user agent, if GitHub permits Copilot API access for it, or remove the provider.

## Bash deny list is bypassed by any silent approval

`newBashTool` (`internal/agent/tools/bash.go:262`) now prompts for a
deny-listed command instead of refusing it, and on approval runs the command
with `execBlockFuncs = nil` — the whole deny list off for that invocation.
The approval comes from `permission.Request`, which returns `true` without
showing anything to the user when YOLO is on (`s.skip`), when the session is
auto-approved, when `--allowedTools` contains `bash` or `bash:execute`, or
when a `PreToolUse` hook answered `allow`. In all four cases `sudo`, `curl`,
`apt-get install` and `go install` now execute silently; before the change
the deny list refused them regardless of permission mode.

A second, smaller problem in the same path: a deny-listed command produces
two consecutive dialogs, the new "Execute deny-listed command: …" one and
then the ordinary "Execute command: …" one, since a deny-listed command is
never `isSafeReadOnlyCommand`.

Next step: decide whether the deny list is a user-overridable prompt or a
hard floor. If it is a prompt, it still must not be satisfiable by a blanket
approval — route it through a channel that ignores `skip`/auto-approve/
`allowedTools`, or keep the block funcs on and drop only the one matched
entry. Then fold the two dialogs into one.

## The consecutive-429 circuit breaker never trips

`runTurn.rateLimitSteps` (`internal/agent/turn.go:152`) is incremented only in
`handleStreamError`, and `handleStreamError` is called only at
`internal/agent/run_turn.go:866`, after `Stream` has already returned an
error. Fantasy's step loop returns on the first step error
(`third_party/fantasy/agent.go:1084`), so a step that exhausts its retry
budget on a 429 ends the whole turn and the `runTurn` with it. The counter
can therefore never exceed 1, and the `t.rateLimitSteps >= rateLimitStepLimit`
guard in `prepareStep` is unreachable: the turn-ending "Rate limited" finish
reason it persists is never shown. The commit added no test, which is why
nothing caught it.

Next step: either drop the breaker and its sentinel `rateLimitStopError`, or
move the counter somewhere that outlives one `runTurn` — the auto-continuation
path that starts the next turn for the same run — and give it a test that
drives three consecutive 429 turns.

## Per-account token refresh is wired but unreachable

`f5ae77825` added a whole per-account OAuth refresh path:
`Manager.RefreshOAuthTokenForAccount` (~100 lines in
`internal/config/credentials/credentials.go`), `ConfigStore.ListAccounts`/
`UpsertAccount`, a `RefreshOAuthTokenForAccount` method on the `Workspace`
interface with its four implementations and test stubs, and a
`dialog.ActionRefreshAccountTokens` handler at
`internal/ui/model/dialog_actions.go:418`. Nothing ever emits that action:
the only key bound in the accounts dialog is `r`, which emits
`ActionRefreshTokens` and refreshes the *provider's active* credential
instead. The commit message's "lets the user refresh a specific account's
token from the list" does not describe the shipped behaviour.

If it is wired up as written, there is a second problem waiting: when the
selected account happens to be the active one, the exchange rotates the
refresh token and writes the new one into the account store while the live
`ProviderConfig` credential keeps the spent one, so the next active-credential
refresh fails with `invalid_grant`.

Next step: either bind the per-account action to a key in the accounts list
and make the active-account case also publish the new token to the live
credential, or delete the unreachable path and keep only the provider-wide
refresh.

## fantasy `retry.go`: an undocumented patch that does nothing

`RetryWithExponentialBackoffRespectingRetryHeaders`
(`third_party/fantasy/retry.go:88`) ends with a branch that, on a
`*RetryError` whose chain carries a 401, returns `zero, retryErr`. In that
branch `err` *is* `retryErr`, and the pre-existing code already returned
without retrying, so the branch changes nothing except discarding `result`.
Its comment ("rather than hammering an endpoint that is already rejecting
us") describes a retry that never happened. It also does not achieve what it
claims to: `RetryError.Unwrap` returns only the *last* error
(`third_party/fantasy/errors.go:320`), so the 401 it detected still does not
surface to `errors.As` downstream — including `agent.isAuthError`, added in
the same commit to quarantine a 401 account. The accompanying subtest passes
identically without the change.

Separately, `third_party/PATCHES.md` says "Update this file whenever a commit
touches `third_party/`", and its `retry.go` row still lists only the
`OnRateLimit` patch. This change is an unrecorded local divergence that the
next `git subtree pull` will resolve blind.

Next step: remove the branch, or make it unwrap to the 401 it found and give
it a test that fails without it. Either way record the current state of
`retry.go` in `PATCHES.md`.

## Bare `r` in the accounts dialog eats the filter input

`internal/ui/dialog/accounts.go:119` binds a bare `r` to "refresh tokens", and
`Accounts.HandleMsg` matches it before delegating to `m.sd.HandleMsg`. The
underlying `selectDialog` feeds every unclaimed key into its filter input
(`internal/ui/dialog/select_dialog.go:264`), so the letter `r` can no longer
be typed when filtering accounts — which matters, since the rows are email
addresses. The binding's own comment claims "the filter input only claims
bare letters when it has focus"; that is not how `handleNavigation` works.
The two neighbouring bindings in the same struct deliberately use `ctrl+r`
and `ctrl+x` for exactly this reason, and a later commit in the same session
moved the settings dialog's sign-in shortcut from `a` to `ctrl+a` after
hitting the same collision.

Next step: rebind to a chord, consistent with `Edit`/`Delete` and with
`AccountForm.Auth`.

## "general-purpose" is appended to the agent enum unconditionally

`delegationFinalizer.agentTool` (`internal/agent/delegation_finalizer.go:976`)
appends the literal `"general-purpose"` to the `subagent_type` enum whenever a
named roster exists, and routes that value to the built-in agent
(`:998`). A workspace that configures an agent actually named
`general-purpose` therefore gets the string twice in the schema enum (which
strict-schema providers reject) and can never reach its own agent — the alias
check runs before `runNamedAgent`. The name is a magic string repeated in four
places with no constant.

Next step: give the alias a constant, append it only when the roster does not
already contain it, and let a configured agent of that name win over the
built-in.

## The dialog cursor anchor search is copy-pasted and border-fragile

`OAuth.proxyCursor` (`internal/ui/dialog/oauth.go:574`) and
`ProviderSettings.Cursor` (`internal/ui/dialog/provider_settings.go:431`) now
carry the same hand-rolled routine: split the rendered view, `ansi.Strip` each
line, `strings.TrimLeft(plain, "│╭╰ ")`, check `HasPrefix(prompt)`, then
`strings.Index` back into the untrimmed line. The cutset is a hardcoded set of
three box-drawing runes, so any other border style (`┌`, `└`, `├`, a thick or
doubled frame) silently drops the cursor instead of placing it, and the cutset
also eats a leading space that belongs to the value. `ProviderSettings`
additionally disambiguates three same-prompt fields by substring-matching the
value or placeholder, which is ambiguous whenever two fields hold the same
text.

Next step: extract one helper next to `InputCursor`, take the border runes
from the theme rather than a literal, and key the field by something
unambiguous (render the field with a zero-width marker, or measure the row
while building the parts).

## The provider settings auth badge does not read what it says

`ProviderSettings.loadAuthStateCmd`
(`internal/ui/dialog/provider_settings.go:234`) is documented in three places
as an "async `ListAccounts` + `RuntimeProvider` read"; it never calls
`ListAccounts`. It calls `com.Config().RuntimeProvider(providerID)` twice in
one expression, and the badge it feeds is labelled "Active account:" while
reporting the provider-level credential, which is a different thing as soon as
more than one account is stored.

Next step: either read the active account's own token and keep the label, or
relabel the badge to the provider credential and collapse the duplicated
lookup.
