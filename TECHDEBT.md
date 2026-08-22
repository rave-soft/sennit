# Technical debt

What is still open after the Crush fork, the rebrand, and the removal of the
external services. Every entry carries its reason and a concrete next step, so
it can be picked up again without repeating the investigation.

Closed entries do not accumulate here: once a debt is paid the entry is
deleted, and the history stays in git.

## Open debt

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

## `TestBeginAuth_*` failed once under a full-repo `-race` run and did not reproduce

Observed 2026-08-21, immediately after `internal/agent/tools/mcp`'s `Registry`
was split into `Registry` / `connectionManager` / `authCoordinator`. During
`go test ./... -race`, two tests failed together:

    --- FAIL: TestBeginAuth_CancelSettlesExactStartingOwner
        init_test.go:969: Expected value not to be nil.
    --- FAIL: TestBeginAuth_CancelDoesNotOverwriteNewerLifecycleState/connected
    panic: runtime error: invalid memory address or nil pointer dereference

It has not reproduced since, across: a second full `go test ./... -race` (clean),
`-race -count=6` on the package (clean, 33s), `-race -count=2 -cpu=1,2,8` three
times (clean), and five isolated `-run BeginAuth -race` runs (clean). No DATA
RACE was reported in the failing run — the failure was a nil dereference, not a
detected race.

So this is recorded rather than diagnosed. What makes it worth keeping: the
failing run was the *first* full-suite race run after the auth flow moved to a
new type holding a back-reference to `Registry`, and the symptom is a nil where
an auth flow was expected. The cancel path settling against a flow that has
already been cleared is the shape to look at first —
`authCoordinator`'s flow lifecycle and `Registry.publishMu`'s ownership of
`authFlows`.

A test that fails one run in N is still a failing test; it should not be assumed
benign because a rerun was green.

## The agent's continuation/dispatch tests are intermittently red under `-race` in CI

The `race` CI job (`go test -race -failfast ./...`, added 2026-08-21) has failed on
both runs since it was introduced, each time on a *different* test in the
continuation/dispatch area:

- run 32509070021: `TestSendToParent_AndCompletionBothSurviveSameDrain`
- run 32512761082: `TestDeliverTaskCompletion_RaceWithUserPromptStartsOnlyOneTurn`,
  at iteration 3, `continuation_test.go:465`:
  `expected: 1, actual: 0 -- the user prompt must be delivered exactly once`

**The failure mode is not a scheduling wobble in an assertion — it is a dropped user
prompt.** The test's own `require.Eventually` had already passed (queue drained,
session idle, completion delivered), and then zero user messages were found
persisted. A prompt the user typed went nowhere.

Not reproduced locally: ~26 runs of the named test under `-race` (plain,
`-count=8 -cpu=2,4`, and twelve single-CPU runs) were all green. CI's runners are
slower and differently loaded, which changes the interleaving.

**Regression or newly exposed is unknown, and should not be assumed.** Both tests
predate the dispatcher rewrite, and `continuation_test.go` was not modified by it
(`git diff 378bd0c5 HEAD` touches only one line of `delegation_parent_test.go`). But
the production code under them *was* rewritten that day (7 maps + 3 mutexes collapsed
into one `sessionState`), and the `race` job is new — so there is no prior CI
evidence that these ever passed under `-race`. Establishing which it is means running
the pre-rewrite production code under `-race` in a loop.

Where to look: `dispatchDecision` has two paths that deliberately **drop** a call
rather than queue it — the `Continuation` branch, and the steering follow-up dropped
because "the fold is keyed on the absence of a RunID". If a real user prompt can
reach either branch under the right interleaving, it is discarded with no terminal
event and nothing persisted, which matches the observed `userCount == 0` exactly.

## Windows and macOS CI: what the real logs turned out to be

The three-OS `build` matrix (added 2026-08-21) was red on two legs. Two rounds of
work, 2026-08-22. Round one reasoned from static analysis, fixed real bugs, and
still left 81 Windows failures; round two read the actual runner log and found the
dominant causes had nothing to do with path spelling.

