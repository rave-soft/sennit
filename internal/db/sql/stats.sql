-- The queries below back `braid stat`, a terminal-table
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
ORDER BY created_at ASC;

-- name: ListAssistantMessagesSince :many
SELECT
    session_id,
    COALESCE(model, 'unknown') as model,
    COALESCE(provider, 'unknown') as provider,
    created_at,
    COALESCE(finished_at, created_at) as finished_at
FROM messages
WHERE role = 'assistant'
  AND created_at >= ?
ORDER BY created_at ASC;

-- name: ListSkillLoadsSince :many
SELECT
    json_extract(json_extract(value, '$.data.metadata'), '$.resource_name') as skill_name,
    COUNT(*) as load_count,
    COUNT(DISTINCT messages.session_id) as session_count,
    MIN(messages.created_at) as first_used_at,
    MAX(messages.created_at) as last_used_at
FROM messages, json_each(messages.parts)
WHERE messages.role = 'tool'
  AND messages.created_at >= ?
  AND json_extract(value, '$.type') = 'tool_result'
  AND json_extract(json_extract(value, '$.data.metadata'), '$.resource_type') = 'skill'
  AND json_extract(json_extract(value, '$.data.metadata'), '$.resource_name') IS NOT NULL
GROUP BY skill_name
ORDER BY load_count DESC;
