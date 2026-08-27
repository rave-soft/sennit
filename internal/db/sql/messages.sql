-- name: GetMessage :one
SELECT *
FROM messages
WHERE id = ? LIMIT 1;

-- name: ListMessagesBySession :many
SELECT *
FROM messages
WHERE session_id = ?
ORDER BY created_at ASC;

-- name: CreateMessage :one
INSERT INTO messages (
    id,
    session_id,
    role,
    parts,
    model,
    provider,
    is_summary_message,
    origin,
    created_at,
    updated_at
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?, strftime('%s', 'now'), strftime('%s', 'now')
)
RETURNING *;

-- name: UpdateMessage :exec
UPDATE messages
SET
    parts = ?,
    finished_at = ?,
    updated_at = strftime('%s', 'now')
WHERE id = ?;


-- name: DeleteMessage :exec
DELETE FROM messages
WHERE id = ?;

-- name: DeleteSessionMessages :exec
DELETE FROM messages
WHERE session_id = ?;

-- name: CountSessionMessages :one
SELECT COUNT(*) FROM messages
WHERE session_id = ?;

-- name: CountMessagesForSessionIDs :one
-- gc's dependent-row count for a batch of sessions it is about to delete.
-- One aggregate query via json_each rather than an IN-list, so the bound
-- parameter count does not grow with the number of sessions selected --
-- see ListMessagesBySessionIDs for the same pattern.
WITH input AS (
    SELECT CAST(sqlc.arg(session_ids_json) AS TEXT) AS session_ids_json
)
SELECT COUNT(*) FROM messages
WHERE messages.session_id IN (
    SELECT value FROM input, json_each(CAST(input.session_ids_json AS TEXT))
);

-- name: ListUserMessagesBySession :many
SELECT *
FROM messages
WHERE session_id = ? AND role = 'user' AND origin = 'person'
ORDER BY created_at DESC;

-- name: ListAllUserMessages :many
-- Prompt-history source: only messages a human typed. Sub-agent child sessions
-- and thread sessions carry machine-generated prompts as user-role messages.
SELECT messages.*
FROM messages
JOIN sessions ON sessions.id = messages.session_id
WHERE messages.role = 'user'
  AND messages.origin = 'person'
  AND sessions.parent_session_id IS NULL
  AND NOT EXISTS (
      SELECT 1
      FROM threads
      WHERE threads.session_id = sessions.id
  )
ORDER BY messages.created_at DESC;

-- name: ListMessagesBySessionIDs :many
WITH input AS (
    SELECT CAST(sqlc.arg(session_ids_json) AS TEXT) AS session_ids_json
)
SELECT messages.*
FROM messages
WHERE messages.session_id IN (
    SELECT value FROM input, json_each(CAST(input.session_ids_json AS TEXT))
)
ORDER BY messages.session_id, messages.created_at ASC;

-- name: BatchValidateSessionIDsInTree :many
WITH RECURSIVE
input AS (
    SELECT CAST(sqlc.arg(session_ids_json) AS TEXT) AS session_ids_json
),
tree AS (
    SELECT sessions.id AS session_id
    FROM sessions
    WHERE sessions.id = CAST(sqlc.arg(root_session_id) AS TEXT)
      AND sessions.project_path = CAST(sqlc.arg(project_path) AS TEXT)
    UNION ALL
    SELECT sessions.id
    FROM sessions
    JOIN tree ON sessions.parent_session_id = tree.session_id
    WHERE sessions.project_path = CAST(sqlc.arg(project_path) AS TEXT)
)
SELECT tree.session_id AS id
FROM tree
WHERE tree.session_id IN (
    SELECT value FROM input, json_each(CAST(input.session_ids_json AS TEXT))
);

-- name: ListUnfinishedAssistantMessages :many
-- Assistant messages in a project that carry no Finish part, which is
-- what finished_at records (see message.service.write). Every path that
-- ends a turn -- normal completion, error, cancel -- writes one, and every
-- such path runs inside the process that owns the turn. So a row left
-- here belongs to a turn that was killed, and is the starting point for
-- closing it out on the next start.
--
-- Ordered oldest first so a repair walks a session's history in the
-- order it happened.
SELECT m.id, m.session_id, m.role, m.parts, m.model, m.provider,
       m.created_at, m.updated_at, m.finished_at
FROM messages m
JOIN sessions s ON s.id = m.session_id
WHERE s.project_path = ?
  AND m.role = 'assistant'
  AND m.finished_at IS NULL
ORDER BY m.created_at ASC, m.id ASC;

-- name: LastMessageActivity :one
-- The most recent write to any message in the session, as a Unix
-- timestamp. Every streaming delta the assistant produces updates its
-- message row (debounced by tens of milliseconds, see
-- internal/message/store), so this is the cheapest honest "is this session
-- still producing anything" signal there is - and the one the delegation
-- idle watchdog reads. Covered by
-- idx_messages_session_id. Zero means "no messages at all", which the
-- caller reads as "no activity recorded" rather than "the epoch".
SELECT CAST(COALESCE(MAX(updated_at), 0) AS INTEGER) AS last_activity
FROM messages
WHERE session_id = ?;
