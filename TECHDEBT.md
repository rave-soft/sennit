# Technical debt

What is still open after the Crush fork, the rebrand, and the removal of the
external services. Every entry carries its reason and a concrete next step, so
it can be picked up again without repeating the investigation.

Closed entries do not accumulate here: once a debt is paid the entry is
deleted, and the history stays in git.

## Open debt

- **An over-release closes the shared DB connection out from under other
  holders.** `internal/db/connect.go` keeps a process-global,
  reference-counted pool: `Connect` hands back the existing `*sql.DB` with
  `refCount++`, `Release` does `refCount--` and, at zero, deletes the entry
  and closes the handle. The decrement is unconditional, so a caller that
  releases more times than it connected drives the count to zero while other
  holders are still using the connection. Their `*sql.DB` then fails every
  query with "database is closed", and the next `Connect` opens a fresh one,
  so the pool is inconsistent for the whole process. Nothing guards this and
  nothing detects it; the failure surfaces far from its cause, as unrelated
  query errors in whichever component happened to still hold the handle.
  Callers pair `Connect` with `defer Release(dataDir)` (e.g.
  `internal/cmd/gc.go`), so an extra explicit `Release` alongside a deferred
  one is all it takes. Next step: decide between making `Release` defensive
  (ignore a decrement below zero and log loudly, so an over-release is a
  reported bug rather than silent corruption) and handing out a
  release-once handle instead of a bare `dataDir` string. The current
  behavior is pinned by a test so any change is deliberate.

- **The system prompt can assert "Status: clean" when git failed outright.**
  `getGitStatusSummary` (`internal/agent/prompt/prompt.go:264`) runs
  `git status --short 2>/dev/null | head -20`. stderr is discarded and the
  pipeline's exit status is `head`'s, which is always 0, so `err` is nil even
  when git failed; the empty output is then read as "no changes" and the
  prompt states `Status: clean`. The guard above it, `isGitRepo`
  (`prompt.go:233`), only stats `.git`, so any directory where `.git` exists
  but git cannot read the repository — a corrupt or partially-created repo, a
  permissions problem, a missing git binary — reaches this path. Verified
  empirically 2026-08-21: with a bare `.git` directory the pipeline exits 0
  with no output. Consequence: a false statement about the working tree is fed
  to the model on every request in that state. Next step: drop the `| head`
  pipe (truncate in Go instead) so the exit status is git's, and distinguish
  "clean" from "could not determine" in the prompt text. The current behavior
  is pinned by a test with an explanatory comment, so changing it will fail
  that test deliberately.

- **`options.skills_paths` resolves relative paths against the process cwd,
  not the workspace.** `promptData` (`internal/agent/prompt/prompt.go:185`)
  passes each skills path through `expandPath`, which handles `~` and `$VAR`
  but does **not** join against the store's working directory. The sibling
  path for context files does: `processContextPath` (`prompt.go:111`) calls
  `filepathext.SmartJoin(store.WorkingDir(), p)`. So a relative
  `skills_paths` entry silently resolves against wherever the process was
  started, which is the wrong answer whenever Sennit's working directory is
  not the launch directory. Same class as the `isInsideWorktree` bug fixed in
  2026-08-20. Next step: route skills paths through the same `SmartJoin` as
  context paths, and add a test that a relative skills path resolves against
  the workspace.

- **Azure `apiVersion` is silently ignored.** `buildAzureProvider`
  (`internal/agent/providers.go:521`) reads `options["apiVersion"]` from the
  provider config and passes it as `azure.WithAPIVersion(...)`, so a user who
  pins an Azure API version reasonably expects it to be used. It is not:
  in `charm.land/fantasy` v0.40.0 the `azure` provider stores `apiVersion`
  (field at `providers/azure/azure.go:18`, default at `:44`, setter at
  `:101`) and never reads it again, so the value never reaches a request and
  Azure serves whatever fantasy's default resolves to. Found 2026-08-21 while
  writing provider-construction tests; the test for this path pins that
  construction succeeds and carries a comment explaining why it cannot assert
  on the query string. This is an upstream bug, not ours. Next step: report it
  to fantasy, and until it is fixed either document the limitation where
  `apiVersion` is configured or drop the option rather than accepting a
  setting we cannot honour.

