# Delegation stops blocking

A delegation currently holds three separate locks, and "the subagent blocks
the thread" is any one of them. This record separates them, states which are
meant to go, and stages the work so each step is shippable on its own.

The decision is that a subagent blocks **nothing**: neither the caller's turn
nor the session it was launched from. The result reaches the caller the way a
background delegation's result already does — through the task manager and the
parent's completion inbox — rather than through a blocked tool call.

## What blocks today

**1. The caller's turn.** A foreground `agent` call
(`internal/agent/agent_tool.go:93`) waits for the delegation's answer, so the
step does not finish until the subagent does. This is the model waiting.

**2. The session.** The parent session stays busy because its own coder turn is
still inside the foreground tool call. `subSessions`
(`internal/agent/coordinator.go:148`) is not a parent-side lock: it is keyed by
the **child** session id and exists only so a person navigating into a running
child sees the right busy state. A person's message to the busy parent steers
its current turn, and steering a *detachable* delegation makes that tool call
return while the child continues in the background (`runDetachableSubAgent`,
`internal/agent/subagents.go:243`). So the person is not strictly locked out,
but talking changes the execution mode of the delegation.

**3. The thread's queue.** `thread_send` asks the adapter for the target's
busy/queue state (`SessionQueue`,
`internal/app/threadspawn/coordinator_adapter.go:116`, forwarding
`IsSessionBusy`) and reports the message as queued when the thread is busy. The
send call itself returns promptly; what is delayed is the target thread reading
and acting on that message. A thread whose coder turn is stuck in a foreground
delegation can therefore leave a follow-up unread for the full duration of the
delegation.

## What already exists to build on

- **`agent` with `background: true`** (`runBackgroundAgent`,
  `internal/agent/agent_tool.go:117`) returns a `TaskID` ack immediately and
  runs the delegation through the task manager. Its result wakes the parent
  through the completion inbox (`internal/agent/completion_inbox.go`). This is
  the target semantics; it is already implemented and shipped.
- **Detach-on-steering** (`subAgentParams.Detachable`,
  `internal/agent/subagents.go:36`) already converts a live foreground
  delegation into a background one without losing it.
