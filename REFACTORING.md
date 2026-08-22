# Refactoring plan

Consolidated from a full-project review (2026-08-20) covering `internal/ui`,
`internal/agent`, `internal/config` + `internal/shellconfig`, the app/CLI/data
layer (`app`, `cmd`, `thread`, `workspace`, `session`, `message`, `db`), and a
cross-cutting sweep. Baseline: ~925 Go files, ~204k lines; `go vet` and
`golangci-lint` clean; rebranding from Crush essentially complete.

Ordering principle: correctness fixes and quick wins first, then test coverage
on the riskiest untested surfaces (the safety net), then mechanical
deduplication, and only then the structural decomposition of the god objects.
Structural work done before the safety net exists is the main risk in this
plan — do not reorder phases 3 and 5.

Effort key: S = hours, M = a day or two, L = multiple days.

---

## Phase 0 — Correctness fixes — DONE (2026-08-20)

These are latent bugs found during the review, not refactorings.

All 13 items are fixed, each with a regression test where one was possible.
One piece was deliberately scoped down and remains open:

- **`sennit gc` worktree teardown.** gc now *reports* the worktrees it
  orphans (human and `--json` output, dry-run included, only paths that
  still exist on disk), so the information is no longer lost when the row
  is deleted. It still does not remove them — deleting a directory that may
  hold a user's work is left to the gc extraction in phase 4.
- [x] **`internal/agent/providers.go:384,390`** used to call
  `os.Setenv("ANTHROPIC_API_KEY", "")` to stop the Anthropic SDK picking the
  key up from the environment for a Bearer-token or MiniMax provider — a
  process-global write that corrupted the key for every other Anthropic
  provider built afterwards and every subprocess Sennit spawns.
  `option.WithAPIKey("")` was not an equivalent local fix, since the SDK's
  `WithHeader` uses `Header.Set` and would send an empty `X-Api-Key` header
  instead of omitting it. Fixed with header deletion after
  `DefaultClientOptions`: `buildAnthropicProvider` now installs a small
  `stripHeaderTransport` (same seam `azureAPIVersionTransport` uses) that
  deletes `X-Api-Key` from the outgoing request when auth goes through
  `Authorization` instead, leaving the process environment untouched. No
  `os.Setenv` remains in `providers.go`. See `TestBuildAnthropicProvider` in
  `internal/agent/providers_build_test.go`, including a regression test that
  builds a Bearer provider and then confirms a later provider still sees
  `$ANTHROPIC_API_KEY`. The `internal/config` half of this item (mutex
  serialization of `PushPopEnvOverrides` and `applyEnv`) was already done.

- [x] **`log.RecoverPanic` skips cleanup and swallows the panic when the log
  file can't be created** — `internal/log/log.go:98-121`: `cleanup()` runs
  only inside the `err == nil` branch, so a read-only dir / full disk silently
  skips the caller's cleanup, and the recovered panic is never logged. All
  three call sites pass cleanup funcs. Hoist `cleanup()` out (defer it first),
  `slog.Error` the panic + stack unconditionally. (S)
- [x] **`sennit gc` leaks git worktrees** — `internal/cmd/gc.go:410-418`
  deletes terminal thread rows but never removes the thread's worktree/branch
  (contrast `thread.Manager.Remove`, `internal/thread/manager.go:1015-1025`).
  An aged-out failed thread leaves an orphaned worktree with no row left to
  find it by. Fix as part of the gc extraction (phase 4), or at minimum report
  orphans now. (M)
- [x] **`workspace.ResolveSession` mis-picks the "last" session** —
  `internal/workspace/resolve_session.go:35-46`: `sessions[0]` is accepted
  unconditionally, so `--continue` can return a child/agent-tool session.
  `session.Service.GetLast` already answers this in SQL; expose it on the
  workspace interface and delete the client-side scan. (S)
- [x] **`isInsideWorktree()` ignores the working directory** —
  `internal/config/paths.go:142-149` shells out without `cmd.Dir`, so it
  answers for the process cwd, not the workspace being configured (matters in
  server mode). Replace with `worktreeRoot(dir) != ""` and pass
  `cfg.workingDir`. Related: `worktreeRootCache` is never invalidated. (S)
- [x] **`errors.Is`/`errors.As` violations that break under wrapping** —
  `internal/cmd/login.go:128`, `internal/oauth/copilot/oauth.go:83,86` (the
  device-flow retry loop silently stops retrying if `errPending` is ever
  wrapped), `internal/shell/dispatch.go:133`, `internal/agent/tools/grep.go:369`,
  `internal/agent/tools/glob.go:175`, `internal/agent/tools/ripgrep.go:107`,
  `internal/shell/exec_unix.go:93`. Enable `errorlint` in `.golangci.yml` and
  fix (25 findings total; the `==` comparisons are correctness, the `%v`
  formatting ones are style). (M)
- [x] **`fsext.HasPrefix` false-negative on `..`-prefixed segments** —
  `internal/fsext/fileutil.go:227-234`: `Rel("/a", "/a/..foo")` → `"..foo"` →
  reported as outside the prefix; a `..`-named directory silently drops out of
  LSP scope. Check for `".."` exactly or `"../"` prefix. (S)
- [x] **`make ci` is broken** — `Makefile:82` references a non-existent
  `client-server` target; `link` and `ci` missing from `.PHONY`. (S)
- [x] **Machine-specific absolute path in tracked `sennit.json`** —
  `"command": "/home/borodulin/go/bin/gopls"` breaks the config for every
  other contributor. Use `"gopls"`. (S)
- [x] **Duplicated `atomicWriteFile` with divergent Windows behaviour** —
  `internal/config/atomicwrite.go` has the rename-retry hardening,
  `internal/projects/projects.go:154` is the same body without it, so
  `projects.json` writes fail on Windows exactly where `sennit.json` writes
  were fixed. Promote the config version into `internal/fsext`. (S)
- [x] **Runtime `os.Setenv` from library code** —
  `internal/agent/providers.go:384,390` and `internal/config/load.go:227-257`
  mutate the process env; concurrent with `os.Getenv` this is a data race, and
  two workspaces reloading concurrently stomp each other
  (`PushPopEnvOverrides` has no cross-store serialization). Thread values
  through a config-carried env map, or at minimum serialize and document. (M)
- [x] **`csync.NewMap(initial)` aliases the caller's map** —
  `internal/csync/maps.go:18-24` stores the map by reference (contrast
  `NewSliceFrom`, which copies). `maps.Clone` it. (S)
- [x] **`herdr` test-mode detection via `flag.Lookup("test.v")`** —
  `internal/herdr/client.go:121`; the codebase already uses
  `testing.Testing()` in five other places. (S)
- [x] **`fsext.ToWindowsLineEndings` not idempotent on mixed content** —
  bails if any CRLF present; normalize to LF first. (S)

## Phase 1 — Quick wins: dead code, stale artifacts, hygiene — DONE (2026-08-20)

Mechanical, zero-behavior-risk deletions. Clears noise before real work.

**Dead code (verified unused):**
- [x] `internal/agent`: `auth.go:99 isUnauthorized`,
  `dispatch.go:564` + `queue.go:73` `clearPendingCancel`,
  `tools/mcp/tools.go:157,176`, `tools/mcp/transport.go:22`,
  `coordinator.go:135 toolsCache` (fix the tests that keep it alive),
  `tools/mcp/registry.go:170 defaultRegistry` singleton and its refcount.
- [x] `internal/ui`: `chat/unified_diff.go` (whole file, test-only),
  `model/messages.go:16,22,92` superseded methods,
  `chat/tools_core.go:82-88 DefaultToolRenderContext` ("TODO" stub, unreachable)
  and `:135 ToolRendererFunc`, `dialog/dialog.go:307 DrawOnboarding`,
  `model/ttl_cache.go:94 ttlCache.apply`, `diffview/style.go:28
  DefaultLightStyle` + unused builder options, exported tool-item ctors
  superseded by `toolItemFactories` (unexport or route tests via registry).
- [x] `internal/config`: `RefreshStalenessSnapshot` (always-nil error, one
  test caller), the `captureStalenessSnapshot` alias, dead `Providers()` error
  return + unreachable fallback branches in `load.go`/`reload.go` (also drops
  dead `err` handling at 6 UI call sites).
- [x] `internal/event`: `Init`, `GetID`, `Alias` have zero callers; delete or
  document as intentional re-attachment points.
- [x] `internal/db`: `sqlc.yaml` `emit_prepared_queries: true` generates ~600
  lines of never-used `Prepare` machinery — flip and regenerate. Unused
  generated queries `CountSessionFiles`/`CountSessionReadFiles` (which gc.go
  re-implements by hand — see phase 4).

