-- name: GetFile :one
SELECT *
FROM files
WHERE id = ? LIMIT 1;

-- name: GetFileByPathAndSession :one
SELECT *
FROM files
WHERE path = ? AND session_id = ?
ORDER BY version DESC, created_at DESC
LIMIT 1;

-- name: ListFilesBySession :many
SELECT *
FROM files
WHERE session_id = ?
ORDER BY version ASC, created_at ASC;

-- name: ListFilesBySessionTree :many
WITH RECURSIVE
ancestors(id, parent_session_id) AS (
    SELECT sessions.id, sessions.parent_session_id
    FROM sessions
    WHERE sessions.id = sqlc.arg(session_id)
    UNION ALL
    SELECT s.id, s.parent_session_id
    FROM sessions s
    JOIN ancestors a ON s.id = a.parent_session_id
),
root(id) AS (
    SELECT ancestors.id
    FROM ancestors
    WHERE ancestors.parent_session_id IS NULL
    LIMIT 1
),
session_tree(id) AS (
    SELECT root.id FROM root
    UNION ALL
    SELECT s.id
    FROM sessions s
    JOIN session_tree tree ON s.parent_session_id = tree.id
)
SELECT files.*
FROM files
JOIN session_tree ON files.session_id = session_tree.id
ORDER BY files.version ASC, files.created_at ASC;

-- name: NextFileVersion :one
-- Version numbers are allocated per path across every session, which is
-- what makes ListFilesBySessionTree's cross-session ordering and the
-- UI's first-to-latest diff meaningful. UNIQUE(path, version) is the key
-- that holds this up, so callers must allocate inside the same
-- transaction as the insert.
SELECT COALESCE(MAX(version), -1) + 1 AS next_version
FROM files
WHERE path = ?;

-- name: CreateFile :one
INSERT INTO files (
    id,
    session_id,
    path,
    content,
    version,
    created_at,
    updated_at
) VALUES (
    ?, ?, ?, ?, ?, strftime('%s', 'now'), strftime('%s', 'now')
)
RETURNING *;

-- name: DeleteFile :exec
DELETE FROM files
WHERE id = ?;

-- name: DeleteSessionFiles :exec
DELETE FROM files
WHERE session_id = ?;

-- name: CountSessionFiles :one
SELECT COUNT(*) FROM files
WHERE session_id = ?;

-- name: ListLatestSessionFiles :many
-- The latest version of each path *within this session*. The maximum has
-- to be taken over the session's own rows: versions are numbered per
-- path across all sessions, so a global MAX(version) matches a sibling
-- session's newer row and this session's files drop out of the result
-- entirely.
SELECT f.*
FROM files f
WHERE f.session_id = sqlc.arg(session_id)
  AND f.version = (
      SELECT MAX(f2.version)
      FROM files f2
      WHERE f2.path = f.path
        AND f2.session_id = f.session_id
  )
ORDER BY f.path;
