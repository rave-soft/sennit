-- +goose Up
-- +goose StatementBegin

-- files: version numbers are allocated globally per path (NextFileVersion
-- takes MAX(version) over the path alone, and both ListFilesBySessionTree
-- and the UI's per-path diff compare versions across every session in a
-- tree). The old UNIQUE(path, session_id, version) did not express that:
-- it let two sessions each hold their own version 0 of the same path with
-- different content, which is exactly what history.Create used to write.
-- Rebuild with UNIQUE(path, version), renumbering existing rows per path
-- so the surviving order matches the order they were recorded in.
DROP TRIGGER IF EXISTS update_files_updated_at;
DROP INDEX IF EXISTS idx_files_path;
DROP INDEX IF EXISTS idx_files_session_id;
DROP INDEX IF EXISTS idx_files_created_at;

ALTER TABLE files RENAME TO files_old;

CREATE TABLE files (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    path TEXT NOT NULL,
    content TEXT NOT NULL,
    version INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,  -- Unix timestamp in seconds
    updated_at INTEGER NOT NULL,  -- Unix timestamp in seconds
    FOREIGN KEY (session_id) REFERENCES sessions (id) ON DELETE CASCADE,
    UNIQUE(path, version)
);

INSERT INTO files (
    id, session_id, path, content, version, created_at, updated_at
)
SELECT
    id, session_id, path, content,
    -- created_at first, so the renumbering follows the order the
    -- versions were actually recorded in across sessions; version breaks
    -- the ties that a one-second timestamp leaves within a session.
    ROW_NUMBER() OVER (
        PARTITION BY path ORDER BY created_at, version, rowid
    ) - 1,
    created_at, updated_at
FROM files_old;

DROP TABLE files_old;

-- Replaces idx_files_session_id: every session-scoped read of this table
-- (GetFileByPathAndSession, ListLatestSessionFiles) filters on session_id
-- and then on path, and idx_files_path was already redundant with the
-- old UNIQUE index leading on path.
CREATE INDEX IF NOT EXISTS idx_files_session_id_path ON files (session_id, path);
CREATE INDEX IF NOT EXISTS idx_files_created_at ON files (created_at);

CREATE TRIGGER update_files_updated_at
AFTER UPDATE ON files
BEGIN
UPDATE files SET updated_at = strftime('%s', 'now')
WHERE id = new.id;
END;

-- read_files: PRIMARY KEY (path, session_id) already provides an index
-- leading on path, so idx_read_files_path never earned its writes.
DROP INDEX IF EXISTS idx_read_files_path;

-- threads: the unique key has to include kind. Lookups by name are
-- kind-scoped (GetThreadByName filters kind = 'thread'), so without kind
-- in the key a task and a thread sharing a name in one project collide
-- on a constraint neither of them can see. Nothing collides today only
-- because task names are generated as "task-"+uuid.
DROP TRIGGER IF EXISTS update_threads_updated_at;
DROP INDEX IF EXISTS idx_threads_status;
DROP INDEX IF EXISTS idx_threads_project_path;

ALTER TABLE threads RENAME TO threads_old;

CREATE TABLE threads (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    project_path TEXT NOT NULL DEFAULT '',
    goal TEXT NOT NULL,
    base_branch TEXT NOT NULL,
    branch TEXT NOT NULL,
    worktree_path TEXT NOT NULL,
    session_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    merge_policy TEXT NOT NULL DEFAULT 'auto',
    result_summary TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,  -- Unix timestamp in seconds
    updated_at INTEGER NOT NULL,  -- Unix timestamp in seconds
    completed_at INTEGER,         -- Unix timestamp in seconds
    kind TEXT NOT NULL DEFAULT 'thread',
    parent_session_id TEXT NOT NULL DEFAULT '',
    UNIQUE(project_path, kind, name)
);

INSERT INTO threads (
    id, name, project_path, goal, base_branch, branch, worktree_path,
    session_id, status, merge_policy, result_summary, error,
    created_at, updated_at, completed_at, kind, parent_session_id
)
SELECT
    id, name, project_path, goal, base_branch, branch, worktree_path,
    session_id, status, merge_policy, result_summary, error,
    created_at, updated_at, completed_at, kind, parent_session_id
FROM threads_old;

DROP TABLE threads_old;

CREATE INDEX IF NOT EXISTS idx_threads_status ON threads (status);
CREATE INDEX IF NOT EXISTS idx_threads_project_path ON threads (project_path);

