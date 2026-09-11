-- +goose Up
-- +goose StatementBegin
UPDATE threads SET status = 'completed'
WHERE status IN ('merging', 'merged', 'conflict', 'merge_blocked');

DROP TRIGGER IF EXISTS update_threads_updated_at;
DROP TRIGGER IF EXISTS clear_thread_session_refs_on_session_delete;
DROP INDEX IF EXISTS idx_threads_status;
DROP INDEX IF EXISTS idx_threads_project_path;
DROP INDEX IF EXISTS idx_task_completion_outbox_pending;

ALTER TABLE task_completion_outbox RENAME TO task_completion_outbox_old;
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
    result_summary TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    completed_at INTEGER,
    kind TEXT NOT NULL DEFAULT 'thread',
    parent_session_id TEXT NOT NULL DEFAULT '',
    completion_pending INTEGER NOT NULL DEFAULT 0,
    completion_depth INTEGER NOT NULL DEFAULT 0,
    terminal_at INTEGER,
    cost_attributed INTEGER NOT NULL DEFAULT 0,
    execution TEXT NOT NULL DEFAULT '',
    UNIQUE(project_path, kind, name)
);

INSERT INTO threads (
    id, name, project_path, goal, base_branch, branch, worktree_path,
    session_id, status, result_summary, error, created_at, updated_at,
    completed_at, kind, parent_session_id, completion_pending,
    completion_depth, terminal_at, cost_attributed, execution
)
SELECT id, name, project_path, goal, base_branch, branch, worktree_path,
       session_id, status, result_summary, error, created_at, updated_at,
       completed_at, kind, parent_session_id, completion_pending,
       completion_depth, terminal_at, cost_attributed, execution
FROM threads_old;

CREATE TABLE task_completion_outbox (
    task_id TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    terminal_at INTEGER NOT NULL,
    status TEXT NOT NULL,
    name TEXT NOT NULL,
    goal TEXT NOT NULL,
    session_id TEXT NOT NULL,
    parent_session_id TEXT NOT NULL,
    error TEXT NOT NULL DEFAULT '',
    result_summary TEXT NOT NULL DEFAULT '',
    completion_depth INTEGER NOT NULL DEFAULT 0,
    completed_at INTEGER,
    PRIMARY KEY (task_id, terminal_at)
);

INSERT INTO task_completion_outbox (
    task_id, terminal_at, status, name, goal, session_id, parent_session_id,
    error, result_summary, completion_depth, completed_at
)
SELECT task_id, terminal_at, status, name, goal, session_id, parent_session_id,
       error, result_summary, completion_depth, completed_at
FROM task_completion_outbox_old;

DROP TABLE task_completion_outbox_old;
DROP TABLE threads_old;

CREATE INDEX idx_threads_status ON threads (status);
CREATE INDEX idx_threads_project_path ON threads (project_path);
CREATE INDEX idx_task_completion_outbox_pending
ON task_completion_outbox (terminal_at, task_id);

CREATE TRIGGER update_threads_updated_at
AFTER UPDATE ON threads
WHEN NEW.updated_at = OLD.updated_at
BEGIN
UPDATE threads SET updated_at = strftime('%s', 'now')
WHERE id = NEW.id;
END;

CREATE TRIGGER clear_thread_session_refs_on_session_delete
AFTER DELETE ON sessions
BEGIN
UPDATE threads SET session_id = ''
WHERE session_id = old.id;
UPDATE threads SET parent_session_id = ''
WHERE parent_session_id = old.id;
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS update_threads_updated_at;
DROP TRIGGER IF EXISTS clear_thread_session_refs_on_session_delete;
DROP INDEX IF EXISTS idx_threads_status;
DROP INDEX IF EXISTS idx_threads_project_path;
DROP INDEX IF EXISTS idx_task_completion_outbox_pending;

ALTER TABLE task_completion_outbox RENAME TO task_completion_outbox_old;
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
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    completed_at INTEGER,
    kind TEXT NOT NULL DEFAULT 'thread',
    parent_session_id TEXT NOT NULL DEFAULT '',
    completion_pending INTEGER NOT NULL DEFAULT 0,
    completion_depth INTEGER NOT NULL DEFAULT 0,
    terminal_at INTEGER,
    cost_attributed INTEGER NOT NULL DEFAULT 0,
    execution TEXT NOT NULL DEFAULT '',
    UNIQUE(project_path, kind, name)
);

INSERT INTO threads (
    id, name, project_path, goal, base_branch, branch, worktree_path,
    session_id, status, result_summary, error, created_at, updated_at,
    completed_at, kind, parent_session_id, completion_pending,
    completion_depth, terminal_at, cost_attributed, execution
)
SELECT id, name, project_path, goal, base_branch, branch, worktree_path,
       session_id, status, result_summary, error, created_at, updated_at,
       completed_at, kind, parent_session_id, completion_pending,
       completion_depth, terminal_at, cost_attributed, execution
FROM threads_old;

CREATE TABLE task_completion_outbox (
    task_id TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    terminal_at INTEGER NOT NULL,
    status TEXT NOT NULL,
    name TEXT NOT NULL,
    goal TEXT NOT NULL,
    session_id TEXT NOT NULL,
    parent_session_id TEXT NOT NULL,
    error TEXT NOT NULL DEFAULT '',
    result_summary TEXT NOT NULL DEFAULT '',
    completion_depth INTEGER NOT NULL DEFAULT 0,
    completed_at INTEGER,
    PRIMARY KEY (task_id, terminal_at)
);

INSERT INTO task_completion_outbox (
    task_id, terminal_at, status, name, goal, session_id, parent_session_id,
    error, result_summary, completion_depth, completed_at
)
SELECT task_id, terminal_at, status, name, goal, session_id, parent_session_id,
       error, result_summary, completion_depth, completed_at
FROM task_completion_outbox_old;

DROP TABLE task_completion_outbox_old;
DROP TABLE threads_old;

CREATE INDEX idx_threads_status ON threads (status);
CREATE INDEX idx_threads_project_path ON threads (project_path);
CREATE INDEX idx_task_completion_outbox_pending
ON task_completion_outbox (terminal_at, task_id);

CREATE TRIGGER update_threads_updated_at
AFTER UPDATE ON threads
WHEN NEW.updated_at = OLD.updated_at
BEGIN
UPDATE threads SET updated_at = strftime('%s', 'now')
WHERE id = NEW.id;
END;

CREATE TRIGGER clear_thread_session_refs_on_session_delete
AFTER DELETE ON sessions
BEGIN
UPDATE threads SET session_id = ''
WHERE session_id = old.id;
UPDATE threads SET parent_session_id = ''
WHERE parent_session_id = old.id;
END;
-- +goose StatementEnd
