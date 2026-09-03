-- name: GetFileByPathAndSession :one
SELECT *
FROM files
WHERE path = ? AND session_id = ?
ORDER BY version DESC, created_at DESC
LIMIT 1;

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

-- name: DeleteSessionFiles :exec
DELETE FROM files
WHERE session_id = ?;

-- name: CountFilesForSessionIDs :one
-- gc's dependent-row count for a batch of sessions it is about to delete;
-- see CountMessagesForSessionIDs for why json_each replaces an IN-list.
WITH input AS (
    SELECT CAST(sqlc.arg(session_ids_json) AS TEXT) AS session_ids_json
)
SELECT COUNT(*) FROM files
WHERE files.session_id IN (
    SELECT value FROM input, json_each(CAST(input.session_ids_json AS TEXT))
);

