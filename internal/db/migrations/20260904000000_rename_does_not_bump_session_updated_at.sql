-- +goose Up
-- +goose StatementBegin
-- Name the columns the auto-bump reacts to, instead of reacting to every
-- UPDATE. The WHEN guard alone cannot tell "the writer left updated_at
-- alone" from "the writer set it to the value it already had", so a
-- title-only write (RenameSession) was indistinguishable from a turn's
-- own save and bumped updated_at with it. That made renaming a
-- year-old session move it to the top of ListSessions, hand it to
-- GetLastSession as the session to resume, reset its age for gc, and add
-- a year to time_seconds in ProjectStatsSince, which reads
-- updated_at - created_at.
--
-- The list is every column except two: title, which is what someone
-- typed rather than work the session did, and updated_at itself, whose
-- explicit writers the WHEN guard still protects. SQLite fires UPDATE OF
-- on the columns named in a statement's SET clause, whether or not the
-- value actually changes, so UpdateSession (title plus five work
-- columns) still bumps while RenameSession (title alone) does not.
--
-- TestSessionUpdatedAtTriggerCoversEveryWorkColumn fails if a column is
-- added to the table and not to this list: without that gate, a new
-- column's writer would silently stop bumping updated_at.
DROP TRIGGER IF EXISTS update_sessions_updated_at;

CREATE TRIGGER update_sessions_updated_at
AFTER UPDATE OF
    id,
    parent_session_id,
    message_count,
    prompt_tokens,
    completion_tokens,
    cost,
    created_at,
    summary_message_id,
    todos,
    agent_id,
    project_path,
    model_provider,
    model_id
ON sessions
WHEN NEW.updated_at = OLD.updated_at
BEGIN
UPDATE sessions SET updated_at = strftime('%s', 'now')
WHERE id = NEW.id;
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS update_sessions_updated_at;

CREATE TRIGGER update_sessions_updated_at
AFTER UPDATE ON sessions
WHEN NEW.updated_at = OLD.updated_at
BEGIN
UPDATE sessions SET updated_at = strftime('%s', 'now')
WHERE id = NEW.id;
END;
-- +goose StatementEnd
