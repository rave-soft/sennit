-- name: CreateThread :one
INSERT INTO threads (
    id,
    name,
    project_path,
    goal,
    base_branch,
    branch,
    worktree_path,
    session_id,
    status,
    merge_policy,
    updated_at,
    created_at
) VALUES (
    ?,
    ?,
    ?,
    ?,
    ?,
    ?,
    ?,
    ?,
    ?,
    ?,
    strftime('%s', 'now'),
    strftime('%s', 'now')
) RETURNING *;

-- name: GetThread :one
SELECT *
FROM threads
WHERE id = ? LIMIT 1;

-- name: GetThreadByName :one
SELECT *
FROM threads
WHERE name = ? AND project_path = ? LIMIT 1;

-- name: ListThreads :many
SELECT *
FROM threads
WHERE project_path = ?
ORDER BY created_at;

-- name: UpdateThreadStatus :one
UPDATE threads
SET
    status = ?,
    error = ?,
    result_summary = ?,
    completed_at = ?
WHERE id = ?
RETURNING *;

-- name: UpdateThreadSession :one
UPDATE threads
SET
    session_id = ?
WHERE id = ?
RETURNING *;

-- name: DeleteThread :exec
DELETE FROM threads
WHERE id = ?;

-- name: ListThreadsForGC :many
-- Every thread across every project, trimmed to the columns `braid gc`
-- needs to pick finished threads older than the retention cutoff.
-- Unscoped by project_path; the caller filters by project in Go for
-- --project.
SELECT id, project_path, status, updated_at
FROM threads;
