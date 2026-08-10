-- name: CreateStrand :one
INSERT INTO strands (
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

-- name: GetStrand :one
SELECT *
FROM strands
WHERE id = ? LIMIT 1;

-- name: GetStrandByName :one
SELECT *
FROM strands
WHERE name = ? LIMIT 1;

-- name: ListStrands :many
SELECT *
FROM strands
ORDER BY created_at;

-- name: UpdateStrandStatus :one
UPDATE strands
SET
    status = ?,
    error = ?,
    result_summary = ?,
    completed_at = ?
WHERE id = ?
RETURNING *;

-- name: UpdateStrandSession :one
UPDATE strands
SET
    session_id = ?
WHERE id = ?
RETURNING *;

-- name: DeleteStrand :exec
DELETE FROM strands
WHERE id = ?;
