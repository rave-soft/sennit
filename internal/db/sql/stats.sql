-- The queries below back `sennit stat`, a terminal-table
-- breakdown by model/agent/project/skill. They intentionally return raw
-- rows for a time window rather than pre-aggregating, since the
-- model/agent grouping requires Go-side logic (proportional token
-- attribution for multi-model sessions, see internal/cmd/stat.go).

-- name: ListSessionsSince :many
SELECT
    id,
    parent_session_id,
    title,
    prompt_tokens,
    completion_tokens,
    cost,
    created_at,
    updated_at
FROM sessions
WHERE created_at >= ?
  AND project_path = ?
ORDER BY created_at ASC;

-- name: ListAssistantMessagesSince :many
SELECT
    messages.session_id,
    COALESCE(messages.model, 'unknown') as model,
    COALESCE(messages.provider, 'unknown') as provider,
    messages.created_at,
    COALESCE(messages.finished_at, messages.created_at) as finished_at
FROM messages
JOIN sessions ON sessions.id = messages.session_id
WHERE messages.role = 'assistant'
  AND messages.created_at >= ?
  AND sessions.project_path = ?
ORDER BY messages.created_at ASC;

-- name: ListSkillLoadsSince :many
-- Counts skill loads by matching the `view` tool's result metadata. Both
-- JSON layers are guarded with json_valid before being extracted: a
-- message part whose parts blob or whose metadata field is not valid JSON
-- (a tool that records a plain string there, say) would otherwise abort
-- the whole query with "malformed JSON", losing every other skill load
-- along with it. Substituting an empty object makes such a row contribute
-- nothing instead of taking the report down.
WITH parts AS (
    SELECT
        messages.session_id AS session_id,
        messages.created_at AS created_at,
        CASE
            WHEN json_valid(json_extract(value, '$.data.metadata'))
            THEN json_extract(value, '$.data.metadata')
            ELSE '{}'
        END AS metadata
    FROM messages
    JOIN sessions ON sessions.id = messages.session_id,
    json_each(CASE WHEN json_valid(messages.parts) THEN messages.parts ELSE '[]' END)
    WHERE messages.role = 'tool'
      AND messages.created_at >= ?
      AND sessions.project_path = ?
      AND json_extract(value, '$.type') = 'tool_result'
)
SELECT
    json_extract(metadata, '$.resource_name') as skill_name,
    COUNT(*) as load_count,
    COUNT(DISTINCT session_id) as session_count,
    MIN(created_at) as first_used_at,
    MAX(created_at) as last_used_at
FROM parts
WHERE json_extract(metadata, '$.resource_type') = 'skill'
  AND json_extract(metadata, '$.resource_name') IS NOT NULL
GROUP BY skill_name
ORDER BY load_count DESC;

-- name: ProjectStatsSince :many
SELECT
    project_path,
    COUNT(*) as sessions,
    COALESCE(SUM(prompt_tokens), 0) as prompt_tokens,
    COALESCE(SUM(completion_tokens), 0) as completion_tokens,
    COALESCE(SUM(cost), 0) as cost,
    COALESCE(SUM(updated_at - created_at), 0) as time_seconds
FROM sessions
WHERE created_at >= ? AND parent_session_id IS NULL
GROUP BY project_path
ORDER BY (COALESCE(SUM(prompt_tokens), 0) + COALESCE(SUM(completion_tokens), 0)) DESC;

-- The queries below extend the raw-rows-for-a-window shape above to the
-- two scopes `sennit stat` never needed but the TUI's /stats screen does:
-- one session's own tree, and every project at once. They deliberately
-- return the same columns as their project-scoped counterparts so the
-- Go-side aggregation (internal/stats) can treat all three identically.

-- name: ListSessionTreeSince :many
WITH RECURSIVE tree(id) AS (
    SELECT sessions.id FROM sessions WHERE sessions.id = ?
    UNION
    SELECT sessions.id
    FROM sessions
    JOIN tree ON sessions.parent_session_id = tree.id
)
SELECT
    sessions.id,
    sessions.parent_session_id,
    sessions.title,
    sessions.agent_id,
    sessions.prompt_tokens,
    sessions.completion_tokens,
    sessions.cost,
    sessions.created_at,
    sessions.updated_at
FROM sessions
JOIN tree ON tree.id = sessions.id
ORDER BY sessions.created_at ASC;

-- name: ListSessionTreeAssistantMessages :many
WITH RECURSIVE tree(id) AS (
    SELECT sessions.id FROM sessions WHERE sessions.id = ?
    UNION
    SELECT sessions.id
    FROM sessions
    JOIN tree ON sessions.parent_session_id = tree.id
)
SELECT
    messages.session_id,
    COALESCE(messages.model, 'unknown') as model,
    COALESCE(messages.provider, 'unknown') as provider,
    messages.created_at,
    COALESCE(messages.finished_at, messages.created_at) as finished_at
FROM messages
JOIN tree ON tree.id = messages.session_id
WHERE messages.role = 'assistant'
ORDER BY messages.created_at ASC;

-- name: ListAllSessionsSince :many
SELECT
    id,
    parent_session_id,
    title,
    agent_id,
    prompt_tokens,
    completion_tokens,
    cost,
    created_at,
    updated_at
FROM sessions
WHERE created_at >= ?
ORDER BY created_at ASC;

-- name: ListAllAssistantMessagesSince :many
SELECT
    messages.session_id,
    COALESCE(messages.model, 'unknown') as model,
    COALESCE(messages.provider, 'unknown') as provider,
    messages.created_at,
    COALESCE(messages.finished_at, messages.created_at) as finished_at
FROM messages
WHERE messages.role = 'assistant'
  AND messages.created_at >= ?
ORDER BY messages.created_at ASC;

-- name: ListSessionsSinceWithAgent :many
SELECT
    id,
    parent_session_id,
    title,
    agent_id,
    prompt_tokens,
    completion_tokens,
    cost,
    created_at,
    updated_at
FROM sessions
WHERE created_at >= ?
  AND project_path = ?
ORDER BY created_at ASC;

-- name: ListDelegationOutcomesSince :many
-- Reports how each background delegation (a task or a thread) ended,
-- joined to the session that ran it so the outcome can be attributed to
-- an agent. Status is the delegation's own terminal state --
-- completed/merged versus failed/cancelled -- which is as close to "did
-- this work land" as the database gets: whether a reviewer approved the
-- change is not something this process records.
SELECT
    threads.id,
    threads.kind,
    threads.status,
    threads.session_id,
    COALESCE(sessions.agent_id, '') as agent_id,
    COALESCE(sessions.title, '') as title,
    threads.created_at,
    COALESCE(threads.completed_at, threads.updated_at) as completed_at
FROM threads
LEFT JOIN sessions ON sessions.id = threads.session_id
WHERE threads.created_at >= ?
  AND threads.project_path = ?
ORDER BY threads.created_at ASC;

-- name: ListAllDelegationOutcomesSince :many
SELECT
    threads.id,
    threads.kind,
    threads.status,
    threads.session_id,
    COALESCE(sessions.agent_id, '') as agent_id,
    COALESCE(sessions.title, '') as title,
    threads.created_at,
    COALESCE(threads.completed_at, threads.updated_at) as completed_at
FROM threads
LEFT JOIN sessions ON sessions.id = threads.session_id
WHERE threads.created_at >= ?
ORDER BY threads.created_at ASC;
