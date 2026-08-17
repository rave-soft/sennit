-- The queries below back `sennit stat --by latency`, the per-kind
-- breakdown of how long two internal handoffs waited before reaching the
-- model. Like stats.sql they return raw rows for a time window rather
-- than pre-aggregating: percentiles are computed Go-side over the whole
-- distribution (see internal/stats.ComputeLatency), and SQLite has no
-- percentile aggregate to lean on anyway.

-- name: RecordLatencyEvent :exec
INSERT INTO latency_events (
    session_id,
    kind,
    waited_ms,
    created_at
) VALUES (
    ?,
    ?,
    ?,
    strftime('%s', 'now')
);

-- name: ListLatencyEventsSince :many
-- Scoped by joining sessions rather than by a project_path column of its
-- own: the scope of a latency event is the scope of the session that
-- produced it, and duplicating the path would let the two disagree after
-- a session moves.
SELECT
    latency_events.kind,
    latency_events.waited_ms
FROM latency_events
JOIN sessions ON sessions.id = latency_events.session_id
WHERE latency_events.created_at >= ?
  AND sessions.project_path = ?
ORDER BY latency_events.created_at ASC;

-- name: ListAllLatencyEventsSince :many
SELECT
    kind,
    waited_ms
FROM latency_events
WHERE created_at >= ?
ORDER BY created_at ASC;