CREATE TRIGGER update_threads_updated_at
AFTER UPDATE ON threads
WHEN NEW.updated_at = OLD.updated_at
BEGIN
UPDATE threads SET updated_at = strftime('%s', 'now')
WHERE id = NEW.id;
END;

-- sessions.summary_message_id points at a message but is not a foreign
-- key, so deleting the message left the pointer dangling and the summary
-- lookup in agent/usage.go silently matching nothing. Clear it instead.
CREATE TRIGGER clear_summary_message_id_on_message_delete
AFTER DELETE ON messages
BEGIN
UPDATE sessions SET summary_message_id = NULL
WHERE summary_message_id = old.id;
END;

-- threads.session_id and threads.parent_session_id likewise reference
-- sessions without a foreign key — they cannot have one, because ''
-- ("not attached to a session") is their sentinel and no session row
-- carries that id. Reset them to the sentinel when the session goes
-- away, which is already how every reader treats a thread with no
-- session (see threads_dock.go's attach skip and manager.go's parent
-- notification guards).
CREATE TRIGGER clear_thread_session_refs_on_session_delete
AFTER DELETE ON sessions
BEGIN
UPDATE threads SET session_id = ''
WHERE session_id = old.id;
UPDATE threads SET parent_session_id = ''
WHERE parent_session_id = old.id;
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS clear_thread_session_refs_on_session_delete;
DROP TRIGGER IF EXISTS clear_summary_message_id_on_message_delete;

DROP TRIGGER IF EXISTS update_threads_updated_at;
DROP INDEX IF EXISTS idx_threads_project_path;
DROP INDEX IF EXISTS idx_threads_status;

ALTER TABLE threads RENAME TO threads_old;

CREATE TABLE threads (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    project_path TEXT NOT NULL DEFAULT '',
    goal TEXT NOT NULL,
    base_branch TEXT NOT NULL,
    branch TEXT NOT NULL,
    worktree_path TEXT NOT NULL,
    session_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    merge_policy TEXT NOT NULL DEFAULT 'auto',
    result_summary TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    completed_at INTEGER,
    kind TEXT NOT NULL DEFAULT 'thread',
    parent_session_id TEXT NOT NULL DEFAULT '',
    UNIQUE(project_path, name)
);

INSERT INTO threads (
    id, name, project_path, goal, base_branch, branch, worktree_path,
    session_id, status, merge_policy, result_summary, error,
    created_at, updated_at, completed_at, kind, parent_session_id
)
SELECT
    id, name, project_path, goal, base_branch, branch, worktree_path,
    session_id, status, merge_policy, result_summary, error,
    created_at, updated_at, completed_at, kind, parent_session_id
FROM threads_old;

DROP TABLE threads_old;

CREATE INDEX IF NOT EXISTS idx_threads_status ON threads (status);
CREATE INDEX IF NOT EXISTS idx_threads_project_path ON threads (project_path);

CREATE TRIGGER update_threads_updated_at
AFTER UPDATE ON threads
WHEN NEW.updated_at = OLD.updated_at
BEGIN
UPDATE threads SET updated_at = strftime('%s', 'now')
WHERE id = NEW.id;
END;

CREATE INDEX IF NOT EXISTS idx_read_files_path ON read_files (path);

-- The renumbering done on the way up is not reversible; rows keep their
-- new version numbers and only the constraint and indexes go back.
DROP TRIGGER IF EXISTS update_files_updated_at;
DROP INDEX IF EXISTS idx_files_created_at;
DROP INDEX IF EXISTS idx_files_session_id_path;

ALTER TABLE files RENAME TO files_old;

CREATE TABLE files (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    path TEXT NOT NULL,
    content TEXT NOT NULL,
    version INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    FOREIGN KEY (session_id) REFERENCES sessions (id) ON DELETE CASCADE,
    UNIQUE(path, session_id, version)
);

INSERT INTO files (
    id, session_id, path, content, version, created_at, updated_at
)
SELECT id, session_id, path, content, version, created_at, updated_at
FROM files_old;

DROP TABLE files_old;

CREATE INDEX IF NOT EXISTS idx_files_session_id ON files (session_id);
CREATE INDEX IF NOT EXISTS idx_files_path ON files (path);
CREATE INDEX IF NOT EXISTS idx_files_created_at ON files (created_at);

CREATE TRIGGER update_files_updated_at
AFTER UPDATE ON files
BEGIN
UPDATE files SET updated_at = strftime('%s', 'now')
WHERE id = new.id;
END;
-- +goose StatementEnd