- **The external-change watcher can misread this process's own write.**
  `SetConfigFields` writes the config file, then runs the whole reload
  pipeline (`autoReload` -> `reloadFromDisk`: disk read, JSON merge, provider
  reconfiguration) and only refreshes the staleness snapshot at the very end,
  under `writeMu`. A watcher poll landing inside that window sees the on-disk
  mtime already changed while the snapshot still holds the old one, and
  reports an own-write as external — firing the `OnExternalChange` callback
  that re-inits MCP servers and publishes `ConfigChanged`. In production the
  window is unreachable in practice: `externalChangePollInterval` is 2s and
  the pipeline is orders of magnitude faster. It was found (2026-08-21) only
  by driving the interval down to test speeds, where under `-race` it
  reproduced below ~100ms. Recorded rather than fixed because the fix belongs
  in the write path, not the watcher: refresh the staleness snapshot for a
  path as part of the same critical section that writes it, so the on-disk
  state and the snapshot can never disagree, instead of at the end of the
  reload. Next step: move the snapshot refresh in `SetConfigFields` to
  immediately after the atomic write, and confirm the four
  `TestWatchForExternalChanges_*` tests stay green at a 10ms interval under
  `-race` (they currently need 100ms for headroom).

- **Gemini and two consecutive user contents.** Steering is delivered as its
  own `user` message after all tool results
  (`internal/agent/completion_inbox.go`, `prepareStep` in
  `internal/agent/turn.go`). Anthropic merges such a pair itself; for
  OpenAI-compatible providers this was verified with a live request (200, and
  the model acted on the steering specifically); in OpenAI Responses keeping
  them separate is what the protocol expects. On Gemini the `fantasy` adapter
  puts tool results into a `genai.Content{Role: user}` and then creates a
  **second** `Content{Role: user}` right after it; nothing merges
  same-role contents, not in the adapter and not here, and Gemini answers 400
  to non-alternating roles in some cases. Whether it accepts this particular
  shape is an empirical question, and there is no key to ask it with. Next
  step: run one real `user → assistant(tool_calls) → tool → user(steering)`
  request against Gemini once a key is available; if it 400s, merge
  consecutive user contents, preferably in the adapter, otherwise by
  normalizing before the call. Until then, mid-turn steering on Gemini is an
  untested path.

## Decisions deferred pending confirmation

### GitHub Copilot

Not removed, and keeping it is a deliberate decision (2026-08) rather than an
omission. It is a model provider, like OpenAI or Anthropic, not a Charm
service: removing it takes a backend away from the user, it does not detach
the fork from someone else's infrastructure.

What ships with it is worth knowing, because "just another provider" does not
describe the situation fully. The client authenticates with someone else's
OAuth application and presents itself as someone else's product:

- `clientID = "Iv1.b507a08c87ecfe98"` (`internal/oauth/copilot/oauth.go`) —
  the Copilot/VS Code client ID, not one registered to rave-soft.
- `userAgent = "GitHubCopilotChat/0.32.4"`, `editorVersion = "vscode/1.105.1"`
  (`internal/oauth/copilot/http.go`) — to GitHub's API, Sennit presents itself
  as the Copilot Chat extension for VS Code.

All of this is inherited from upstream rather than introduced here. An
inconsistency lives next to it: `SignupURL` in
`internal/oauth/copilot/urls.go` carries `editor=sennit`, so signup is branded
as Sennit while the traffic claims to be VS Code.

Three possible next steps, none of them started:

1. Do nothing — the status quo this entry records.
2. Remove Copilot entirely. The surface is larger than it looks: it is
   mentioned in 36 files, 24 of them non-test. The `internal/oauth/copilot`
   package (419 lines) and the `internal/ui/dialog/oauth_copilot.go` dialog
   (77 lines) go entirely; then the branches in `internal/cmd/login.go` and
   `logout.go`, the refresh path in
   `internal/config/credentials/credentials.go`, `SetupGitHubCopilot` and the
   token exchange in `internal/config/{config,store,providers_merge,reload}.go`,
   the dialog wiring in `internal/ui/model/`, the "model not enabled in
   Copilot" diagnostic in `internal/agent/turn.go`, and the plumbing in
   `internal/workspace/`. After that `catwalk.InferenceProviderCopilot` is no
   longer used anywhere.
3. Keep it, but stop impersonating VS Code: register a GitHub OAuth App of our
   own and send an honest `User-Agent`. Three constants to edit, but it needs
   an application registered, and whether GitHub grants such a client access
   to the Copilot API is an empirical question — theirs is a restricted list.

