-- +goose Up
-- +goose StatementBegin
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

UPDATE threads
SET terminal_at = MAX(1, updated_at * 1000000000)
WHERE kind = 'task' AND completion_pending = 1 AND terminal_at IS NULL;

INSERT INTO task_completion_outbox (
    task_id, terminal_at, status, error, result_summary, completion_depth, completed_at,
    name, goal, session_id, parent_session_id
)
SELECT id, terminal_at, status, error,
       result_summary, completion_depth, completed_at,
       name, goal, session_id, parent_session_id
FROM threads
WHERE kind = 'task' AND completion_pending = 1;

CREATE INDEX idx_task_completion_outbox_pending
ON task_completion_outbox (terminal_at, task_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX idx_task_completion_outbox_pending;
DROP TABLE task_completion_outbox;
-- +goose StatementEnd
