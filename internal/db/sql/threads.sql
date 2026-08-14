-- name: CreateThread :one
INSERT INTO threads (
    id,
    name,
    project_path,
    goal,
    base_branch,
    branch,
    worktree_path,
    session_id,
    status,
    merge_policy,
    kind,
    parent_session_id,
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
    ?,
    ?,
    ?,
    strftime('%s', 'now'),
    strftime('%s', 'now')
) RETURNING *;

-- name: GetThread :one
-- Unfiltered by kind: entries are addressed by primary key, and callers
-- that hold an id already know what they asked for (id-or-name resolution,
-- RunComplete matching). A kind-scoped caller uses GetThreadByName or
-- ListThreads instead.
SELECT *
FROM threads
WHERE id = ? LIMIT 1;

-- name: GetThreadByName :one
SELECT *
FROM threads
WHERE name = ? AND project_path = ? AND kind = 'thread' LIMIT 1;

-- name: ListThreads :many
-- Thread-facing: thread_list, the dashboard, and any other caller that
-- means "threads" specifically. Scoped to kind = 'thread' so a caller
-- asking for threads never sees another delegation kind sharing this
-- table. The generic lifecycle recovery sweep must NOT use this query;
-- see ListThreadsAll.
SELECT *
FROM threads
WHERE project_path = ? AND kind = 'thread'
ORDER BY created_at;

-- name: ListThreadsAll :many
-- Every delegation kind sharing this table (threads today, tasks once
-- they exist), scoped to project_path but not kind. This is the listing
-- the generic lifecycle recovery sweep uses: recovery must reconcile
-- every kind after a restart, not just threads, or a non-thread row left
-- "running" when the process died would never be caught and would sit
-- displayed as active forever. Not for thread-facing callers; see
-- ListThreads.
SELECT *
FROM threads
WHERE project_path = ?
ORDER BY created_at;

-- name: UpdateThreadStatus :one
UPDATE threads
SET
    status = ?,
    error = ?,
    result_summary = ?,
    completed_at = ?
WHERE id = ?
RETURNING *;

-- name: UpdateThreadSession :one
UPDATE threads
SET
    session_id = ?
WHERE id = ?
RETURNING *;

-- name: DeleteThread :exec
DELETE FROM threads
WHERE id = ?;

-- name: ListThreadsForGC :many
-- Every thread across every project, trimmed to the columns `braid gc`
-- needs to pick finished threads older than the retention cutoff.
-- Unscoped by project_path; the caller filters by project in Go for
-- --project. Scoped by kind = 'thread': gc is a thread-facing caller and
-- must not see other delegation kinds sharing this table.
SELECT id, project_path, status, updated_at
FROM threads
WHERE kind = 'thread';
