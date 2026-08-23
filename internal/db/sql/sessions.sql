-- name: CreateSession :one
INSERT INTO sessions (
    id,
    parent_session_id,
    title,
    message_count,
    prompt_tokens,
    completion_tokens,
    cost,
    summary_message_id,
    project_path,
    agent_id,
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
    null,
    ?,
    ?,
    strftime('%s', 'now'),
    strftime('%s', 'now')
) RETURNING *;

-- name: ListSubAgentSessions :many
-- The sessions a named sub-agent has already had under one parent, oldest
-- first: every prior turn of the same continuing conversation. agent_id is
-- empty for sessions that are not a named delegation, and the caller must
-- never pass '' here - that would sweep up every unrelated child session.
SELECT *
FROM sessions
WHERE parent_session_id = ?
  AND agent_id = ?
  AND agent_id != ''
  AND id != ?
ORDER BY created_at ASC, id ASC;

-- name: GetSessionByID :one
SELECT *
FROM sessions
WHERE id = ? LIMIT 1;

-- name: GetLastSession :one
-- The most recently updated top-level session in a project: same scope as
-- ListSessions (parent_session_id IS NULL), which also excludes every
-- agent-tool sub-session, since those always carry a parent_session_id.
SELECT *
FROM sessions
WHERE project_path = ? AND parent_session_id IS NULL
ORDER BY updated_at DESC
LIMIT 1;

-- name: ListSessions :many
SELECT *
FROM sessions
WHERE parent_session_id is NULL AND project_path = ?
ORDER BY updated_at DESC;

-- name: UpdateSession :one
UPDATE sessions
SET
    title = ?,
    prompt_tokens = ?,
    completion_tokens = ?,
    summary_message_id = ?,
    cost = ?,
    todos = ?
WHERE id = ?
RETURNING *;

-- name: UpdateSessionTitleAndUsage :exec
UPDATE sessions
SET
    title = ?,
    prompt_tokens = prompt_tokens + ?,
    completion_tokens = completion_tokens + ?,
    cost = cost + ?,
    updated_at = strftime('%s', 'now')
WHERE id = ?;


-- name: AddSessionCost :execrows
-- Accumulate a delegation's cost onto its parent. Narrow on purpose: the
-- read-modify-write this replaces raced every other writer of the row
-- (a turn saving usage, the todo tool saving todos), and two children
-- finishing together dropped one of the two deltas.
UPDATE sessions
SET
    cost = cost + ?,
    updated_at = strftime('%s', 'now')
WHERE id = ?;

-- name: SetSessionTodos :exec
-- Write only the todo list. The todo tool runs mid-turn, alongside the
-- turn's own usage saves; a full-row write from either side carried a
-- stale copy of what the other had just written.
UPDATE sessions
SET
    todos = ?,
    updated_at = strftime('%s', 'now')
WHERE id = ?;

-- name: RenameSession :exec
UPDATE sessions
SET
    title = ?
WHERE id = ?;

-- name: SetSessionModel :exec
-- Pin the model a session runs on, so restoring it later restores the
-- model it was working with rather than the instance's current selection.
-- Empty strings clear the pin, returning the session to that fallback.
--
-- Deliberately not RETURNING the row: this is written on every turn, from
-- the dispatch path, and its result is never read back.
UPDATE sessions
SET
    model_provider = ?,
    model_id = ?
WHERE id = ?;

-- name: DeleteSession :exec
DELETE FROM sessions
WHERE id = ?;

-- name: ListSessionTreeIDs :many
-- A session and every descendant of it (agent-tool sub-sessions, title
-- sessions, and their own children), which is the unit a delete has to
-- operate on: parent_session_id carries no foreign key, so nothing
-- cascades from a parent to its children on its own.
WITH RECURSIVE tree(id) AS (
    SELECT sessions.id
    FROM sessions
    WHERE sessions.id = sqlc.arg(session_id)
    UNION ALL
    SELECT sessions.id
    FROM sessions
    JOIN tree ON sessions.parent_session_id = tree.id
)
SELECT tree.id FROM tree;

-- name: ListSessionsForGC :many
-- Every session across every project, trimmed to the columns `sennit gc`
-- needs to compute its retention set (age filter + parent/child
-- expansion) without pulling message/file bodies into memory. Unscoped by
-- project_path; the caller filters by project in Go for --project.
SELECT id, parent_session_id, project_path, updated_at
FROM sessions;
