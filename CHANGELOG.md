# Changelog

## Unreleased

- A Codex turn refused for a spent subscription window now says which window
  is spent and when it comes back ("Codex plus: the 5h limit is spent (100%
  used), resets at 19:42 (in 1h 12m)") instead of "Rate limited", and stops
  there rather than spending three backoff attempts on a refusal that stands
  for hours. Both plan shapes are read as the backend reports them: a 5h
  window plus a weekly one, or a weekly one alone.
- A session stopped by such a limit picks its own work back up once the
  window resets — no prompt needed. Sending a prompt yourself, or cancelling,
  drops the parked resume.
- Rotation holds a rate-limited account down until its window actually
  resets, instead of the flat ten-minute cooldown, when the provider quoted
  no Retry-After, and no longer rotates into an account whose window is just
  as spent: every candidate is judged on the last figures the provider
  quoted, not on what was last written to accounts.json.
- Fixed: Codex usage snapshots were filed under the provider's account id
  and looked up under Sennit's own, so they were never found. Threshold
  rotation for Codex never fired because of it.
- Fixed: a 429 after a successful account rotation was never shown to the
  rotation hook again, so the rest of the retry pass ran blind against
  accounts it had already switched into.
- A session parked on a spent window is left alone until it comes back: no
  idle summarize, no delegation-completion wake. Both used to spend another
  request each, and get another 429.
- Fixed: the sidebar kept showing the previous account's name after an
  automatic account switch.

## 0.11.2

- Fixed: signing in from the Codex accounts dialog can reach an account other
  than the one the Codex CLI is signed in as. "Login account…" reused the
  CLI's login on disk whenever it could, so it refreshed the account already
  on file, made that one active and reported a successful sign-in — while the
  account you meant to add or re-authenticate was never touched.
  `sennit accounts add codex` was a no-op for the same reason.
- Fixed: `ctrl+t` on an account no longer writes another account's token into
  it. The refresh matched the Codex CLI's login on disk against the
  provider's *active* account instead of the one being refreshed, so
  refreshing a second account while the first was live copied the first
  account's credential into the second's entry.
- A sign-in's success screen names the account it signed in as, and says when
  the login was adopted from the Codex CLI instead of the browser.
  "Authentication successful!" on its own answered neither question.

## 0.11.1

- Security: the bash deny list (`sudo`, `curl`, `apt`, `go install`, …) is a
  floor again. It was matched against the sandboxed command rather than the
  one the model asked for, so inside a confined workspace — a worktree
  delegation running unattended, among others — nothing in the wrapper was
  ever recognised as deny-listed. Yolo, an auto-approved session and an
  `--allowed-tools` entry naming `bash` also answered that prompt without a
  person seeing it; they no longer do. A `PreToolUse` hook still decides per
  call, and headless `sennit run`, which has no one to ask, denies such a
  command at once instead of waiting forever for an answer.
- Fixed: a delegation that finished while the provider's OAuth token was
  dead left its parent retrying a continuation every second and a half, over
  1600 failed requests in one session. The attempt cap engages now, and
  OpenAI's `refresh_token_reused` family of codes opens the
  re-authentication prompt the way `invalid_grant` already did.
- Fixed: a stream cut off while the model was writing a tool call's
  arguments left a JSON fragment in the session's history, and llama.cpp
  rejected every later request for that session because of it. Such a call
  never ran, so its arguments are replayed as `{}`.
- Fixed: a long run counts its own steps toward the history it can reclaim.
  One that started just after a compact measured 3k tokens and never updated
  the figure, so it declined to summarize while filling a 262k window with
  its own tool output, until the provider cut the turn off mid tool call.
- Fixed: rate limiting. A 429 now ends the turn with words you can act on
  instead of an empty provider body.
- Fixed: `general-purpose` is no longer appended to the `subagent_type`
  options when the workspace defines an agent of that name. The duplicate
  value was rejected by strict-schema providers, and the alias shadowed your
  own agent.
- Fixed: an MCP tool whose server name contains an underscore is named the
  same way in the transcript and in the permission prompt.
- Fixed: opening a session, a delegation included, no longer hangs for
  seconds in a repository with a large untracked tree. Marking the session's
  uncommitted files listed the whole working tree and read every untracked
  file to count its lines; it now asks git about the session's own files
  alone.
- Fixed: opening a long session no longer reads every finished delegation's
  child transcript into memory. One session's 160 children came to 292MB of
  JSON, held for as long as the session stayed open, to render collapsed
  blocks that need only a step count. The full transcript is still one click
  away.
- Fixed: on Windows, cancelling a command kills the whole process tree.
  Grandchildren outlived the command, and the wait could hang on a pipe one
  of them held.
- Fixed: reading a single delegation no longer pulls its execution snapshot
  with it. The dashboard dock refreshes each running delegation every eight
  seconds, so three of them moved gigabytes a minute through memory; the
  snapshot is now loaded only where it is needed, when a task is resumed.
- `ctrl+t` in the accounts dialog refreshes the selected account's OAuth
  token, without switching to it. It replaces the `r` binding, which
  refreshed the provider's active credential instead; a bare letter could
  not stay, because the rows are email addresses and every unclaimed key
  goes to the filter input.

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

