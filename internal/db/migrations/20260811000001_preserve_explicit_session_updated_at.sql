-- +goose Up
-- +goose StatementBegin
-- Auto-bump updated_at only when the writer did not set it explicitly
-- (NEW = OLD means the UPDATE statement left the column untouched).
-- Writers that DO set updated_at — tests fabricating old sessions, and
-- back then a legacy per-project importer restoring original timestamps
-- — keep their value.
DROP TRIGGER IF EXISTS update_sessions_updated_at;

CREATE TRIGGER update_sessions_updated_at
AFTER UPDATE ON sessions
WHEN NEW.updated_at = OLD.updated_at
BEGIN
UPDATE sessions SET updated_at = strftime('%s', 'now')
WHERE id = NEW.id;
END;

DROP TRIGGER IF EXISTS update_threads_updated_at;

CREATE TRIGGER update_threads_updated_at
AFTER UPDATE ON threads
WHEN NEW.updated_at = OLD.updated_at
BEGIN
UPDATE threads SET updated_at = strftime('%s', 'now')
WHERE id = NEW.id;
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS update_sessions_updated_at;

CREATE TRIGGER update_sessions_updated_at
AFTER UPDATE ON sessions
BEGIN
UPDATE sessions SET updated_at = strftime('%s', 'now')
WHERE id = NEW.id;
END;

DROP TRIGGER IF EXISTS update_threads_updated_at;

CREATE TRIGGER update_threads_updated_at
AFTER UPDATE ON threads
BEGIN
UPDATE threads SET updated_at = strftime('%s', 'now')
WHERE id = NEW.id;
END;
-- +goose StatementEnd
