-- +goose Up
-- +goose StatementBegin
-- UpdateMessage already sets updated_at = strftime('%s', 'now') itself, so
-- the update_messages_updated_at trigger from the initial migration did a
-- second, redundant write on every row it touched -- and message updates
-- happen constantly while a response streams in. Drop it.
--
-- (files has the same trigger shape, but nothing ever issues an UPDATE
-- against files, so that one never fires and is left alone. sessions and
-- threads already got a targeted fix in
-- 20260811000001_preserve_explicit_session_updated_at.sql rather than
-- outright removal, since callers there rely on the trigger to bump
-- updated_at when they don't set it themselves.)
DROP TRIGGER IF EXISTS update_messages_updated_at;

-- ListMessagesBySession filters by session_id and orders by created_at;
-- idx_messages_session_id and idx_messages_created_at each cover half of
-- that but neither serves the query alone. Add the composite index.
CREATE INDEX IF NOT EXISTS idx_messages_session_id_created_at
ON messages (session_id, created_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_messages_session_id_created_at;

CREATE TRIGGER IF NOT EXISTS update_messages_updated_at
AFTER UPDATE ON messages
BEGIN
UPDATE messages SET updated_at = strftime('%s', 'now')
WHERE id = new.id;
END;
-- +goose StatementEnd
