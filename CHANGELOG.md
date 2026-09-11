# Changelog

## 0.11

- **Breaking:** Removed the `sennit threads` CLI command. Manage delegations from the TUI instead.
- **Breaking:** The builtin `threads` skill is now `isolation`, after its
  subject: an `agent` delegation asking for `isolation: worktree`. A
  `disabled_skills` entry naming `threads` no longer matches anything.
- **Breaking:** Threads and tasks are one thing now — a delegation, with an
  isolation flag. Delegated work runs behind a single unified tool surface,
  and automatic thread merging is replaced with an explicit, safe cleanup
  flow rather than a merge that could fail halfway.
- `worktree` and `exit worktree` commands move the session you are in into a
  git worktree and back. The conversation is the same one — the same session,
  the same transcript — only its working root changes, and delegations that
  finish mid-move are still delivered exactly once.
- Delegations are monitored in one place in the dashboard, whatever their
  isolation, and a delegated session now stays under its parent's project
  rather than being filed under the worktree it happened to run in.
- A turn may now run up to 10 background tasks at once (20 per workspace),
  up from 2 and 4. The old ceiling was reachable in ordinary use, which made
  it read as a throttle on normal fan-out rather than the backstop against a
  runaway cascade it is meant to be.
- An opt-in pprof server, enabled by setting `SENNIT_PPROF` to an address.
  A wedged render loop or a goroutine pile-up leaves nothing in the log, and
  the alternatives were a SIGQUIT that kills the session being diagnosed or
  a debugger that needs root on a stock Linux.
- Fixed: a finished delegation's report reaches the session that started it
  again. Sessions that had never been moved into a worktree have no ownership
  record, and delivery read that absence as "nobody owns this session" and
  dropped the report, so the parent waited on an answer that never came.
- Fixed: the UI no longer piles up parked deliveries when its message queue
  fills. One wedged session was found holding 922 goroutines blocked in
  Send, most of them carrying updates newer arrivals had already replaced.
- Fixed: listing delegations no longer reads back each one's execution
  snapshot. The snapshot embeds the full prior history of a delegated
  session and runs to tens of megabytes per row, so listings — which happen
  on every dispatch and every cancel — were dragging hundreds of megabytes
  through memory and burning CPU on the garbage that made.
