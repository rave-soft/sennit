-- +goose Up
-- +goose StatementBegin
ALTER TABLE strands RENAME TO threads;

DROP INDEX IF EXISTS idx_strands_status;
CREATE INDEX IF NOT EXISTS idx_threads_status ON threads (status);

DROP TRIGGER IF EXISTS update_strands_updated_at;
CREATE TRIGGER IF NOT EXISTS update_threads_updated_at
AFTER UPDATE ON threads
BEGIN
UPDATE threads SET updated_at = strftime('%s', 'now')
WHERE id = new.id;
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS update_threads_updated_at;
DROP INDEX IF EXISTS idx_threads_status;

ALTER TABLE threads RENAME TO strands;

CREATE INDEX IF NOT EXISTS idx_strands_status ON strands (status);

CREATE TRIGGER IF NOT EXISTS update_strands_updated_at
AFTER UPDATE ON strands
BEGIN
UPDATE strands SET updated_at = strftime('%s', 'now')
WHERE id = new.id;
END;
-- +goose StatementEnd
