# Changelog

## 0.10

- **Breaking:** Removed the `sennit threads` CLI command. Manage delegations from the TUI instead.
- **Breaking:** The builtin `threads` skill is now `isolation`, after its
  subject: an `agent` delegation asking for `isolation: worktree`. A
  `disabled_skills` entry naming `threads` no longer matches anything.
- `worktree` and `exit worktree` commands move the session you are in into a
  git worktree and back. The conversation is the same one — the same session,
  the same transcript — only its working root changes, and delegations that
  finish mid-move are still delivered exactly once.
- Fixed: a finished delegation's report reaches the session that started it
  again. Sessions that had never been moved into a worktree have no ownership
  record, and delivery read that absence as "nobody owns this session" and
  dropped the report, so the parent waited on an answer that never came.
