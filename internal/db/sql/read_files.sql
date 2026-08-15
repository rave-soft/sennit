-- name: RecordFileRead :exec
INSERT INTO read_files (
    session_id,
    path,
    read_at,
    read_ranges
) VALUES (
    ?,
    ?,
    strftime('%s', 'now'),
    ?
) ON CONFLICT(path, session_id) DO UPDATE SET
    read_at = excluded.read_at,
    read_ranges = excluded.read_ranges;

-- name: GetFileRead :one
SELECT * FROM read_files
WHERE session_id = ? AND path = ? LIMIT 1;

-- name: ListSessionReadFiles :many
SELECT * FROM read_files
WHERE session_id = ?
ORDER BY read_at DESC;

-- name: CountSessionReadFiles :one
SELECT COUNT(*) FROM read_files
WHERE session_id = ?;

-- name: DeleteSessionReadFiles :exec
DELETE FROM read_files
WHERE session_id = ?;