## Limitations of imported agent definitions

Not debt but the contract of `sennit import` — documented here because the
code points at this section (`internal/config/import.go`, the `sennit-config`
skill).

User's decision (2026-08): Sennit no longer scans another tool's config
directories on its own. Discovery — `internal/config/agents_markdown.go`
(`agentDirs`) and `internal/config/load.go` (`GlobalSkillsDirs`,
`projectSkillSubdirs`) — reads only `.sennit/agents` and `.sennit/skills`
(plus their global equivalents and `options.skills_paths`). It used to take in
`.claude/agents`, `.opencode/agent`, `.agents/skills`, `.claude/skills`,
`.cursor/skills`, and `.opencode/skills` as well — meaning Sennit silently
trusted the contents of directories another tool writes, without validating
them and without telling the user what failed to apply.

Bringing files over now goes through an explicit import only — `sennit import
claude|opencode [--skills] [--agents] [--dry-run] [--global] [--force]`
(`internal/config/import.go`, `internal/cmd/import.go`). The import copies
into `.sennit/skills`/`.sennit/agents` rather than reading a foreign directory
on the fly, and prints a report for every file (`imported`/`adjusted`/
`skipped` plus the reason or warnings) — the same limitations below still
apply, but the user now sees them at import time instead of discovering them
afterwards in the logs.

