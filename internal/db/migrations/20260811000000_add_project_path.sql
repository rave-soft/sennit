-- +goose Up
-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN project_path TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_sessions_project_path ON sessions (project_path);

ALTER TABLE threads RENAME TO threads_old;

CREATE TABLE threads (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    project_path TEXT NOT NULL DEFAULT '',
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
    completed_at INTEGER,         -- Unix timestamp in seconds
    UNIQUE(project_path, name)
);

INSERT INTO threads (
    id, name, project_path, goal, base_branch, branch, worktree_path,
    session_id, status, merge_policy, result_summary, error,
    created_at, updated_at, completed_at
)
SELECT
    id, name, '', goal, base_branch, branch, worktree_path,
    session_id, status, merge_policy, result_summary, error,
    created_at, updated_at, completed_at
FROM threads_old;

DROP TABLE threads_old;

CREATE INDEX IF NOT EXISTS idx_threads_status ON threads (status);
CREATE INDEX IF NOT EXISTS idx_threads_project_path ON threads (project_path);

CREATE TRIGGER IF NOT EXISTS update_threads_updated_at
AFTER UPDATE ON threads
BEGIN
UPDATE threads SET updated_at = strftime('%s', 'now')
WHERE id = new.id;
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS update_threads_updated_at;
DROP INDEX IF EXISTS idx_threads_project_path;
DROP INDEX IF EXISTS idx_threads_status;

ALTER TABLE threads RENAME TO threads_old;

CREATE TABLE threads (
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

INSERT INTO threads (
    id, name, goal, base_branch, branch, worktree_path,
    session_id, status, merge_policy, result_summary, error,
    created_at, updated_at, completed_at
)
SELECT
    id, name, goal, base_branch, branch, worktree_path,
    session_id, status, merge_policy, result_summary, error,
    created_at, updated_at, completed_at
FROM threads_old;

DROP TABLE threads_old;

CREATE INDEX IF NOT EXISTS idx_threads_status ON threads (status);

CREATE TRIGGER IF NOT EXISTS update_threads_updated_at
AFTER UPDATE ON threads
BEGIN
UPDATE threads SET updated_at = strftime('%s', 'now')
WHERE id = new.id;
END;

DROP INDEX IF EXISTS idx_sessions_project_path;
ALTER TABLE sessions DROP COLUMN project_path;
-- +goose StatementEnd
