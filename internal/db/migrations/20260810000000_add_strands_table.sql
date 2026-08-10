-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS strands (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    goal TEXT NOT NULL,
    base_branch TEXT NOT NULL,
    branch TEXT NOT NULL,
    worktree_path TEXT NOT NULL,
    session_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    merge_policy TEXT NOT NULL DEFAULT 'auto',
    result_summary TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,  -- Unix timestamp in seconds
    updated_at INTEGER NOT NULL,  -- Unix timestamp in seconds
    completed_at INTEGER          -- Unix timestamp in seconds
);

CREATE INDEX IF NOT EXISTS idx_strands_status ON strands (status);

CREATE TRIGGER IF NOT EXISTS update_strands_updated_at
AFTER UPDATE ON strands
BEGIN
UPDATE strands SET updated_at = strftime('%s', 'now')
WHERE id = new.id;
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS update_strands_updated_at;
DROP INDEX IF EXISTS idx_strands_status;
DROP TABLE IF EXISTS strands;
-- +goose StatementEnd
