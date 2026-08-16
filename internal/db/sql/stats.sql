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
SELECT
    json_extract(json_extract(value, '$.data.metadata'), '$.resource_name') as skill_name,
    COUNT(*) as load_count,
    COUNT(DISTINCT messages.session_id) as session_count,
    MIN(messages.created_at) as first_used_at,
    MAX(messages.created_at) as last_used_at
FROM messages
JOIN sessions ON sessions.id = messages.session_id, json_each(messages.parts)
WHERE messages.role = 'tool'
  AND messages.created_at >= ?
  AND sessions.project_path = ?
  AND json_extract(value, '$.type') = 'tool_result'
  AND json_extract(json_extract(value, '$.data.metadata'), '$.resource_type') = 'skill'
  AND json_extract(json_extract(value, '$.data.metadata'), '$.resource_name') IS NOT NULL
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
ORDER BY (prompt_tokens + completion_tokens) DESC;
