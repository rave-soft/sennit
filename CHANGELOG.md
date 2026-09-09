# Changelog

## 0.10

- **Breaking:** Removed the `sennit threads` CLI command. Manage delegations from the TUI instead.
- `worktree` and `exit worktree` commands move the session you are in into a
  git worktree and back. The conversation is the same one — the same session,
  the same transcript — only its working root changes, and delegations that
  finish mid-move are still delivered exactly once.