- **Threads already deliver completion on their own**
  (`internal/thread/lifecycle.go:960` into the parent's inbox), which is why
  `thread_wait` is redundant rather than load-bearing.
- **`bash`** is the strongest non-blocking pattern in the codebase:
  `run_in_background`, plus an automatic hand-off to a background job after
  `DefaultAutoBackgroundAfter` seconds (`internal/agent/tools/bash.go:54`),
  with `job_output`/`job_kill` to follow up. It needs no cooperation from the
  model — a call that turns out to be slow becomes a job by itself.

## Target

Every subagent tool — built-in `agent`, `agentic_fetch`, and each custom agent
tool — returns an ack immediately. The parent's current turn may continue doing
other work and remains legitimately busy while it does; the child delegation
no longer keeps that turn busy by itself. Once the parent turn rests, a new
person message or `thread_send` follow-up can start normally rather than
waiting for the child. The delegation's result arrives later as a completion,
and the parent picks it up on the next step the inbox wakes.

For `agent`, `background: true` then no longer selects a different execution
model — asynchronous execution becomes the only model, and the parameter goes
away with the distinction it named. The other subagent tools keep their current
schemas but adopt the same launch and completion semantics.

## Staged plan

Each stage leaves the tree green and shippable. Stages 1 and 2 are independent
cleanup. Stage 3 can land independently of them; stage 4 depends on stage 3 and
makes the earlier detach machinery obsolete.

### Stage 1 — close the inconsistencies (small behavior fixes)

These are wrong today regardless of what the rest of this plan does, and
fixing them first shrinks the diff of every later stage.

- **`agentic_fetch` cannot detach.** It calls `runSubAgent` without
  `Detachable` (`internal/agent/agentic_fetch_tool.go:207`), while `agent`
  and every custom agent tool (`internal/agent/custom_agent_tool.go:138`) set
  it. Steering therefore cancels a fetch analysis where it would have detached
  a delegation. Set the field.

  Note what this does *not* buy. `canDetachSubAgent`
  (`internal/agent/subagents.go:213`) requires `backgroundAgentsEnabled()`, a
  depth below `maxTaskCascadeDepth`, *and* a non-nil `tasksManager()`. Outside
  a repository root the last of those is nil (see stage 3), so
  detach-on-steering is already dead there for every subagent tool. The fix
  makes `agentic_fetch` behave like the others wherever detaching works at
  all; it does not extend where that is.
- **`thread_wait`'s doc comment contradicts its metadata.**
  `NewThreadWaitTool` says the tool is "deliberately absent from the default
  AllowedTools set ... now that a thread's completion is delivered on its own
  through the completion inbox", but `internal/toolmeta/toolmeta.go:119`
  declares `DefaultAllowed: true` and the default sets in
  `internal/config/setup_agents_test.go` contain it. Whichever way stage 2
  goes, one of the two is a lie today.

### Stage 2 — remove `thread_wait`

Threads are the only delegation kind with a blocking wait. Tasks have none —
`task_list`, `task_result`, `task_output`, `task_cancel`, `task_send` and the
inbox — and threads already deliver completion the same way. Removing it is
what makes the two uniform.

Delete `internal/agent/tools/thread_wait.go` and `thread_wait.md.tpl`, the
`toolmeta.go:119` entry, the `internal/proto/ui_tools.go:38` constant and the
registration in the tool registry. Tests to update: `setup_agents_test`,
`toolmeta_test`, `tool_registry_test`, `coordinator_threads_test`,
`schema_contract_test`, `parallel_tools_test`, `permission_coverage_test`,
`thread_tools_test`. Docs under `docs/` mention it too.

The one capability lost is "block until several named threads settle
together". If that turns out to be wanted, it belongs as a *non-blocking*
predicate over `thread_status`, not as a tool that parks a turn for ten
minutes.

### Stage 3 — make task dispatch available in every workspace

The existing background path is not yet a universal replacement for foreground
delegation. `TaskManager` is installed by `threadspawn.Attach`, and Attach
returns without wiring it outside a repository root
(`internal/app/threadspawn/attach.go:97`). Simply routing every `agent` call to
`runBackgroundAgent` would therefore remove delegation from non-Git
workspaces.

Decouple `TaskManager` and its lifecycle from construction through a
Git-backed `thread.Manager`, then install the lightweight task overlay in every
workspace. The delegation store can still use the global database keyed by
workspace path; only the thread overlay, worktrees, branches, and merging stay
conditional on a repository. Keep the current persistence, recovery,
concurrency caps, cancellation, `task_send`, output, and completion behavior.
Add coverage for dispatch, restart recovery, and completion in a non-Git
temporary directory before removing the synchronous fallback.

### Stage 4 — generalize asynchronous launch to every subagent tool

The current task path always dispatches a normal coder turn through
`Coordinator.RunAccepted`; it cannot preserve the specialized runtime of a
custom agent or `agentic_fetch`. The replacement therefore cannot be a blind
call to `runBackgroundAgent`.

1. Introduce one asynchronous delegation launcher that creates the task record
   and child session, then owns the concrete run from preparation through
   terminal bookkeeping. Its launch description must carry the child-session
   title and agent id as well as the prompt, so custom-agent history remains
   scoped exactly as it is through `CreateSubAgentSession`. The launcher accepts
   a run factory rather than an already-prepared delegate: everything that may
   block belongs behind the ack. The actual runner remains in-process; task
   recovery after restart keeps its current interrupted semantics.
2. Route the built-in `agent` and every custom agent tool
   (`internal/agent/custom_agent_tool.go:108`) through that launcher. Move the
   custom-agent call-time definition check and any rebuild it triggers into the
   run factory, so a config reload still selects the latest model and prompt
   without delaying the ack.
3. Route `agentic_fetch` (`internal/agent/agentic_fetch_tool.go:76`) through the
   same launcher, but move its entire operation behind the ack: permission
   request, temporary-directory creation, optional URL download, prompt/model
   construction, and delegate execution. Leaving the current preparation in
   the tool handler would still block on a person, network, filesystem, and
   runtime build before returning, which would violate the target even if only
   `runSubAgent` became asynchronous.
4. Make resource ownership explicit in the launch contract. The run factory
   returns `(run, cleanup, err)`, and `cleanup` may be non-nil even when `err`
   is non-nil, so resources acquired before a later preparation failure still
   have an owner. The launcher calls cleanup exactly once after terminal
   completion, failure, cancellation, or partial preparation failure. In
   particular, move `agentic_fetch`'s `defer os.RemoveAll(tmpDir)`
   (`internal/agent/agentic_fetch_tool.go:119`) into this lifecycle: a defer in
   the now-short tool handler would delete the directory immediately after the
   ack, before the delegate could read it. Test success, permission denial,
   download/build failure, explicit cancellation, and workspace shutdown for
   both no leak and no early deletion.
5. Preserve terminal bookkeeping as one idempotent path. On every terminal
   outcome, release the child busy marker, persist the task status/result, and
   deliver one completion. A permission denial is a terminal, model-visible
   refusal rather than an infrastructure error; it must settle the task and
   explain the denial through the same completion path.

   Accumulate the child session's cost into the parent exactly once whenever
   the child has recorded cost, including failed or cancelled runs if providers
   accounted usage before returning an error. `updateParentSessionCost`
   (`internal/agent/subagents.go:444`) provides the atomic increment but is not
   itself idempotent. Add durable finalization state (or perform task terminal
   transition, cost attribution, and an outbox completion in one database
   transaction) so duplicate run-complete/cancel races and a process crash
   between those writes cannot charge twice or lose the completion. Add
   sibling-concurrency, cancel-vs-complete, and crash/recovery tests around that
   invariant.
6. Keep the child marked busy for navigation while its delegate runs.
   `subSessions` already has exactly that child-only meaning and does not need
   a parent-side removal. The parent stops being busy on the delegation's
   account naturally because its tool call returns immediately; it may still
   be busy doing later steps in its own turn, in which case normal steering and
   queueing rules still apply.
7. Drop `background` from `AgentParams` and the built-in tool description.
   `options.background_agents` now gates all new subagent dispatch, including
   custom agents and `agentic_fetch`.

   This is not only a semantics change: the option's own published schema
   description (`internal/config/config.go:300`) states that it allows "the
   agent tool's background mode and the task_* tools" and closes with "Does
   not affect threads." Broadening it to gate every subagent tool contradicts
   text that already shipped in `schema.json`, so the choice is between
   rewriting that description or renaming the option outright. Either way it
   belongs in the release notes rather than in a silent redefinition.
8. With no foreground subagent path left, remove `Detachable`,
   `runDetachableSubAgent`, and `AgentDetachedResponseMetadata`. Fold the
   chat's detached-delegation rendering (`internal/ui/chat/agent.go`) into the
   background-dispatch rendering that already exists
   (`renderBackgroundDispatch`).

### Open questions for stage 4

- **Cascade depth.** `maxTaskCascadeDepth` (currently 3,
  `internal/agent/continuation.go:27`) is documented as a deliberately hard,
  non-configurable bound on auto-woken continuation chains — the failure mode
  it guards is an unattended cascade quietly burning tokens, which is why no
  config value may reopen it. It is already broader than the background
  branch: `canDetachSubAgent` consults it too. The question is therefore not
  whether the value was picked for a rare path, but that the *population* it
  bounds changes: once every delegation is asynchronous, ordinary delegation
  nesting inherits a limit written for continuation chains. Re-pick it with
  that in mind, and keep it a constant.
- **Does the model cope?** Tools that answer "started, result to follow"
  instead of returning the work are a real prompt-level change. The task
  agent's description, custom-agent descriptions, `agentic_fetch` description,
  and the coder prompt currently describe delegation as something you get an
  answer from. This needs prompt work and an eval, not just the plumbing.
- **Ordering.** A parent that fires three delegations and continues now sees
  three completions arrive in whatever order they finish. The inbox already
  handles multiple entries; the prompt has to make the correlation explicit.

## Other blocking tools, not in this plan

Found while surveying; recorded so the survey is not repeated. Ranked by
pain over cost.

| tool | holds the turn for | escape hatch |
| --- | --- | --- |
| MCP tools | unbounded — an external server | none; no per-call timeout anywhere in `internal/agent/tools/mcp` |
| `question` | unbounded — waits for a person | none; `ErrQuestionPending` also serializes them one at a time |
| permission requests | unbounded — waits for a person | none (`internal/permission/permission.go:419`); *any* mutating tool can park here |
| `download` | up to 600s | a timeout parameter, no background mode — though it is a `bash` job in all but name |
| `fetch`, `web_fetch` | up to 120s | timeout parameter |
| LSP (`lsp_restart`, `lsp_workspace_symbols`) | until the server is ready | none |

The first cheap win here is a per-call MCP timeout: it is the only unbounded
wait with no human on the other end, so nothing legitimately wants it to be
infinite.
