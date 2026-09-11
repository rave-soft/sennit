-- +goose Up
-- +goose StatementBegin
-- A session has exactly one active runtime owner.  The target fields are
-- retained while preparation is in flight so restart recovery can safely
-- choose the source owner until a commit has occurred.
CREATE TABLE session_worktree_ownership (
    session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    owner_id TEXT NOT NULL,
    owner_root TEXT NOT NULL,
    worktree_name TEXT NOT NULL DEFAULT '',
    worktree_path TEXT NOT NULL DEFAULT '',
    phase TEXT NOT NULL DEFAULT 'stable' CHECK (phase IN ('stable', 'preparing')),
    target_owner_id TEXT NOT NULL DEFAULT '',
    target_root TEXT NOT NULL DEFAULT '',
    target_worktree_name TEXT NOT NULL DEFAULT '',
    target_worktree_path TEXT NOT NULL DEFAULT '',
    epoch INTEGER NOT NULL DEFAULT 1 CHECK (epoch > 0),
    updated_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now'))
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS session_worktree_ownership;
-- +goose StatementEnd