- `permission:` blocks from opencode files are **not applied**, neither on a
  normal load nor after an import. A role locked to read-only there gets
  everything listed in its `tools` under Sennit. The import reports this as a
  warning ("permission block is not supported; restrict tools via the tools
  list instead") and leaves a comment in the frontmatter, but does not write
  the field itself. Restrict via the `tools` list or the config's
  `permissions` section instead.
- The `model` field from foreign files understands `provider/model-id`
  references when such a model exists among the configured providers — it
  resolves through `config.ResolveModelString`. There are no `large`/`small`
  slots for `Agent.Model` any more: those words carry no special meaning even
  in Sennit's own agent files and are treated like any other unresolvable
  string. Values that resolve to nothing (`opus`, say — a model name from
  another tool that is not in the config — or literally `large`/`small`) are
  not dropped silently on import: the `model` field is omitted (the agent
  inherits the main model), the original value stays in the frontmatter as a
  comment (`# original model: ... — not available`), and the import reports it
  as a warning rather than an `slog.Debug`.
- `reasoning_effort` / opencode's `effort`: `low`/`medium`/`high` carry over
  as they are; close but non-standard values (`max`, `minimal`, ...) map onto
  the nearest valid level with a warning; unrecognized ones are dropped with a
  warning and left as a frontmatter comment.
- opencode's `temperature`/`top_p`: `config.Agent` has no such fields. The
  import neither rejects the file nor invents a field — the value stays as a
  frontmatter comment and the import reports a warning.
- Tool names with no counterpart (`WebSearch`, for instance) are not dropped
  silently on import — they are reported as a warning naming the tool. On a
  normal load of `.sennit/agents`, the Claude Code name translation
  (`Read`→`read` and so on, `ClaudeToolNames` in `agents_markdown.go`) no
  longer applies at all: files in Sennit's own directory are expected to name
  tools Sennit's way already. The translator was not deleted — it is used by
  the import only.
- Delegation is single-level: a role cannot call another role.

## Docker MCP availability is a process-global cache with a wall-clock TTL

`internal/config/docker_mcp.go:18` keeps availability in a package-level
`dockerMCPAvailabilityCache` guarded by its own mutex, with a 10s TTL
(`dockerMCPAvailabilityTTL`) measured against `time.Since`. There is no seam to
reset or inject it.

Two consequences. First, tests that exercise anything reading
`DockerMCPAvailabilityCached()` — e.g. `Commands.InitialCmd` in
`internal/ui/dialog/commands.go` — are order-dependent: whichever test warms the
cache first decides what later tests in the same process observe. This surfaced
while covering `commands.go` (2026-08-21); the test works around it by setting
`c.dockerMCPAvailable`/`c.dockerMCPCheckInFlight` on the component directly
rather than going through the cache. Second, the TTL is wall-clock, so a slow
test run can silently cross the 10s boundary and flip `known` mid-suite.

Fix shape: move the cache behind a small injectable type (a struct with a clock
and a lookup func) held by whatever needs it, or at minimum export a
test-only reset. Do not paper over it with `t.Sleep`.

## `--debug` is lost on the first config reload

`Options.Debug` has two sources: the `"debug"` key in a config file, and the
process's `--debug` flag, which `internal/app/bootstrap.go:102` passes into
`config.Load(path, dataDir, opts.Debug)`. The flag is applied once, while the
config is being built (now `internal/config/build.go:76`), and is never recorded
on the `ConfigStore` -- there is no `debug` field on the store to remember it.

`reloadFromDisk` rebuilds the config from the files on disk, so it cannot
reapply a flag it never saw. A process started with `--debug` therefore silently
drops back to non-debug the first time its config reloads (a config-file write,
a watcher tick, a sibling instance's change). Users who set `"debug": true` in
the file are unaffected -- for them the value comes back through the merge.

Observable effect: `internal/agent/providers.go:361` and `:497` read
`Options.Debug` to decide provider HTTP debug logging (including the Copilot
client), and `internal/agent/tools/sennit_info.go:506` reports it. So the
symptom is provider request logging that stops partway through a session for no
visible reason -- the worst kind of debugging experience, in the debugging
feature itself.

Found 2026-08-21 while extracting `buildConfig` from `Load`/`reloadFromDisk`;
the two copies had drifted and only `Load` ever set it. Reproduced exactly in
the extracted pipeline (`buildConfigOptions.debug`, set only by `Load`) rather
than fixed, because unifying it changes shipped behavior and deserves its own
decision.

Fix shape: store the process-level `--debug` override on `ConfigStore` when
`Load` receives it and have `buildConfig` reapply it on every reload, so the
flag behaves like the process-scoped override it is.

## Provider-drop reporting is inconsistent between merge and validate

Surfaced 2026-08-21 while extracting the shared helpers into
`internal/config/providers_shared.go`. Both were reproduced exactly rather than
unified, because each changes what a user sees from `sennit doctor`.

**1. Hint present on catalog drops, absent on custom drops.** When a *catalog*
provider is dropped for missing credentials, `providers_merge.go` records a
`Problem` carrying `Hint: "static check only; ..."`. Every custom-provider drop
in `providers_validate.go` records a `Problem` of the same shape with no `Hint`
at all. There is no known reason the two classes of provider should differ in
whether the user gets the hint.

**2. A discovery-triggered empty-models drop reports nothing.** In
`providers_validate.go`, a provider whose model discovery fails and which ends
up with zero models is dropped with two `slog.Warn` calls and **no** `Problem`.
A few lines below, the generic "provider has no models" drop — the same
underlying condition — logs *and* records a `Problem`. So whether `sennit
doctor` tells the user their provider vanished depends on which of two nearly
identical branches happened to fire.

Note the contrast with a genuinely deliberate case in the same function: a
provider dropped because the user set `disable: true` logs at `slog.Debug` and
records no `Problem` on purpose — the user asked for it to be off, so it is not
a misconfiguration. That one is now explicit in `dropProvider`'s parameters; the
two above are not deliberate, merely undecided.

Fix shape: decide the policy once — most likely "every drop the user did not ask
for records a Problem, and Problems of the same class carry the same Hint" — and
make `dropProvider`'s call sites reflect it.

## `failCreate` marks the wrong row when `SetSession` fails

`internal/thread/manager.go:302`:

    st, err = m.store.SetSession(ctx, st.ID, sess.ID)
    if err != nil {
        return Thread{}, m.failCreate(ctx, st, err)
    }

The assignment overwrites `st` with the store's zero-value return *before* the
error is checked, so `failCreate` receives an empty `Thread` and marks status
against an empty ID. The thread row that actually exists is left at whatever
status it had, and nothing records why creation failed.

Found 2026-08-21 while unifying `Create`/`Activate`'s rollback paths. Not fixed
there: the rollback work was scoped to *undoing side effects*, and changing which
row gets marked failed is a separate, observable behavior change.

Related and deliberately left alone in the same pass: `Create`'s `ctx.Err()` and
`setStatus`-failure paths return without calling `failCreate` at all, unlike
their siblings, so those rows also keep their previous status rather than
becoming `StatusFailed`. Whether that asymmetry is deliberate is unclear from the
code; decide it together with the above.

Fix shape: capture the store's result in a separate variable and pass the
original `st` to `failCreate`, then settle the `failCreate`-on-every-path
question one way for all of them.
