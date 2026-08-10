-- name: CreateThread :one
INSERT INTO threads (
    id,
    name,
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
WHERE name = ? LIMIT 1;

-- name: ListThreads :many
SELECT *
FROM threads
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