**Stale comments and upstream artifacts:**
- [x] ~15 comments referencing the removed `backend` package
  (`internal/app/app.go`, `bootstrap.go`, `threadspawn/`, `thread/lifecycle.go:320`,
  `proto/proto.go`); `internal/workspace/workspace.go:1-4` ("two
  implementations… HTTP client SDK"); stale import-cycle rationale in
  `workspace/threads.go:33-38`; `thread/types.go:84-86` ("Nothing constructs
  KindTask yet" — contradicted by `tasks.go:158`); wrong `cloneForWrite` doc
  (`config.go:590-596`) and the stale reasoning in
  `store.go:726-730 PersistRefreshedToken`; Crush references in
  `tools/read.go:84` and `credentials/refresh_singleflight_test.go`;
  "Charmtone Pantera" doc in `styles/quickstyle.go:101`.
- [x] `internal/cmd/root.go:133` crash message promises telemetry that
  `internal/event` explicitly never sends; rewrite ("copy the stacktrace and
  open an issue at …"). The `heartbit` logo var is upstream's.
- [x] Stale `model/AGENTS.md` in `internal/ui` — documents `UI` fields that
  moved into sub-state structs, and instructs "keep most logic and state here"
  (see phase 5).

**Tooling hygiene:**
- [x] `.golangci.yml`: enable `errorlint` (phase 0) and `unconvert` (9 dead
  conversions, auto-fixable); delete or justify the 13 commented-out linters;
  the `noctx` exclusion rule matches nothing.
- [x] `Makefile clean:` → also `rm -f $(BINARY) *.test; rm -rf site .venv-docs`
  (439 MB of stale local artifacts observed).
- [x] `.githooks/pre-commit` runs `go mod tidy` (a write) — use
  `go mod tidy -diff`.
- [x] `main.go:16` unconditional pprof blank import — move behind the
  `SENNIT_PROFILE` guard or a build tag.
- [x] Variadic-as-optional `makeAuthRefreshCallback(…, active ...*activeRuntime)`
  (`agent/auth.go:100`) → explicit `active *activeRuntime`, with sub-agents
  passing an explicit nil. **The send-tool dedup in the same item was dropped
  after measuring it** (2026-08-20): only `task_send.go`/`thread_send.go` are
  near-identical (4 differing lines once the noun is normalized), and their
  irreducible half — the tool name const, the embedded `.md.tpl`, and a params
  struct whose `description:` tags are deliberately different LLM-facing schema
  text (`"The task's ID"` vs `"The thread's ID or name"`) — cannot be shared,
  because struct tags are compile-time constants. Only ~12 lines of handler
  body per file are actually common. The claimed sibling duplicates are not
  duplicates at all: `task_list`/`thread_list` differ by 30 diff lines,
  `task_cancel`/`thread_remove` by 52, `task_result`/`thread_status` by 54.
  A generic helper would add indirection to LLM-facing tool definitions that
  currently read top-to-bottom, for a negligible saving.

## Phase 2 — CI — DONE (2026-08-21)

- [x] CI runs only `ubuntu-latest`, no `-race`, while the tree carries
  substantial Windows-specific code (`lock_windows.go`, `drive_windows.go`,
  rename-retry, ~15 windows skip markers) that is never even compiled in CI.
  Add an OS matrix (ubuntu/macos/windows) and a separate race job. (M)
- [x] Speed up `internal/config` tests (18.5s). **The original diagnosis here
  was wrong and was corrected by measurement** (2026-08-20): profiling with
  `go test -v` showed ~16 of the 18.5s are four filesystem-watcher tests —
  `TestWatchForExternalChanges_IgnoresOwnWrites` (6.02s),
  `_DetectsAgentFileChanges` (6.02s), `_DetectsAgentDirCreatedLater` (2.02s),
  `_DetectsEditOfExistingFile` (2.01s) — which wait real wall-clock multiples
  of the hardcoded `externalChangePollInterval = 2 * time.Second`
  (`watch.go:18`). Everything else in the package runs in under 0.4s, and the
  config-side `shellconfig_*_test.go` files run in 0.5s **total**, so moving
  them down into `internal/shellconfig` — the original suggestion — would save
  essentially nothing and is dropped. The real fix is to make the poll
  interval injectable per store so tests drive it at millisecond scale while
  production keeps polling every 2s. (M)

## Phase 3 — Safety net: tests on the riskiest untested code

Build coverage *before* the structural refactors in phase 5 touch this code.

Current coverage lowlights: `internal/cmd` 25.1%, `internal/db` 9.3%,
`internal/agent/prompt` 0%, `internal/ui/list` 33.9%, `internal/ui/util` 0%.

- [x] **Agent auth path — all 0%**: `agent/auth.go` (`refreshTokenIfExpired`,
  `retryAfterUnauthorized`, `waitForInteractiveReauth`), `aws_sso_refresh.go`,
  and `coordinator.run` itself. This is the 401 → re-auth → retry chain the
  whole `onComplete` coalescing machinery serves. Seams exist
  (`credentials.Manager`, `WaitForTokenChange`); inject a clock and a fake. (M)
  **Done 2026-08-21**: 0% -> 100/100/85.7/81.8/83.3/55.6% across `auth.go` and
  90.9/88.2% in `aws_sso_refresh.go`; package 61.6% -> 69.2%. One additive
  production seam was needed: `credentials.New` gained a variadic
  `Option`/`WithExchangeToken` so tests outside that package can reach the
  pre-existing test-only `exchangeToken` field.
- [x] **`agent/providers.go getProviderOptions`** (270 lines, all provider
  builders 0%): table-drive over (provider, model, effort) → expected
  options. (M)
  **Done 2026-08-21**: `getProviderOptions` 36.2% -> 94.0%; all seven 0%
  provider constructors now 83-93%; `buildProvider` 36.7% -> 100%; package
  69.2% -> 76.3%. No production change. Uncoverable branches are documented
  in the tests: `buildOpenrouterProvider`/`buildVercelProvider`/
  `buildBedrockProvider` take no base URL so no offline request probe is
  possible, and `buildGoogleVertexProvider` resolves Application Default
  Credentials. Found an upstream fantasy bug on the way — see TECHDEBT.md,
  Azure `apiVersion`.
- [x] **`internal/agent/prompt`** (0%, shells out to git, reads user context
  files, deliberately detached to `lifecycleCtx` because it can hang). (S)
- [x] **`internal/db/connect.go`** refcounted pool — double-release corrupts
  the pool for every holder; only benches around it today. (S)
- [x] **UI state machines with no tests.** **Two of the four targets named
  here were wrong and were corrected by measurement** (2026-08-21):
  `dialog/dialog.go`'s `Overlay` grace-period machine is in fact covered
  (`OpenDialog` and `OpenDialogWithGrace` 100%, `inGracePeriod` 85.7%,
  `CloseDialog` and `removeDialog` 100%) — only its stack-query accessors and
  `Restyle`/`StartLoading`/`StopLoading` sit at 0%; `dialog/select_dialog.go`
  is likewise 70-100% across nearly every method. The real gaps in
  `internal/ui/dialog` are `question_freetext.go` (0%, 14 funcs),
  `notifications.go` (0%, 5 funcs), `question_multi.go` (21%),
  `commands.go` (24%), `permissions.go` (37% over 44 funcs) and
  `question_form.go` (40%). `permissions.go` was taken first because it is
  also the file the phase-4 renderer-registry item restructures — its entire
  per-tool rendering path was at 0%. `model/update_session.go`,
  `update_settings.go` and `list/list.go` remain valid targets. (M)
  **Partially done 2026-08-21**: `permissions.go`'s whole rendering surface
  is now covered — `renderContent` dispatch (one case per tool arm plus the
  default), the wrong-params-type guard for all four diff arms, `renderDiff`
  split/unified, `scrollLeft`/`scrollRight` clamping and the small accessors,
  all 0% -> 75-100%; package 52.8% -> 55.1%, no production change. Verified
  the suite catches the exact regression it exists for: swapping one switch
  arm fails precisely that arm's dispatch and guard subtests and nothing
  else. Still open here: `question_*` family, `notifications.go`,
  `commands.go`, `model/update_session.go`, `update_settings.go`,
  `list/list.go`.
  **`question_*` family done 2026-08-21**: `question_freetext.go` 0% -> 92.4%
  (all 14 funcs), `question_editor.go`'s note machine (`openNote`,
  `closeNote`, `handleNoteKey`, `handlePaste`, `noteCursor`) 0% -> 100%, and
  the three `HandleKey` state machines 23.4/31.4/25.0% -> 83.0/88.6/95.0%;
  package 55.1% -> 63.7%, no production change. Coverage was not accepted as
  the criterion: mutation testing found that the number-key branch
  (`question_multi.go:57-65`) counted as covered while the tests only ever
  pressed a digit on an *unselected* choice, so replacing its
  toggle-and-delete with `d.selected[idx] = true` passed the whole suite —
  pressing "2" twice would have silently stopped deselecting. All three
  toggle sites (space, number key, mouse click) now fail under that mutation.
  **`notifications.go`, `commands.go` and `internal/ui/model` done
  2026-08-21**: `notifications.go` 0% -> 93.6% average over all five funcs (the
  remainder is the `newSelectDialog` error path the code itself documents as
  unreachable for notifications); `commands.go` 24 funcs to 93.0% average, with
  the category cycle `nextCommandType` 37.5% -> 100% and `previousCommandType`
  22.2% -> 100%; dialog package 63.7% -> 66.5%. In `internal/ui/model`,
  `updateSettings` 23.9% -> 98.2%, `applySessionsLoaded` 0% -> 100%,
  `updateSession` 62.7% -> 90.5%, package 59.6% -> 63.2%. No production change
  in either package. Mutations verified independently: clamping the forward
  command cycle at `MCPPrompts` instead of wrapping fails three named cycle
  tests, and deleting the stale-generation guard from the
  `agentModelInitializedMsg` arm of `updateSettings` fails
  `TestUpdateSettings_AgentModelInitializedMsg` — that guard is repeated in
  every arm and is precisely what a decomposition would drop.
  Covering `commands.go` surfaced a test-isolation hazard now recorded in
  TECHDEBT.md: `config.DockerMCPAvailabilityCached()` is a process-global cache
  with a wall-clock TTL and no reset seam.
  **`list/list.go` done 2026-08-21**: package 34.1% -> 96.1%, with **no
  function left at 0%** (`SpacerItem`'s three closed last, via
  `item_test.go`). No production change.
  This step needed hands-on rescue: the subagent stopped without reporting
  and left the package red, with four tests asserting behavior the code does
  not have. All four were wrong expectations, not defects, and were corrected
  in the tests -- notably `ScrollBy` upward, where re-adding the previous
  item's height *and* its gap legitimately parks `offsetLine` inside the gap
  region (a(h=4) + gap(2): two lines up from b's first line is (0, 5), not
  (0, 3)); `Offset()` reports 5 against (1,1)'s 7 and `ScrollBy(+2)` round-trips,
  so the pair is consistent, not an overshoot. Mutation testing then found a
  real hole: deleting the clamp-to-bottom arm of `ScrollBy` still passed,
  because `TestList_ScrollBy_ClampsAtBottom` uses a single item, whose
  overscroll exhausts the item list and returns early through
  `ScrollToBottom` before the clamp ever runs. Added
  `TestList_ScrollBy_ClampsWithinLastItem` (two items, overscroll landing
  inside the last one), which does fail under that mutation; a second mutation
  dropping `FilterableList.InvalidateAll`'s walk over filter-hidden items
  fails `TestFilterableList_InvalidateAll`. A third, on `ScrollToIndex`'s
  negative-index clamp (`list.go:490`, `index = 0` turned into a wrap), fails
  `TestList_ScrollToIndex_ClampsNegative`. Worth recording: the pre-existing
  `TestList_ScrollBy_ClampsAtBottom` asserts only `AtBottom()`, which stays
  true either way, so it was decorative for the clamp branch it appears to
  guard -- a reminder that an assertion on a coarse predicate can look like
  coverage while pinning nothing.
  Reachability verdict requested during review: the
  `if totalHeight < l.height { l.ScrollToTop() }` tail at `list.go:795` is
  **dead**. Entering its enclosing `else if` requires the selection to sit
  below the visible range, which implies the content already exceeds the
  viewport, so the loop summing back from `selectedIdx` always reaches
  `l.height` and breaks first. Left in place and documented rather than
  deleted or fake-covered.
- [x] **`internal/cmd`**: whole `sennit session` family and `threads.go` at
  0%; `gc_test.go`/`stat_test.go` show the pattern — extend it. (M)
  **Reopened, then closed for real 2026-08-21. I had marked it done
  prematurely.** The `sennit
  session` half was finished (see the step notes: `session.go` 6.7% -> 75-100%
  on all but `sessionWriter`), but `threads.go` was never covered and is still
  at **0%**: `requireThreads`, `runThreadsList`, `runThreadsCreate`,
  `runThreadsMerge`, `runThreadsRemove` all zero. This surfaced during the
  phase-4 `cmdutil` step, whose mutation test on the newly extracted
  `requireThreads` guard **survived** — there was no `threads_test.go` in the
  package at all. Phase 4 was therefore moving code through an unprotected
  region, which is exactly what phase 3 was supposed to prevent.
  **Closed 2026-08-21**: `threads.go` 0% -> **100% on every function**, via a
  new `threads_test.go` (24 tests). Two production changes, both minimal: an
  additive seam `var acquireWorkspace = setupWorkspaceWithProgressBar` so a
  fake `workspace.Workspace` can stand in without booting an `app.App`, and
  `renderThreadsTable` split out of `runThreadsList` so the table, the empty
  case and the goal truncation are testable with no cobra machinery. Three
  mutations die on named tests: disabling the `SupportsThreads` guard, moving
  the truncation boundary off 60, and — verified independently — dropping the
  `cleanup()` call on the guard's error path, which fails five tests and would
  otherwise leak the acquired workspace.
- [x] Consolidate test helpers so refactors stop being expensive: one
  `newTrackedStore(t, …)` for the 18 copies of staleness-snapshot setup in
  `config/store_test.go`; merge the four near-duplicate store constructors
  (`NewTestStore`, `NewTestStoreWithWorkingDir` — added 2026-08-21 for the
  prompt tests, `testStore`, `testStoreWithPath`); give `NewTestStore` a
  real data path and delete the five production `if globalDataPath == ""`
  guards that exist only for it; consolidate agent tests onto `agenttest`
  (unlocks the `toolsCache` deletion). Split the 2703-line `load_test.go`
  along its six themes. (M)
  **Constraint discovered 2026-08-21**: `agenttest` imports `internal/agent`,
  so it can only serve *external* (`package agent_test`) tests — an
  in-package test that imports it is an import cycle. The auth tests are
  in-package (they exercise unexported methods), so they build their own
  offline coordinator. "Consolidate onto `agenttest`" therefore applies only
  to the external-test subset; plan around that rather than against it.
  **Partially done 2026-08-21**: the four near-duplicate store constructors
  are collapsed into one `NewTestStore(cfg, ...TestStoreOption)` with
  `WithLoadedPaths`/`WithWorkingDir`/`WithGlobalDataPath`; every call site in
  the repo was updated, including the external ones in `internal/workspace`,
  `internal/agent/tools` and `internal/agent/prompt`. `load_test.go` is split
  2698 -> 659 lines into `configure_providers_test.go` (23 tests),
  `selected_model_test.go`, `set_defaults_test.go` and `setup_agents_test.go`;
  the set of tests that run is unchanged (50 `func Test*` in `HEAD`'s
  `load_test.go`; 23+2+4+2 moved + 18 kept + `TestMain` = 50). The staleness
  setup is behind one `newStalenessStore` helper — note there are **six**
  such tests, not the 18 this entry originally claimed. Answering the open
  question: no test depended on `testStore`'s zero `resolver` /
  `externalChangePollInterval`, because `configureProviders` takes the
  resolver as its own parameter and never reads the store's.
  **Data path done 2026-08-21**: `NewTestStore` now takes a `testing.TB` and
  defaults `globalDataPath` to `filepath.Join(tb.TempDir(), appName+".json")`;
  the constructor cluster moved to `internal/config/store_testing.go`. Every
  call site was converted — `internal/config`, `internal/workspace`,
  `internal/agent/tools`, `internal/agent/tools/mcp`, `internal/agent/prompt`
  (including its `newStore` helper and its 17 callers). Importing `testing`
  from a non-test file is safe and already precedented here: eight production
  files do it (`load.go`, `doctor.go`, `db/connect.go`, `agent/tools/tools.go`
  and others), flags are registered by `testing.Init()` from the generated test
  main, not on import, and the built binary exposes no `-test.*` flags.
  Test set unchanged (289 tests before and after, only the timing line differs).
  **The "delete the five guards" instruction in this entry was wrong and is
  withdrawn.** Its premise holds — `GlobalConfigData()` (`paths.go:111`) is
  non-empty on every branch and `load.go:46` is the only production assignment,
  so the field is never empty in production — but the conclusion does not
  follow. `store.go:349` returns the exported `ErrNoGlobalConfig`, part of
  `ConfigPath`'s public contract; `modelcache.go:156,188` return real errors on
  a path reachable from `sennit models refresh` (`cmd/models.go:335`). The two
  remaining candidates (`modelcache.go:63,147`) are unreachable from any
  *current* caller but not unreachable by construction: bare
  `&ConfigStore{config: cfg}` literals are still a common test pattern, and one
  of them calling `configureProviders` would reach both with an empty path. All
  five kept.
  **The `agenttest` sub-item was obsolete and is withdrawn 2026-08-21.** Both
  of its premises are dead. (a) `toolsCache` no longer exists anywhere in the
  tree — it went in commit af342d65, so there is nothing left for the
  consolidation to unlock. (b) There is no "external-test subset" to
  consolidate: all 55 `internal/agent/*_test.go` files are `package agent`
  (zero are `package agent_test`), because they exercise unexported methods,
  and `agenttest` imports `internal/agent`, so none of them can ever import
  it. `agenttest.NewCoordinator` had exactly zero callers — the only mention
  in the tree was a comment in `auth_test.go` explaining why it could not be
  used. The package was 76 lines of provably unreachable test scaffolding and
  has been deleted; `auth_test.go`'s comment now states the general rule
  (in-package tests cannot share a helper from a package that imports them)
  instead of pointing at a package that no longer exists.

## Phase 4 — Deduplication and convention alignment (mechanical, well-bounded)

**internal/ui:**
- [x] **Three near-identical thread caches** (`model/threads_cache.go`,
  `threads_dock.go:125-235`, `thread_indicator.go:59-100`) — three
  `ListThreads` RPCs per thread event. One `threadListCache` with three
  projections; removes ~250 lines and 2 of 3 RPCs. (M)
  **Done 2026-08-21**: one `threadListCache` feeds three projections — the
  dashboard reads `cache.value`, the dock reads it through
  `activeDockThreads`/`visibleDockThreads`, and the header badge computes
  `activeThreadCount(cache.value)` with no cache of its own, so
  `thread_indicator.go` is deleted outright. 773 -> 570 lines across the three.
  The RPC saving is a test, not a claim:
  `TestThreadEventDispatchesOneListThreadsCall` drives one thread event through
  a `Root` with the dashboard open and asserts `listThreadsCalls == 1`;
  removing the in-flight suppression fails it, verified independently.
  **They were not copies — three real divergences, each resolved toward the
  more correct side, which is the point of doing this before phase 5:**
  (a) only the dashboard filtered task-kind pubsub events, so the other two
  re-fetched on task churn that could never appear in a threads-only list —
  filter kept, and removing it now fails `TestApplyThreadEventIgnoresTasks`;
  (b) only the dock and indicator called `ttlCache.fail` on error, so the
  dashboard recorded nothing and would redispatch immediately on the next
  stale check — the spin `threadsRefreshBackoff`'s own doc describes. The
  unified `staleRefreshCmd` honours `backingOff` and the fetched-and-empty
  short circuit, so that spin is now closed by construction rather than by an
  unwired backstop; (c) the "stop polling when empty" short circuit existed in
  two of three — kept.
  Per-thread live activity (`threadsDockState.activity`, AttachThread-based)
  was deliberately left separate: it is a per-thread fetch with its own TTL and
  cost profile, not one of the three duplicated list fetches.
- [x] **~20 tool renderers share one copy-pasted skeleton**
  (`chat/references.go`, `symbols.go`, `definition.go`, `rename.go`, …) with
  inconsistent unmarshal-error policy. Declarative
  `simpleToolRenderer{title, params, summary}` registered via the existing
  registry; collapses ~360 lines into a table. (M)
  **Done 2026-08-21**: the seven LSP-family renderers (`definition.go`,
  `references.go`, `symbols.go`, `rename.go`, `call_hierarchy.go`,
  `diagnostics.go`, `lsp_restart.go`, 309 lines) became table rows in a new
  `lsp.go` (116) over `tools_simple.go` (72) — net -121, -39%.
  The unmarshal-policy survey found **three** groups, not one inconsistency:
  the LSP family plus `replace_symbol.go` ignore the error; `search.go`,
  `fetch.go`, `mcp.go`, `question.go`, `generic.go` check it and render
  "Invalid parameters"; `docker_mcp.go` and `todos.go` check it and swallow it.
  The table adopts "ignore", with the boundary stated in a comment: these params
  feed only a cosmetic header, whereas glob/grep/fetch render bad input to avoid
  misrepresenting a possibly-destructive call. **No tool's behavior changed** —
  all seven already ignored it, so the rule was recorded, not introduced.
  `replace_symbol.go` stayed out: it uses full rather than capped width, checks
  `!opts.HasResult()` rather than `HasEmptyResult()`, summarizes a diff instead
  of a line count, and appends an error tail.
- [x] **Permission dialog re-implements the renderer registry as a 12-arm
  switch** (`dialog/permissions.go:609-790`; four diff arms byte-identical).
  Registry in the same style as `chat/tools_registry.go`; unify diff arms
  behind a `diffParams` interface. (M)
  **Done 2026-08-21**: `renderContent`'s switch became a name-keyed registry
  (`contentRenderer`, `contentRenderers`, `registerContentRenderer`, wired in
  `init()`) matching `chat/tools_registry.go`'s idiom; all ten dispatched tools
  are registered and the unregistered case still falls through to
  `renderDefaultContent`. The four diff arms share
  `diffContentRenderer[P any](toDiff func(P) diffParams)`, which performs the
  same `p.permission.Params.(P)` assertion per registration that each arm did
  before — so the concrete-type requirement from AGENTS.md is preserved rather
  than replaced by an interface. `diffParams` is a plain local struct, not an
  interface, because methods cannot be added to the aliased `proto.*` types.
  Line count 980 -> 979; honestly, near-flat, because gofmt splits each
  type-asserting closure across three lines.
  **A surviving mutation was found and closed.** Bypassing the type guard
  (falling through with a zero-value `P`) passed the whole suite: the existing
  `TestRenderContent_WrongParamsTypeYieldsEmpty` asserts only that output is
  empty, and a bypassed guard renders empty too — `renderDiff` on three empty
  strings produces nothing. Output cannot distinguish a working guard from a
  broken one; only *whether `toDiff` was reached* can. Added
  `TestDiffContentRenderer_GuardStopsBeforeToDiff`, which spies on `toDiff` and
  now fails under exactly that mutation. This guard is what stops a malformed
  permission request from rendering as a plausible-looking one, so leaving it
  unpinned was not acceptable.
- [x] **Finish the `ttlCache` migration** in `model/workspace_cache.go`
  (`promptQueue`, `agentReady`/`agentModel` still hand-rolled). (S)
  **Done 2026-08-21**: `promptQueue*` collapsed into
  `promptQueueCache ttlCache[[]string]`; `agentReady`/`agentModel` bundled into
  `agentCache ttlCache[agentReadyModel]`, deliberately still gated by the shared
  `busyFetchGen` because one workspace probe fetches busy+yolo+ready+model
  atomically and splitting the generation would break that. Real semantic
  difference found and preserved rather than flattened: `ttlCache.invalidate()`
  zeroes the timestamp, but several call sites (`send.go` esc-clear, `ui.go`
  session reset) only want the generation bump — they already know the new value
  synchronously and just need to reject an older in-flight fetch. Mutation
  dropping the generation guard in `complete` fails six named tests.
- [x] **One dialog side-effect convention**: some dialogs return `Action`s
  applied by `dialog_actions.go`, ten call sites reach through
  `com.Workspace` directly (including file writes from `Update` —
  `dialog/api_key_input.go:309`, `dialog/models.go:400` with its own FIXME).
  Pick "dialogs are pure view + Action", move the ten sites, document in
  AGENTS.md. (M)
  **Done 2026-08-21**, and the entry's framing needed correcting. Ten sites do
  reach through `com.Workspace` from `HandleMsg`, but only **two** of them do
  IO — the two file writes this entry names. The other eight are cheap
  in-memory reads of already-loaded state (`WorkingDir`, `Config`, `ThemeID`,
  `SupportsThreads`, `MCPGetStates`, `AgentIsSessionBusy`) that cannot block;
  `session_busy_test.go` already documents `AgentIsSessionBusy` as a lookup in
  the dispatcher's active-request map rather than IO. They were left alone and
  the convention text carves out that exception explicitly, rather than
  pretending a struct-field read is a side effect.
  The two real ones are converted: `api_key_input.go`'s `SetProviderAPIKey` now
  runs in `saveAPIKeyCmd`, with the result returning as a new
  `ActionAPIKeySaved`; `models.go`'s `setProviderItems` (which wrote pruned
  recent-models via `SetConfigField` from inside the `NewModels` constructor,
  carrying its own FIXME about it) now returns a `tea.Cmd`, so `NewModels` is
  `(*Models, tea.Cmd, error)` and `openModelsDialog` batches it.
  Convention written into `internal/ui/AGENTS.md` with the reasoning that makes
  it non-negotiable: `Update` *is* the render loop, so a synchronous workspace
  call stalls the whole TUI for its duration.
  Mutation: moving the API-key save back onto the Update goroutine fails
  `TestAPIKeyInput_WithModelReturnsActionSelectModel` with "HandleMsg must not
  call SetProviderAPIKey synchronously" — verified independently.
- [x] **`context.Background()` sweep**: ~20 model/ call sites plus OAuth
  polls that survive shutdown; `common.Common.Context()` exists for exactly
  this. Mechanical replace + a lint rule. (S)
  **Done 2026-08-21**: 31 call sites converted to `common.Common.Context()`,
  including all four OAuth polls — the actual bug, where quitting the TUI left a
  device-flow poll running against a remote endpoint until the code's hour-long
  expiry. `TestOAuthCopilotPollStopsOnShutdown` pins it; reverting that one poll
  to `context.Background()` fails it in exactly 3.00s (the test's timeout),
  verified independently.
  **Calling this "mechanical" in the plan was wrong** — a blind replace-all would
  have broken the case that matters most. `oauth_codex.go`'s `afterSave` persists
  config and fetches the model list *after* the handshake already succeeded; on
  the lifecycle context, a shutdown landing in that window silently drops a
  completed sign-in's model list, leaving a saved credential with no models. Left
  on `context.Background()` with a documented `//nolint:forbidigo`. Same for
  `common.Context()`'s own nil-Ctx fallback, which *is* what the rule points
  callers at.
  A pre-existing race was closed on the way: `oauth_copilot.go`'s `startPolling`
  created its cancel func inside the goroutine, so a `stopPolling` arriving first
  was a no-op.
  The lint rule (`forbidigo`, scoped to `internal/ui` via `path-except`, with
  `_test.go` exempt) was verified both ways by probe files: it fires on a fresh
  `context.Background()` in `internal/ui/common`, and does not fire on the same
  code in `internal/workspace`. Repo-wide `golangci-lint run` is 0 issues.
- [x] `model/mouse.go` recomputes `sessionPanelPlan` 4× per mouse event —
  one `panelHitTest(pt)` helper. Port `Models` (and then `Doctor`/
  `Providers`) onto `selectDialog`. `dialog.StandardListKeys()` for the 25
  hand-rolled keymaps. Truncation/padding helpers → `presentation`. (S–M each)
  **Two of four done 2026-08-21; the other two were measured and rejected,
  and both rejections correct this entry.**
  `panelHitTest` done — but there were **three** call sites, not four, and they
  sit in mutually exclusive message branches, so nothing was recomputed
  redundantly within one event; the win is one shared hit-test instead of three
  copies of plan + row-layout + hit-test. The mutation the item asks for (cache
  the plan, reuse it stale) **survived the whole suite**, because every existing
  test drives a single event on a fresh `UI`. Closed that myself with
  `TestPanelHitTest_ReflectsCurrentLayout`, which hit-tests the same `UI` twice
  across a thread-list change; the caching mutation now fails it and nothing
  else. That matters: a stale plan maps a click to the wrong thread.
  Truncation/padding done — `dialog/stats.go`'s local `truncate()` was doing
  rune-slicing rather than ANSI-safe truncation, against AGENTS.md's explicit
  rule; it and `threads_admin.go`'s `padTo` are now `presentation.Truncate` /
  `presentation.PadTo`, collapsing 13 call sites. `chat`'s
  `truncateToolParam`/`truncateHookName` were left alone — they are documented
  domain-named wrappers over `presentation.TruncatePathAware`, not duplicated
  logic.
  **`StandardListKeys()` rejected:** of ~24 keymap-bearing dialogs only
  `select_dialog.go` and `models.go` share the standard list shape, and
  `models.go`'s is the subject of the next sub-item — so the helper would have
  had at most one caller. Everything else differs for real: tab/shift-tab field
  navigation, vim-style 2D browsing, y/n confirms, toggle/quick-select,
  copy/skip/open auth actions. "25 hand-rolled keymaps" counted files, not
  duplication.
  **Porting `Models` onto `selectDialog` rejected as scoped:** `selectDialog.list`
  is concretely `*list.FilterableList` while `Models.list` is `*ModelsList` — a
  materially different type with grouped rendering, group-prefixed fuzzy
  filtering and navigation that skips non-item rows — and `Models` has an `Edit`
  (ctrl+e) binding producing a *different* Action for the same selected item,
  which `selectDialogConfig.onSelect(id) Action` cannot express. A real port
  needs `selectDialog`'s list generalized behind an interface, touching every
  current consumer with golden-file risk. That is a phase-5-sized item, not an
  (S–M) one.

**internal/agent:**
- [x] **File-mutation boilerplate** open-coded 8× across
  `tools/write.go`, `edit.go`, `multiedit.go`, `lsp_rename.go`,
  `lsp_replace_symbol.go` (confinement → staleness → diff → permission →
  history-write → filetracker → LSP-notify → diagnostics; ~350 duplicated
  lines, two divergent staleness checks). One `applyFileMutation(ctx, req)`
  in `tools/filemutation.go`. Also fix the sentinel-`ToolResponse` 5-value
  return in `edit.go:305 loadExistingFile`. (M)
  **Done 2026-08-21, for three of the five tools.** `write.go`, `edit.go` and
  `multiedit.go` now share `applyFileMutation` (diff -> permission -> history
  write -> filetracker record) in the new `filemutation.go`; those three files
  went 1014 -> 892 lines (-122), and with the 138-line helper the production
  ledger is +16 overall. The mechanical duplication is genuinely gone; the count
  is flat because Go's explicit-struct style plus the new types offset it.
  Metadata stays a per-caller closure on purpose: `WriteResponseMetadata` /
  `EditResponseMetadata` / `MultiEditResponseMetadata` must stay distinct
  concrete types for the permission-dialog type assertions in
  `ui/dialog/permissions.go` (see AGENTS.md).
  **`lsp_rename.go` and `lsp_replace_symbol.go` were deliberately NOT
  converted** — a judgment I accept. They have no confinement check, no
  staleness check, an *optional* permission request, and `lsp_rename` mutates
  many files through an LSP workspace edit rather than one old/new pair.
  Routing them through the helper would mean either silently adding confinement
  and staleness checks they do not have today — a real behavior change — or
  filling the shared helper with conditionals until it stops guaranteeing "the
  same order for every caller", which is the only reason it exists.
  **The two divergent staleness checks, resolved:** both compared
  `ModTime().Truncate(time.Second)` against `filetracker.LastReadTime`, but
  `edit.go` special-cased "never read" while `write.go` folded it into
  "modified after read" — and since `modTime.After(zeroTime)` is always true, an
  unread file was refused by `write` too, with the nonsensical message
  "last read 0001-01-01T00:00:00Z". Unified on `edit.go`'s version in
  `checkFileFreshness`. Only `write`'s *message* for a never-read pre-existing
  file changes; the write was refused before and is refused now.
  `loadExistingFile`'s 5-value sentinel became `(existingFileResult, error)`
  with precondition stops carried by a typed `*mutationStop` error unwrapped via
  `errors.As` — "this call already produced the answer" is now in the type
  system instead of a forgettable `resp != zero` check.
  **Security-relevant mutations verified independently, not taken on report.**
  Ignoring the permission denial (`if denied && false`) fails
  `TestWriteTool_PermissionDeniedDoesNotWriteFile`; making `confinementRefusal`
  always allow fails four named tests including all three tools'
  `ConfinedWorkspaceRefusesAnAbsolutePathOutside`. Method note for the next
  reviewer: my first attempt at the permission mutation *appeared* to survive
  with "0 failures" — it had actually failed to compile, and grepping for
  `--- FAIL` cannot tell a clean build from a broken one. Always assert the
  build succeeded before reading a mutation as survived.
- [x] **Tool error-handling rule**: no stated rule for
  `NewTextErrorResponse` vs returned `error` (compare `read.go:186` vs
  `ls.go:81`); ~40 differently-worded "X is required" strings. Document:
  model-recoverable → text response; infrastructure → `%w`-wrapped error.
  Add `tools.invalidParam(name)`. (M)
  **Done 2026-08-21**: the rule is in AGENTS.md, and it is grounded in what the
  provider library actually does rather than in taste — verified independently
  in `charm.land/fantasy@v0.40.0/agent.go:768-774`: a Go error from a tool sets
  `isCriticalError`, and the batch returns `nil, err`, **discarding every other
  tool result in that batch**. A `NewTextErrorResponse` is a normal result the
  model can react to. So the choice is not cosmetic: it decides whether one bad
  argument kills the whole parallel tool-call batch.
  `invalidParam(name)` and `missingSessionID(action)` now serve ~40 call sites.
  Four contradictions fixed, each pinned by a test: `todos.go` invalid-enum and
  `fetch.go`/`download.go` request-creation and `client.Do` failures became text
  responses (fetch's sibling `web_fetch.go` already handled the identical
  failure that way); `ask_parent.go`'s missing session ID became a Go error like
  every other tool's. Reverting the todos fix fails
  `TestTodosTool_InvalidStatusIsTextResponseNotError`, verified independently.
  **The `read.go:186` vs `ls.go:81` pair that motivated this entry turned out
  not to be an inconsistency.** `ls.go`'s `fsext.Expand` failure is a shell-syntax
  error in the model's own `path` argument — recoverable, correctly a text
  response. `read.go`'s remaining `os.Stat` error, after ENOENT is handled
  separately, is `EACCES`/`ENOTDIR`/IO — an environment condition the model's
  next call cannot fix. They only look alike; under the rule they land on
  opposite sides for different reasons. Both left as they were.
  Local-filesystem failures in `download.go` (mkdir/create/write) stayed Go
  errors deliberately — unlike the network half, they are process-local infra.
- [x] `tools/bash.go`: replace the 100ms poll ticker and the fixed 1s sleep
  with a `Done()` channel on `shell.BackgroundShell`; drop the `panic` on nil
  manager (coordinator already returns an error for it). (S)
  **Done 2026-08-21**: no new completion machinery was needed — `BackgroundShell`
  already had a private `done` channel closed by `defer close(...)` in the single
  goroutine `Start` spawns, so the change was to expose it as `Done()`. Ordering
  is safe by construction: `exitErr` and `completedAt` are assigned in the
  goroutine body *before* the deferred close, so anything observing `Done()` sees
  the final state. Exactly-once is enforced by Go itself — a second close panics,
  so a bug here fails loudly rather than silently. `Done()` is indifferent to
  context; `bash.go` races it against `ctx.Done()` in its own select.
  The nil-manager panic was dead code: `coordinator.go:313-315` returns
  `errBackgroundShellsRequired` before any tool is constructed.
  Latency claim measured, not asserted: against the pre-change `bash.go` the new
  `TestBashTool_RunInBackgroundReportsCompletionWithoutSleeping` fails at exactly
  1.000s (the fixed sleep); after, it completes well under 500ms. Verified the
  same independently by making `Done()` return a fresh unclosed channel — the
  test fails at 1.000243695s, falling back to the old timeout.
  One test assertion was removed by design: `background_di_test.go` asserted the
  panic this item deletes. `NewJobOutputTool`/`NewJobKillTool` keep theirs — they
  have no equivalent coordinator-level guarantee.

**internal/config:**
- [x] **Provider merge/validate duplication** (`providers_merge.go` 337 L /
  `providers_validate.go` 150 L): identical header-resolution, proxy, and
  drop-provider blocks; inconsistent Warn-vs-Problem-vs-silent policies.
  Extract `resolveProviderHeaders`, `resolveOptionalProxy`, `dropProvider`;
  split `mergeCatalogProviders` into override/vendor/credentials passes.
  Also the typo "there are no custom providers are configured". (L)
  **Done 2026-08-21**: `providers_shared.go` (73 lines) holds
  `resolveProviderHeaders`, `resolveOptionalProxy` and `dropProvider`;
  `mergeCatalogProviders` split into `mergeProviderOverride` (a pure transform),
  `applyProviderVendorSetup` and `applyProviderCredentials`. Typo fixed (no test
  asserted on it). Line accounting, stated honestly: the two original files went
  487 -> 479, but the new shared file adds 73, so the three-file total is
  487 -> 552. The criterion as written ("across the two files") was met; the
  total went up, and centralising three duplicated blocks behind documented
  parameters is worth it.
  Most of the suspected policy divergence turned out **not** to exist: header
  resolution, empty-header dropping and optional-proxy handling were already
  byte-identical in both files. One divergence is genuinely deliberate and is
  now an explicit `dropProvider` parameter — a provider dropped because the user
  set `disable: true` logs at `slog.Debug` and records no doctor `Problem`,
  since the user asked for it. **Two accidental drifts were found, reported and
  deliberately NOT fixed** (both in TECHDEBT.md): catalog-provider drops attach
  a `Hint` to their `Problem` while custom-provider drops of the same shape do
  not; and a discovery-triggered empty-models drop records no `Problem` at all
  while the generic empty-models drop two branches below records one.
  Mutation `dropProvider` into a no-op that logs but never deletes: 19 tests
  fail. Header-swallow and proxy-passthrough mutations each fail a named test.
- [x] **`Load` vs `reloadFromDisk`** are two copies of the same 13-step
  pipeline — extract `buildConfig(...)`; the "fallback not persisted on
  reload" divergence becomes an explicit flag. (M)
  **Done 2026-08-21**: new `internal/config/build.go` holds `buildConfig` /
  `buildConfigOptions` / `builtConfig`; both callers are thin wrappers. Six
  divergences turned into explicit options (`persistFallback`, `ctx`, `dataDir`,
  `migrateModelCache`, `presetModel`, `debug`); two were accidental drift — the
  `ValidateHooks` / `applyEnvironmentDefaults` ordering (verified to touch
  disjoint fields, unified on `Load`'s order) and cosmetically different error
  wording (unified; untested, but an observable log-text change).
  **A real shipping bug was surfaced and deliberately NOT fixed:** only `Load`
  ever set `Options.Debug`, so a process started with `--debug` silently loses
  provider HTTP debug logging on its first config reload. Reproduced exactly via
  `buildConfigOptions.debug` and written up in TECHDEBT.md — fixing it changes
  shipped behavior and deserves its own decision.
  The `persistFallback` divergence turned out to be **untested**: the mutation
  making reload persist too passed the whole suite, so
  `TestReloadFromDisk_DoesNotPersistFallbackCorrection` was added to pin it
  (289 tests, up from 288 — the only intended change to the test set).
  **Line-count criterion missed, accepted:** `load.go`+`reload.go` shrank
  789 -> 701, but `build.go` adds 139 for a net +51. The excess is the
  divergence documentation this entry asked for, which previously did not exist
  because the differences were implicit. Documenting six divergences is worth
  51 lines.
- [x] `resolve.go`: four copies of `Resolved*` → `resolveSlice`/`resolveMap`
  (~110 of 368 lines). `applyPendingDiskActions` re-implements
  `writeConfigFields` — split out a no-reload core. `Init()` is a no-op
  wrapper over `Load` with 42 call sites — pick one entry point; move the
  project-bootstrap half of init.go out. Problem constructors in one place so
  severity/hints can't drift; move `ValidateHooks` next to `HookConfig`.
  Attribution migration → `internal/config/migrate`. `modelcache.go`: stop
  re-running DDL on every call. (S–M each)
  **Five of seven done 2026-08-21.** `resolveSlice`/`resolveMap` replace the four
  `Resolved*` copies, keeping each site's distinct error quoting and empty-value
  policy as constructor parameters. `providerProblem`/`providerDropProblem` in
  `providers_shared.go` now serve all 8 call sites so severity and area cannot
  drift — the two documented drifts from TECHDEBT.md were reproduced, not fixed.
  `ValidateHooks` and `normalizeHookEvent` moved next to `HookConfig`.
  `modelcache.go`'s DDL is once-only **per database file** (mutex + map keyed by
  `dbPath`, marked done only after a successful `ExecContext` so a transient
  failure retries instead of wedging the cache); the process-global version of
  that guard fails four named tests, verified independently.
  A stale comment was disproved along the way: `applyPendingDiskActions` carried
  an inline copy of the sort-and-`sjson.Set` loop justified by "calling
  `writeConfigFields` would trigger a recursive reload". Only `SetConfigFields`
  reloads; `writeConfigFields` does not. The two implementations were
  byte-identical, so the copy was replaced by a direct call with no behavior to
  reproduce.
  **Both remaining pieces done 2026-08-21.** `Init` was checked before being
  removed rather than on my say-so — it genuinely did nothing but call `Load`
  and return its result: no globals, no memoization, no ordering, no extra
  validation. So it is deleted outright rather than kept as an alias, and all
  26 call sites now call `config.Load` (repo-wide `grep 'config\.Init('` is
  empty). `internal/config/init.go` -> `project_init.go`, still in the package
  because it needs the unexported `defaultContextPaths`; the attribution
  migration moved to `internal/config/migrate` as
  `Attribution(current, coAuthoredBy, assistedBy, none)`, taking plain strings
  because `migrate` is a leaf that must not import `config` (which imports it).
  Mutation: dropping the `co_authored_by=false -> none` mapping fails
  `TestAttributionMigration/old_setting_co_authored_by=false_migrates_to_none`,
  verified independently; skipping the workspace-config layer in `buildConfig`
  fails `TestLoad_WorkspaceLegacyRecentModelsPreservesSiblingFields`.
  The `--debug`-lost-on-reload bug in TECHDEBT.md was left exactly as it was;
  only the function name in its writeup was updated to `config.Load`.
- [x] `shellconfig`: parameterize `applyFlags`' start index (kills the
  fake-argv padding in `modelSet` and the `hookRemove` map abuse); make
  `options.go handleOption`'s special cases (`ui`, attribution keys) table
  rows via `enum`/`path` fields on `optionSpec`. (M)
  **Done 2026-08-21**: `applyFlags` takes an explicit `start`; the fake-argv
  padding in `modelSet` is gone (it passes 2, since bare `model <provider/id>`
  has a two-token prefix, while every other caller passes 3). Shifting that
  index back to 3 fails five named tests — verified independently.
  `optionSpec` gained `enum`, `path`, `implies` and `nonNegative`; the two
  `attribution-*` special cases and `optionUI`'s whole switch became table rows
  over a shared `setOption`. `keybinding` stays bespoke — it is variadic and does
  not fit a single-value spec. A `shorthand` flag preserves a real pinned
  difference: top-level bools accept the bare form (`option debug` => true)
  while `option ui compact`/`transparent` require an explicit value.
  Production net -4 lines; the diff is mostly restructuring, not deletion.
  **The `hookRemove` "map abuse" in this entry was wrong.** Its 3-token prefix
  (`hook remove EVENT`) already matched the fixed offset, so it was never an
  offset workaround — the scratch map is the same legitimate pattern `hookAdd`
  uses to give `applyFlags`' generic map target somewhere to put a single
  `--name` flag. Nothing to delete; left as it was.
  **None of the eight options this entry touches had any test at all.** 14
  pinning tests were written first, confirmed passing against the *unrefactored*
  code, and only then was the refactor made — the right order for user-facing
  input parsing, where a changed binding makes an existing `sennitrc` silently
  mean something else. Dropping the `scrollbar` row's enum fails
  `TestOption_UIScrollbarInvalid`, one of those new tests.

**internal/cmd:**
- [x] Three copies of the session-ID resolver, four copies of the threads
  command prologue, six copies of the `ResolveCwd`+`config.Init`+`db.Connect`
  bootstrap, five package-level `--json` bools. One `cmdutil` file:
  `withQueries`, `resolveSession`, `requireThreads`, `emitJSON`; `--json`
  into per-command option structs. (M)
  **Done 2026-08-21**: `internal/cmd/cmdutil.go` (142 lines) now holds
  `cmdContext`, `initConfig`, `connectDB`, `emitJSON` and one `resolveSessionID`
  over a narrow `sessionByIDLister`, with a `workspaceSessionLookup` adapter for
  `workspace.Workspace`'s differently-named methods. `requireThreads` collapses
  the four threads prologues. The `--json` count was **seven**, not five
  (`session.go` x5 plus `doctorJSON` and `threadsListJSON`); all moved to the
  `cmd.Flags().GetBool("json")` style already used by `gc`/`stat`/`projects`.
  Files owned outright: +80/-259. `dirs.go` deliberately left alone — it needs
  only `ResolveCwd`, and folding it into `initConfig` would add an unwanted
  config load.
  **Observable change, accepted:** unifying the resolvers means `run.go`'s and
  `root.go`'s ambiguous-prefix error switched from a one-line message to
  `session.go`'s git-style listing of matches. No test pinned the old text; the
  new one is strictly more informative.
  **`threads.go:72/73` verdict: not a bug.** Both commands declare their own
  `--json` under the same name and cobra resolves each invocation's own flag
  set, so the shared `threadsListJSON` var is harmless today; it would only
  become a bug if the two commands' behavior diverged.
- [x] Move `sennit gc` out of the CLI into `internal/gc` (or `db/gc.go`)
  behind `gc.Run(ctx, deps, policy) (Report, error)`; replace string-built
  SQL with the existing generated queries; add worktree teardown (phase 0
  item). Add `db.InTx(...)` and use it here and in `session.Service.Delete` /
  `history`. (M)
  **Done 2026-08-21**: `gc.Run(ctx, Deps, Policy) (Report, error)` in the new
  `internal/gc`, with `Collect`/`Delete`/`Vacuum` beneath it; `internal/cmd/gc.go`
  459 -> 219 lines and now only parses flags and prints. The three hand-built
  `SELECT COUNT(*) ... WHERE session_id IN (...)` became generated queries
  (`CountMessagesForSessionIDs`, `CountFilesForSessionIDs`,
  `CountReadFilesForSessionIDs`) following `ListMessagesBySessionIDs`'s
  `json_each` pattern — which also **deleted the 500-ID chunking loop**, since
  a single JSON-array parameter has no parameter-count limit to work around:
  always 3 calls, whatever the selection size. Verified after regeneration that
  `ListThreadsForGCRow` still carries `Kind`, `WorktreePath` and `Branch` — the
  phase-0 columns a previous agent once lost this way.
  `db.InTx` added and adopted in `gc` and `session.Service.Delete`.
  **A missing transaction was found:** `history.Service.DeleteSessionFiles`
  deleted files one at a time with no transaction at all — not deliberate, just
  never wrapped; now one `InTx`, with pubsub events published only after commit.
  `CreateVersion` was deliberately left alone: its bespoke `fileVersionStore`
  transaction exists to serialize `NextFileVersion` + `CreateFile` against
  concurrent version allocation, which the generic helper does not replace.
  Reporting-only for orphaned worktrees is intact. The dry-run mutation fails
  `TestRun_DryRunMakesNoWrites` plus both CLI dry-run tests — verified
  independently, since a dry run that writes is the one failure here that would
  destroy user data.
  Note: `internal/cmd/gc_test.go` reaches unexported CLI identifiers, so a
  test-only `gc_compat_test.go` forwards those names to the new package rather
  than editing the safety net; production `runGC` calls `gc.Run` directly.

## Phase 5 — Structural decomposition (largest, do last, needs phase 3)

**God objects, in dependency-safe order:**

1. [x] **Drop the stale `any`-typed thread/task manager seam** in `app`
   (`app.go:57-76,445-470`). ~~the import cycle it works around no longer
   exists (verified via `go list -deps`)~~ — **this premise is WRONG and the
   item is blocked. Attempted and reverted 2026-08-21.** (S/M)
   `go list -deps` is clean in both directions, so there is no *production*
   cycle — that much of the claim held. But seven of `internal/thread`'s own
   test files are `package thread` (in-package, not `thread_test`) and import
   `internal/app`: `fakes_test.go` (822 lines), `discard_merged_test.go`,
   `manager_delivery_test.go`, `steer_test.go`, `test_adapters_test.go`,
   `tasks_test.go`, `task_wiring_test.go`. Verified independently.
   The moment `app.go` names `*thread.Manager` in production code, Go's test
   compilation turns that into a cycle: `go build ./internal/thread/...` still
   succeeds, but `go test ./internal/thread/...` fails outright with "import
   cycle not allowed in test" — the whole thread suite dies. So `go list -deps`
   was the wrong check; it cannot see test-only edges.
   **Prerequisite DONE 2026-08-21 — this item is unblocked.** All of
   `internal/thread`'s app-importing tests are now `package thread_test`; only
   `store_test.go` (which has no `app` dependency) stays in-package. The scope
   was larger than the seven files: `fakes_test.go`'s scaffolding is used by
   every other test file, so they all moved together. Test-name set verified
   identical, 108 before and after. Rather than exporting internals wholesale,
   narrow `...ForTest` seams were added (`SetStatusForTest`, `RuntimeForTest`,
   `DiscardMergedForTest`, `ResolveDeliveryTargetForTest`, `WorktreeDirForTest`,
   `ShutdownStartedForTest`, `NewStoreForTest`) — the `lifecycle`/`threadControl`
   types stay unexported, and the two task-cap constants got exported *aliases*
   so their "deliberately not configuration" comment still holds. The fakes
   themselves needed no export at all once their file became `thread_test`.
   Verified independently: adding `import "internal/thread"` plus
   `var _ *thread.Manager` to `internal/app/app.go` now builds **and**
   `go test ./internal/thread/...` passes. The cycle is gone.
   **Item DONE 2026-08-21 on the second attempt.** `threadManager` is
   `*thread.Manager` and `taskManager` is `*thread.TaskManager`; every runtime
   assertion for them is gone — `SetPermissionsSkip`'s
   `permissionsSkipPropagator` (that one-method interface is deleted outright,
   since `*thread.Manager` already has the method),
   `workspace/threads.go`'s and `tasks.go`'s accessors, seven assertions in
   `threadspawn/attach_test.go` and two in `tasks_appworkspace_test.go`.
   Repo-wide grep for `.(*thread.Manager)` / `.(*thread.TaskManager)` is empty.
   Net -9 lines; `go test ./internal/thread/...` passes.
   The obsolete import-cycle rationale is gone, but three live facts were kept
   on the newly typed field: why this field exists separately from
   `Threads`/`Tasks` at all (workspace needs the richer API, the `tools.*`
   seams are narrower), that it is nil until wired post-bootstrap, and that it
   is set independently of `SetThreads`/`SetTasks`.
   One test was replaced rather than kept, and the replacement is stronger:
   the old propagation test used a hand-rolled `recordingSkipPropagator` fake
   that cannot compile against a concrete type and only checked that the
   interface got called. `TestAttach_SetPermissionsSkipReachesLiveThread` now
   wires a real manager through `Attach`, spawns a live thread and asserts
   `SetPermissionsSkip` reaches that thread's own `Permissions().SkipRequests()`.
   Unwiring `SetThreadManager` in `attach.go` fails seven tests — verified
   independently.
   What was kept from the attempt: the doc comments on `app.go`'s
   `threadManager` field and on `thread_workspace.go` claimed the reason was
   "internal/thread imports internal/app", which is false, and implied the
   untyped seam was a preference rather than a forced constraint. Both now
   state the real reason, so the next attempt starts from an accurate premise.
2. [x] **`workspace.Workspace` 93-method interface**: make
   `readOnlyWorkspace` embed `Workspace` and override only the ~25 mutating
   methods (the `attachedThreadWorkspace` pattern) — deletes ~450 lines of
   stubs and the "forgot a stub" bug class; then split the interface into the
   sub-interfaces consumers actually need. Decide `proto`'s fate: it survives
   only as an in-process round-trip for the TUI. (M/L)
   **Embedding done 2026-08-21.** `readOnlyWorkspace` embeds `Workspace`;
   613 -> 483 lines. The 34 pure passthroughs are gone; what remains is the 47
   refused methods plus 12 reads that need real scoping logic — so the "~450
   lines of stubs" in this entry was an overcount: only the passthrough third
   was ever removable by embedding.
   **The safety inversion this creates is closed, which was the condition.**
   Embedding turns a forgotten stub from a compile error into a silent
   forward — worse, since read-only is a safety property. Two tests fix that:
   `TestReadOnlyWorkspace_MethodClassificationIsComplete` reflects over
   `Workspace`'s method set and fails unless every method is in exactly one of
   `refusedMethods` (47) / `readOnlySafeMethods` (46) — they sum to 93; and
   `TestReadOnlyWorkspace_RefusesEveryMutatingMethod` is table-driven over the
   refused list, asserting both the refusal *and* `stub.calls[name] == 0`, so
   it proves the call never reached the embedded workspace rather than merely
   that an error also came back.
   Verified independently: deleting an override lets the mutation forward and
   fails the behavioral test; adding an unclassified method to the interface
   fails the classification test with a message naming the method and telling
   the next person what decision to make.
   Two classifications were judgment calls, both resolved conservatively:
   `Shutdown` (writes nothing, but promoting it would tear down the parent
   workspace behind a read-only view) and `Subscribe` (promoting it would hand
   the caller's program the live parent's event stream, risking duplicate
   delivery into a screen that is not supposed to be live). Both refused.
   **Closed 2026-08-21, and the premise was stale.** There was no 93-method
   monolith to split: `HEAD`'s `Workspace` is already composed of fourteen role
   interfaces (`SessionStore`, `AgentController`, `ConfigAccessor`,
   `ThreadController`, ...). Verified directly against `HEAD`. What remained was
   the part that makes a split real — consumers declaring the whole union for a
   handful of methods — and eight were narrowed: `cmdutil`'s session lookup and
   `resolve_session.go` to `SessionStore`, `threads.go` to `ThreadController`,
   `login`/`login_codex`/`logout` and both OAuth dialogs to `ConfigAccessor`.
   `Common.Workspace` and `run.go`'s helpers stay on the union: no single role
   fits them and no other caller needs that composition, so a new interface would
   be indirection nobody uses.
   **`proto`'s fate, answered with evidence and left for a separate decision:**
   only `proto.Thread` is a live DTO. `proto.Message`, `AgentEvent`,
   `PermissionRequest`, `PermissionNotification`, `ServerNotice`, `RunComplete`,
   `ConfigProviderKeyRequest` and `LSPClientInfo` are never constructed in
   production code. More importantly **AGENTS.md's stated rationale for the
   exceptions no longer matches the code**: `tools.*PermissionsParams` is
   documented as an alias because consumers assert the concrete type "after a
   JSON round trip", but there is no round trip — the params travel in-process
   through pubsub channels and the assertion works on Go type identity. There is
   no server/client mode in the tree at all. Recommend dropping the dead types
   and rewriting that AGENTS.md section; not done here.
3. [x] **`sessionAgent.run`** (`agent/agent.go:398-871`, 476 lines): extract
   `dispatchDecision`, `buildStreamAgent`, `finishTurn`; replace the tail
   recursion over the queue with a `for` loop (fixes the documented
   named-return clobbering and unbounded stack growth). Make
   `completionReporter` the single owner of RunComplete publication (today
   the "who owes the terminal event" contract lives in ~120 lines of prose
   across five places). Split `prepareStep` (`turn.go:105-295`) into
   `foldCompletions`/`foldSteering`/`applyCacheControl`/`createStepAssistant`
   with an explicit rollback list; extract a table-testable
   `classifyStreamError` from `handleStreamError` (and replace the hardcoded
   Copilot message-string match). (L)
   **Done 2026-08-21, all five pieces, each verified before the next.**
   `run` is **469 lines -> 12**: a `for` loop over `runTurn` (163), which calls
   `dispatchDecision` (134) and `buildStreamAgent` (45), with `finishTurn` (104)
   owning the post-Stream tail. The tail recursion is gone, and with it both
   documented defects: each hop is now its own frame with its own named returns
   and defers, so a later hop cannot clobber an earlier one's return values, and
   stack depth no longer grows with queue length.
   **The RunComplete contract is now structural rather than prose.**
   `publishRunComplete` is unexported and has exactly **one** caller in the
   package — `completionReporter.publish` — which I verified directly by grep.
   Every terminating path (cancel-on-entry, the streaming defer, `finishTurn`'s
   publish and its suppress, the canceled-queue-drop publishes in `queue.go`)
   goes through a reporter whose `sync.Once` makes "at most one emission per
   call" true whichever branch fires. Removing that `Once` fails three named
   tests including `TestRun_AutoSummarizeContinuationClearQueueCompletesOnce` —
   verified independently.
   `prepareStep` split into `foldCompletions` -> `foldSteering` ->
   `applyCacheControl` -> `createStepAssistant` with an explicit `rollback`
   list. `foldSteering` deliberately does *not* join that list: a persisted
   follow-up is durable the moment `createUserMessage` succeeds, so there is
   nothing for a later stage to undo; it rolls back its own partial batch inline.
   `classifyStreamError` (new `stream_error.go`) replaces the exact-string match
   on Copilot's `"The requested model is not supported."` with a case-insensitive
   substring match over a documented phrase list, table-tested against the
   original wording, a rewording, different casing, an unrelated provider error
   and an empty message — so a provider editing its copy no longer silently
   changes behavior.
   Note for the record: the "pre-existing MCP data race" this step reported was
   **mine** — its race run overlapped the window in which I had deliberately
   removed `publishMu` from `owns()` for a mutation test. `mcp` is clean.
4. [x] **Agent dispatcher** (`dispatch.go`, 7 maps + 3 mutexes, three
   coexisting cancellation signals): collapse per-session state into one
   `sessionState` struct in a single `csync.Map`; delete `queue.go`'s 242
   lines of pass-through; refcount the per-session mutex to fix the
   acknowledged leak at `dispatch.go:30-40`. (L)
   **Done 2026-08-21.** One `sessionState` per session in a single
   `csync.Map`; `queue.go` (237 lines) deleted, its pass-throughs promoted by
   embedding. The mutex leak is fixed by a refcount whose invariant is stated
   on the type: a `*sessionState` is dropped only when `refs == 0` **and**
   `idle()` — refcount alone is not enough, since the data must outlive a
   moment of being unreferenced. `acceptedRuns`/`cancelMark` are deliberately
   under a *global* mutex rather than the session's own, because
   `dispatchDecision` touches them while already holding `s.mu` and a
   `sync.Mutex` is not reentrant.
   **The three cancellation signals are genuinely distinct**, proven rather
   than assumed: (1) the in-flight turn's `CancelFunc`; (2) `cancelMark`, a
   high-water accept sequence poisoning runs accepted-but-not-yet-active; (3)
   `cancelled`, "the user canceled this session", read only by the wake check.
   Collapsing (3) into (2) fails
   `TestDeliverTaskCompletion_CancelledParentDoesNotAutoStart`, because
   cancelling an *idle* session never raises the mark.
   `TestDispatcher_SessionStateReclaimedWhenIdle` runs 50 sessions and asserts
   the map empties.
   **On the atomicity test, which took a round trip and corrected me.** My
   first mutation — `Lock/Unlock/Lock` at the top of `dispatchDecision` —
   survives `-race -count=4`, and I sent the step back over it. The agent was
   right that this mutation is *inert*: nothing is read from session state
   before that point, so there is no stale value for the churn to poison. (Its
   own claim that a `runtime.Gosched()` in the same gap would be caught did not
   reproduce — I ran it three times, all passing.) The invariant that actually
   matters is check-then-act, and the committed
   `TestDispatchDecision_AcceptIsAtomicUnderConcurrentRuns` does pin it:
   dropping the lock between the `s.active != nil` check (`agent.go:480`) and
   the `s.active = ac` write (`:554`) fails it deterministically, 3 runs of 3.
   Lesson worth keeping: a surviving mutation is only evidence when the
   mutation would really break the invariant. Mine did not.
5. [x] **`coordinator.buildTools`** (227 lines): registry table
   `[]toolSpec{Name, Gate, Build}`; snapshot config once per build instead of
   21 live `c.cfg.Config()` reads (also a live-mutation hazard); introduce a
   narrow `agentConfig` interface so `agent` stops drilling into `config`
   internals. (M)
   **Done 2026-08-21**: `buildTools` 222 -> 108 lines over a `toolSpecs()` table
   in the new `tool_registry.go`, with `agentConfig` (11 methods) implemented by
   a `configSnapshot` read once per build.
   **The mid-build reload race is real, not theoretical** — the agent traced it
   and I accept the trace: `ConfigStore.Config()` takes `configMu.RLock` per call
   and releases it immediately, nothing in `UpdateModels -> runtimeFor ->
   buildTools` holds a lock across the work, and `reloadFromDisk` only takes
   `writeMu` for the final `setConfig` swap while its disk read, catalog refresh
   and model-discovery HTTP calls run unlocked. `WatchForExternalChanges` polls
   on its own timer, independent of any turn. So a swap landing between two of
   the old 21 reads could build one tool set from two different `*config.Config`
   values.
   Two config reads were deliberately **left live**: `webSearchBackend` is also
   called at tool-invocation time from `agentic_fetch_tool.go`, and
   `backgroundAgentsEnabled` carries an existing "read fresh on every call —
   never cached" comment because the background-dispatch path calls it live.
   Snapshotting either would make a runtime caller stale.
   `TestBuildToolsPinnedSet_Coder`/`_SubAgent` now pin the exact tool-name lists.
   Verified independently with two security mutations: dropping the `interactive`
   half of the `question` gate fails the coder pin, and inverting `ask_parent`'s
   gate fails both pins. The agent also caught itself dropping `c.interactive`
   from that same gate on its first pass, by re-reading the table against the
   original before running tests.
   **Line count went up and I am recording that rather than burying it:**
   `buildTools` shrank 222 -> 108 (-107 across tracked files) but the new
   `tool_registry.go` (188) and `agent_config.go` (81) add 269, plus a 139-line
   pin test. The overhead is new decoupling machinery that did not exist in any
   form before, not duplicated logic — but the stated bar was not met.
   `GetMCPTools`' per-server loop and the user-defined-agent loop stay outside
   the table: both are dynamic, not fixed rows.
6. [x] **MCP `Registry`** (12 sync primitives in one struct, `Close` is 100
   lines, core methods 0% covered): split into `Registry` (public API +
   catalog snapshot), `connectionManager` (per-server lifecycle), and
   `authCoordinator`; per-server mutex maps become fields on a per-server
   struct. (L)
   **Partially done 2026-08-21.** The per-server mutex *maps* half is done and
   is the part that was structurally wrong: `renewMusMu`+`renewMus` and
   `suppressMus` were two separate hand-rolled per-server-lock mechanisms; they
   are now one `locks *csync.Map[string, *serverLocks]` where
   `serverLocks{renew, suppress sync.Mutex}` makes the granularity structural
   instead of conventional. Same accessors, no call-site changes.
   Coverage 63.9% -> 73.0%, with the flagged core methods off zero:
   `Initialize`/`CatalogSnapshot`/`GetStates`/`Version` 100%, `PendingAuthMCPs`
   90%, `MCPAuthURL` 83%, `RefreshTools` 80%, `reconcileOnce` 75%,
   `Reinitialize` 69%, `RunTool` 63%. Tests were written against the
   *unmodified* Registry first, as instructed, using the package's existing
   in-memory-session fake so nothing spawns a real MCP subprocess.
   The 12-primitive enumeration found **no primitive guarding nothing and no
   two guarding the same field** — the closest pair, `renewMus`/`suppressMus`,
   guarded *different* per-server locks through *duplicated* infrastructure,
   which is what got merged. Lock nesting is consistently
   `publishMu` -> `catalogMu`, never reversed.
   **Split completed 2026-08-21.** `connectionManager` (session create/renew/
   reconnect, reconcile, `InitializeSingle`) and `authCoordinator` (`BeginAuth`,
   `AuthenticateMCP`, `MCPAuthURL`, flow lifecycle, `oauthSetup`) are new types,
   each holding a `reg *Registry` back-reference and embedded anonymously so
   every existing call site keeps compiling by promotion.
   The lock question was answered by *not moving the lock*: `publishMu` and every
   owns-check-then-commit method (`owns`, `beginAttempt`, `publishOrClose`,
   `teardown`, ...) stay on `Registry`. Verified structurally — neither new file
   references `catalogMu` at all except in comments, so `publishMu -> catalogMu`
   is entered only from `Registry`-receiver code and cannot be reversed from the
   new types by construction rather than by convention.
   **Caveat, recorded in TECHDEBT.md:** the first full-repo `-race` run after
   this split failed `TestBeginAuth_CancelSettlesExactStartingOwner` with a nil
   dereference. It has not reproduced (second full race run, `-count=6`,
   `-cpu=1,2,8`, and isolated runs all clean) and no DATA RACE was reported, so
   it is written up rather than diagnosed — but it is a failing test, not a
   benign one.
   Superseded note: the three-way split into `Registry`/`connectionManager`/
   `authCoordinator` was deliberately not attempted, and I accept the reason.**
   `publishMu` is genuinely cross-cutting: it protects `owners`,
   `sessionOwners`, `closing`, `tokenReservations` and `tokenWrites`, and every
   "does this attempt still own the server" check must be atomic with the
   catalog/session write that follows it — that is exactly what
   `publishOrClose`'s owns-check-then-commit depends on. Splitting it without
   re-deriving every call site's ordering is the deadlock hazard the item warns
   about. Verified the lock is load-bearing by removing it from `owns()`:
   `-race -count=4` reports a DATA RACE and `TestOwns_RaceAgainstBeginAttempt`
   fails.
   `Close`'s ordering, for whoever attempts the split: refcount out; then under
   `publishMu` set `closing`, bump every server's generation, snapshot-and-clear
   sessions/authURLs, and under nested `catalogMu` clear the catalogs and bump
   the version; then outside the lock abort auth flows, close sessions
   concurrently, wait for pending token writes, shut down the broker. The
   generation bump is what makes concurrent session close safe against a
   connect still in flight — a later `publishOrClose` sees the bumped
   generation and discards its session.
7. [x] **`app.App`** (46 fields, 5 cleanup slices, 230-line 6-phase
   `Shutdown`): extract `appServices`, `appEvents`, and a `shutdown.Phases`
   type owning the ordering (the `thread/lifecycle.go` split is the in-repo
   precedent); `App` becomes a facade. (L)
   **Done 2026-08-21.** `app.go` 1075 -> 223 lines (-79%), split into
   `services.go` (340), `events.go` (177) and `shutdown.go` (410). The six
   phases are now documented on `shutdownPhases` with the *reason* each
   precedes the next, which is the part that makes the sequence movable at all:
   (0) latch the dispatcher's accept gate so a `Send` racing shutdown is
   refused; (1) watcher/hook stop, because hooks can themselves initiate
   MCP/DB/agent work; (2) cancel agent work and join the dispatcher, since a
   live turn can still write messages or touch MCP/DB; (3) flush messages while
   the DB is still open; (4) close MCP, which can still touch the DB; (5)
   critical cleanups then release the DB last; (6) parallel independent
   teardown. The five cleanup queues are order-dependent *relative to their
   phase* — that is why they are separate queues — but incidental *within* a
   queue.
   Swapping phases 4 and 5 (release the DB before closing MCP) fails three
   named tests including `TestApp_Shutdown_MCPInitBeforeCloseAndDB` — verified
   independently.
   **I had to fix two things the step got wrong.** It moved `AgentCoordinator`
   into the unexported `appServices`, which broke
   `internal/workspace/agent_model_test.go`'s `&app.App{AgentCoordinator: ...}`
   literal — flagged but left broken, so the tree did not build. More
   importantly the three groupings were embedded **by pointer**, which made a
   bare `&app.App{}` panic on any promoted field: the offered
   `SetAgentCoordinatorForTest` seam dereferenced a nil `appServices`. Switched
   all three to value embedding, so the zero `App` is usable again, and fixed
   the call site. That is a contract worth keeping — pointer embedding turns
   every promoted access on a partially built App into a nil panic instead of a
   compile error.
   Line count: `app.go` shrank 79%, but the new files total 927, so the package
   is +79 overall — recorded rather than hidden.
8. [~] **`ConfigStore`** (5 mutexes + 2 atomics over 6 files, locking rules
   enforced by a 50-line comment): carve out `configFile` (persistence),
   `changeTracker` (staleness + agent-file snapshots), `modelCache` — retires
   3 of the mutexes structurally. (L)
   **Partially done 2026-08-21, and the entry's count was wrong.** There are
   **7 mutexes + 2 atomics**, not 5+2 — `onExternalChangeMu` and
   `agentSnapshotMu` (both in `watch.go`) were missed by my review.
   The full table is now written down: `mu`/`file.mu` guards in-process
   serialization of config-file writes paired with the cross-process flock;
   `writeMu` guards production of a new in-memory `Config`; `reloadMu`
   serializes reload attempts; `configMu` guards the `*Config` pointer word
   only; `stalenessMu` guards `trackedConfigPaths`/`snapshots`;
   `onExternalChangeMu` the callback field; `agentSnapshotMu` the agent-file
   snapshot; `version` and `credentialVersion` are lock-free counters.
   **Ordering is `reloadMu -> writeMu -> {configMu, stalenessMu,
   agentSnapshotMu, file.mu}`, and no path violates it** — every call site of
   `setConfig`, `updateLocked`, `CaptureStalenessSnapshot` and
   `captureAgentFileSnapshot` was traced and holds `writeMu`. `Load()`'s early
   return looks like an exception but runs single-goroutine before the store is
   published.
   One undocumented coupling found and deliberately left:
   `CaptureStalenessSnapshot` reads `workspacePath`/`globalDataPath` under only
   `stalenessMu`, relying on every caller also holding `writeMu` — true today,
   stated nowhere.
   Split so far: `configFile` (`configfile.go`) owns `mu` plus the lock and
   atomic-write mechanics, parameterized by path rather than `Scope`, so it
   knows nothing about `Config`, providers or scopes. `store.go` -28/+86... net
   -58. Two concurrency tests added (`store_locking_test.go`); the mutation
   dropping `configMu` from `Config()` trips `-race` in two tests including the
   pre-existing `TestScopeB_InPlaceMutationRace` — verified independently.
   Still open: the remaining six locks. Each is entangled with fields spread
   across `reload.go`, `staleness.go`, `watch.go`, `modelcache.go` and
   `build.go`; splitting them needs its own pass with its own tests rather than
   being done on faith at the tail of this one.

9. [~] **`model.UI` and `model.Chat`** (`ui/model/ui.go` 1440 L / ~30 fields;
   `chat.go` 1577 L / 91 methods): move `func (m *UI)` methods that touch a
   single sub-state struct onto that struct (`thread_indicator.go` is the
   model); extract `chatScrollbar` and `chatSelection` widgets (their tests
   carry over unchanged); split `updateMouse` (334 lines) into per-zone
   `hitTest*` methods; split the pure-dispatch `updateSession`/
   `updateSettings`. Rewrite `ui/AGENTS.md` to describe the sub-state pattern
   instead of "keep everything on UI". (L)
   **`UI` done 2026-08-21; `Chat` deliberately not split.** `ui.go` 1440 -> 1143.
   Six anonymous **value**-embedded groups — `widgets`, `term`, `notifyState`,
   `breadcrumbState`, `integrationsState`, `mouseState` — each declared next to
   the file that owns its behavior, plus ~15 message types and the
   `settingsOps`/`sessionState`/`layoutState` definitions relocated to their
   consumers. Value embedding was used on purpose: pointer embedding is what made
   a bare `&App{}` panic in item 7, and this package's tests build `UI` literals
   directly.
   `com`, `embedded`, `focus`, `state`, `keyMap`, `isCanceling` stay flat — no
   group fit them better than the model itself, the same call made for
   `globalCtx`/`agentDispatcher` on `App`.
   No golden file changed. Dropping the `ui.status` wiring fails
   `TestCmdDrivingGolden/open_models` — verified independently (note the naive
   version of that mutation does not compile, so it must be written to keep the
   parameter used; a "0 failures" from a broken build is not a surviving
   mutation).
   **`Chat` left alone, and I accept the reasoning**: its fields already cluster
   by comment block, it is never built from a literal outside `NewChat`, and
   `chat.go` is the hot render path this package warns against regressing — with
   no golden coverage scoped to `Chat`'s internal fields the way there is for
   `UI` rendering. Splitting it would have been symmetry for its own sake.
10. [x] **`styles.quickStyle`** (single 1033-line function): mechanical split
    into `quickStyleDialog`/`quickStyleTool`/… mirroring the `Styles`
    sub-structs; zero behavior risk. Delete the two `Deprecated` style fields
    if unreferenced; route the last direct `charmtone` constants
    (`dialog/api_key_input.go:252`, `diffview/style.go`) through `Palette`. (S)
   **Done 2026-08-21.** `quickstyle.go` 1033 -> 149 lines, now just an
   assembler; 12 builders live in 10 new per-area files, none over 225 lines.
   **The silent failure mode this split invites is closed mechanically, which
   is the part that matters.** A field assigned in the wrong builder, or twice
   with the second winning, changes the theme and no compiler notices.
   `TestQuickStyleFieldsAssignedOnce` reflects over `Styles` (recursing into
   the anonymous inline structs, treating named external types as leaves) and
   parses every builder with `go/ast`, then asserts a one-to-one match:
   **357 leaf fields, 357 assignment sites, all singular**, and it flags any
   assignment aimed at an unknown path. Verified independently by deleting
   `s.Tool.IconPending` — the test names the missing field.
   One real defect surfaced: `Resource.DefaultTitleFg`/`DefaultDescFg` were
   assigned **twice**, under two separate `// ResourceGroup` comments. Same
   values, so no rendered output changed, but a genuine duplicate would have
   defeated the invariant — the duplicate was dropped.
   No golden file changed, confirmed by `git status`.
11. [x] **`message` model carries provider SDKs**: move `ToAIMessage` and the
    provider-specific reasoning accessors into an adapter in `internal/agent`;
    the persistence model shared with `db` becomes provider-free. Extract the
    debounce buffer (`pendingState`, 8 fields) into a channel-driven
    `messageWriter` so "flush before read" stops leaking to callers
    (`app.Shutdown` phase 3, session switch, tests). (M each)
   **Done 2026-08-21; premise verified real, not assumed.** Before,
   `go list -deps ./internal/message` pulled `charm.land/fantasy` and its
   anthropic/google/openai providers, `openai-go/v3`, `anthropic-sdk-go` with
   bedrock and vertex, `genai`, and the whole `aws-sdk-go-v2` tree. After, the
   same grep returns nothing — `message` is a leaf again, as AGENTS.md's proto
   boundary already claimed it was. `ToAIMessage` is now unexported
   `toAIMessage` in `internal/agent/message_convert.go`.
   **One field made this more than a move, and it was handled correctly.**
   `ReasoningContent.ResponsesData` was typed
   `*openai.ResponsesReasoningMetadata` **in the persisted data model** — a
   provider SDK type inside data written to users' session files. It became a
   local `message.ResponsesReasoningMetadata` with hand-written
   `MarshalJSON`/`UnmarshalJSON` reproducing the SDK's `{"type":...,"data":...}`
   envelope byte for byte, so sessions written by older binaries still decode.
   Changing the envelope's type tag fails
   `TestResponsesReasoningMetadataMarshalsWithLegacyEnvelope` — verified
   independently. Conversion to the real SDK type happens at the agent boundary.
   A pre-existing upstream quirk was found on the way and **preserved, not
   fixed**: the envelope round-trip silently zeroes `ResponsesData` through
   `MarshalParts`/`UnmarshalParts` (double wrapping, reproduced against the
   real SDK type before anything was touched). It is now pinned by
   `TestReasoningContentResponsesDataMarshalPartsQuirk` so it cannot change
   unnoticed.
   Cassette tests pass unchanged — the request body is identical.
12. [x] **`thread.Manager.Create`/`Activate`** manual rollback (7 hand-written
    failure paths; one miss = leaked worktree + spawned App): a rollback
    stack plus one shared `spawnAndInstall`. Consider the `Thread` →
    `Delegation` rename the types.go doc already implies (five `Kind`
    switches would fold into the existing lifecycle hooks). (M/L)
   **Done 2026-08-21.** One `unwinder` (`rollback.go`) replaces the 8 hand-written
   rollback sites and `abortSpawn`. Invariant, stated on the type: `push` only
   after the step it undoes has succeeded; `unwind` runs undos in reverse push
   order and never runs one for a step that never pushed; `commit()` disarms it.
   `Create`/`Activate` now `defer rb.unwind()` once instead of calling a rollback
   at every error return.
   **The same undo was written three different ways**, which is the finding worth
   keeping: "remove the worktree" existed once inline with `_ =` discarding the
   error and once via `abortSpawn` which logged it; "release the spawn handle"
   existed in `abortSpawn` (logged) and twice more in `Activate` (both silently
   discarding). Unifying closed a real gap — a failed cleanup during `Activate`
   could leave an orphaned worktree with **no trace in the logs**, which is
   exactly the condition `sennit gc`'s orphan report exists to surface after the
   fact.
   No failure path was missing a rollback. Two pre-existing bugs were found and
   deliberately **not** fixed here, both now in TECHDEBT.md: `failCreate` receives
   an `st` already overwritten by `SetSession`'s zero-value return
   (`manager.go:302`), so it marks an empty ID; and `Create`'s `ctx.Err()` /
   `setStatus` paths never call `failCreate` at all, unlike their siblings.
   Tests are the real deliverable: table-driven injection at each of Create's 7
   steps and Activate's 3, asserting exactly what unwound and what did not, plus a
   dedicated ordering test — the fake spawner has no real dependency on the
   worktree, so end-state checks cannot see order; `fakeSpawner.Release` now
   records whether the worktree still existed when it was called. Forward-order
   mutation fails precisely that test and nothing else — verified independently.
   Line count is up: 8 call sites plus `abortSpawn` collapsed, but `rollback.go`
   (45) and 339 lines of per-path tests were added. The tests were the point.

## Deliberately out of scope

- Copilot OAuth impersonation and the Gemini consecutive-user-contents
  question — already tracked with decisions/next steps in `TECHDEBT.md`.
- `internal/event` no-op telemetry shim — documented as intentional; only its
  three caller-less functions are addressed here (phase 1).
- Dependency changes: no `replace` directives, no forked deps; the 7
  pseudo-versions are upstream-canonical. Nothing to do.

## Review notes (clean areas, so the next reviewer can skip them)

- Context propagation outside `internal/ui` is disciplined: every detached
  `context.Background()` checked carries an in-place justification.
- The small utility packages (`stringext`, `filepathext`, `ansiext`, `csync`,
  `home`) are genuinely distinct, not redundant; the only real utility
  duplication is `atomicWriteFile` (phase 0).
- Platform build-tag files in `config` are clean and minimal.
- Package-level mutable state is consistently guarded; no unguarded globals
  found.
- Test infrastructure is strong overall: 408 test files, VCR cassettes for
  provider traffic, golden tests, `goleak`. The gaps are concentrated where
  phase 3 points.