**Round one -- production, path canonicalization.** On Windows the same directory
arrives under two spellings: `t.TempDir()` yields the 8.3 short form
(`C:\Users\RUNNER~1\...`) while git and `filepath.Abs` yield the long one, so a raw
`==` says they differ. Added `fsext.Canonical` and used it at three sites:
`threadspawn/attach.go`'s repo-root check -- which silently left a Windows user
standing at their own repo root with **no thread manager** -- `db/connect.go`'s
pool key, and `fsext/lookup.go`'s stop-at-home checks. Pinned by symlink tests,
which exercise the same aliasing class on Linux. `home.Long` separator mixing and
the `confinement_test.go` hand-built JSON (which meant the confinement boundary was
never exercised on Windows at all) were fixed in the same round.

**Round two -- the 56-failure cluster was never about paths.** Two distinct
resource-lifetime bugs, both invisible on Linux because it happily unlinks open
files and removes directories that are somebody's cwd:

- `t.Cleanup` is LIFO, and several tests registered `t.Cleanup(ResetPool)` *before*
  `t.TempDir()`, so the directory was removed while the SQLite handle was open.
- `ResolveCwd` (`internal/cmd/root.go`) does a process-global `os.Chdir` into
  `--cwd` and never returns -- correct for the CLI, but it left the test process
  sitting inside a `t.TempDir()` that a later cleanup wanted to remove.

**Round two -- production, a leaked workspace flock.** `Bootstrap` registered a
final-cleanup closure calling `wsLock.Release()`, then set `wsLock = nil` on the
next line to disarm an earlier failure-path defer. Closures capture by reference,
so the cleanup released a **nil** lock -- a documented no-op -- and the OS flock on
`sennit.lock` was never dropped. Masked on Linux by process exit; it mattered for a
second `Bootstrap` of the same data directory inside one process.

**Round two -- production, `NewManager` mis-anchored absolute worktree dirs.** It
used `filepath.IsAbs`, not the codebase's `filepathext.SmartIsAbs`, so a config
`WorktreeDir` written Unix-style (`/var/tmp/...`, legal and portable in a config
file) was treated as relative on Windows and silently anchored under the repo.

**Round two -- production, truncation dropped the file name.** `IsLikelyPath`
excluded backslash as a shell metacharacter, so every Windows path failed the check
and fell through to plain right-truncation instead of `TruncatePath`'s head elision
-- cutting away the one part of a path that identifies it.

**Round two -- production, config staleness race.** Closed by holding `stalenessMu`
across the write *and* the snapshot refresh; the comment in `SetConfigFields`
records the bounded-latency trade-off this accepts.

**macOS -- production, keybindings leaked into golden files.** `keys.go` rewrites
`ctrl+` to `super+` on darwin, and `ui.go` passed `runtime.GOOS` straight into
`configuredKeyMap`, so every golden test rendered the host's key hints. The
platform is now an injectable field defaulting to `runtime.GOOS`; goldens pin
`"linux"`. No golden file content changed.

**The tool that made all of this checkable.** `internal/testenv`'s
`AssertRemovableOnWindows` reproduces the Windows constraint on Linux: it walks
`/proc/self/fd` and checks `os.Getwd()` against the directory about to be removed,
registered right after `t.TempDir()` so LIFO runs it just before `RemoveAll`. It
names the exact fd or cwd Windows would refuse -- on `internal/cmd` it named the
same `\003` directory the Windows log did. Prefer extending it over reasoning about
Windows semantics from a Linux box.

**Still open -- the `internal/config` cluster.** `TestApplyWorkspaceConfig` and its
subtests, plus `TestExternalChangeDetected_NewCandidateFile`. Static analysis found
no path-comparison cause: `applyWorkspaceConfig` compares no paths, and the
inspected subtest turns on `os.PathError`'s shape when opening a path whose parent
is not a directory -- an OS-error-shape difference, not a spelling one. Left
unfixed rather than guessed at, twice now. If it survives the next Windows run,
wire `AssertRemovableOnWindows` into that package before theorising further.

**Latent, not touched:** `filetracker`'s `filepath.Rel(s.workingDir, path)` has the
same spelling sensitivity, but it was not in the failure list and widening scope on
suspicion was not worth it.
