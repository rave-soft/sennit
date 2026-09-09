-- name: CreateThread :one
-- project_path is denormalized: once session_id is set, the project is
-- also derivable through sessions.project_path. It is stored anyway
-- because a thread exists (and has to be listed and looked up by name)
-- before it has a session at all. The two are kept from diverging by
-- construction (threadspawn.NewStore and session.NewService are both
-- handed the same workspace project path) and by
-- clear_thread_session_refs_on_session_delete, which drops the reference
-- rather than letting it point at a deleted session's project.
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
    kind,
    parent_session_id,
    execution,
    completion_depth,
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

-- name: SetTaskPreparation :one
UPDATE threads
SET base_branch = ?, branch = ?, worktree_path = ?
WHERE id = ? AND kind = 'task' AND status = 'pending'
RETURNING *;

-- name: UpdateThreadSession :one
UPDATE threads
SET
    session_id = ?
WHERE id = ?
RETURNING *;

-- name: AttributeTaskCostOnce :execrows
-- Called in the same transaction as FinalizeTask. A failed transaction rolls
-- this increment back, while the running/marker predicates make retries safe.
UPDATE sessions
SET cost = sessions.cost + COALESCE((SELECT child.cost FROM sessions child WHERE child.id = sqlc.arg(session_id)), 0)
WHERE sessions.id = sqlc.arg(parent_session_id)
  AND EXISTS (
      SELECT 1 FROM threads
      WHERE threads.id = sqlc.arg(id)
        AND threads.kind = 'task'
        AND threads.status = 'running'
        AND threads.session_id = sqlc.arg(session_id)
        AND threads.parent_session_id = sqlc.arg(parent_session_id)
        AND threads.cost_attributed = 0
  );

-- name: FinalizeTask :one
UPDATE threads
SET
    status = sqlc.arg(status),
    error = sqlc.arg(error),
    result_summary = sqlc.arg(result_summary),
    completed_at = sqlc.narg(completed_at),
    terminal_at = MAX(sqlc.arg(terminal_at), COALESCE(terminal_at, 0) + 1),
    completion_depth = sqlc.arg(completion_depth),
    completion_pending = 1,
    cost_attributed = 1
WHERE threads.id = sqlc.arg(id)
  AND threads.kind = 'task'
  AND threads.status IN ('pending', 'running')
  AND threads.session_id = sqlc.arg(session_id)
  AND threads.parent_session_id = sqlc.arg(parent_session_id)
RETURNING *;

-- name: InsertTaskCompletionOutbox :exec
INSERT INTO task_completion_outbox (
    task_id, terminal_at, status, error, result_summary, completion_depth, completed_at,
    name, goal, session_id, parent_session_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListPendingTaskCompletions :many
SELECT threads.*, task_completion_outbox.status, task_completion_outbox.error,
       task_completion_outbox.result_summary, task_completion_outbox.completion_depth,
       task_completion_outbox.completed_at, task_completion_outbox.terminal_at,
       task_completion_outbox.name, task_completion_outbox.goal,
       task_completion_outbox.session_id, task_completion_outbox.parent_session_id
FROM task_completion_outbox
JOIN threads ON threads.id = task_completion_outbox.task_id
WHERE threads.project_path = ?
ORDER BY task_completion_outbox.terminal_at, task_completion_outbox.task_id;

-- name: AcknowledgeTaskCompletionGeneration :execrows
DELETE FROM task_completion_outbox
WHERE task_id = ? AND terminal_at = ?;

-- name: RefreshTaskCompletionPending :execrows
UPDATE threads
SET completion_pending = EXISTS (
    SELECT 1 FROM task_completion_outbox WHERE task_id = threads.id
)
WHERE id = ? AND kind = 'task';

-- name: DeleteThread :exec
DELETE FROM threads
WHERE id = ?;

-- name: ListThreadsForGC :many
-- Every delegation across every project, trimmed to the columns `sennit
-- gc` needs to pick finished ones older than the retention cutoff.
-- Unscoped by project_path; the caller filters by project in Go for
-- --project.
--
-- Deliberately unscoped by kind, unlike the display queries above. gc is
-- not a thread-facing caller -- it is the only thing that reclaims rows
-- here, and a task has nothing else that would: it is never merged (so
-- automatic cleanup may retain it) and the task API has no removal of its
-- own. Scoping this to threads meant finished tasks accumulated for the
-- life of the database. A task carries no worktree, so reclaiming one is
-- the row and its retention alone, with nothing left orphaned on disk.
--
-- kind, worktree_path and branch are selected so gc can report the
-- worktrees it strands: deleting a thread row leaves its worktree on disk
-- with nothing left to find it by, so gc names them before the row goes.
--
-- session_id and parent_session_id are selected so gc can protect a
-- session a non-terminal thread still owns (or still delivers its
-- completion into): selectSessions must never sweep either one, even
-- when it belongs to an otherwise-old session tree, or a live delegation's
-- writes hit sessions.id after the row is gone.
SELECT id, project_path, status, updated_at, kind, worktree_path, branch, session_id, parent_session_id, completion_pending
FROM threads;
