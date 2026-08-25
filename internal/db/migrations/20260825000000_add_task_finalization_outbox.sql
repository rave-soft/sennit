-- +goose Up
-- +goose StatementBegin
ALTER TABLE threads ADD COLUMN completion_pending INTEGER NOT NULL DEFAULT 0;
ALTER TABLE threads ADD COLUMN completion_depth INTEGER NOT NULL DEFAULT 0;
ALTER TABLE threads ADD COLUMN terminal_at INTEGER;
ALTER TABLE threads ADD COLUMN cost_attributed INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE threads DROP COLUMN cost_attributed;
ALTER TABLE threads DROP COLUMN terminal_at;
ALTER TABLE threads DROP COLUMN completion_depth;
ALTER TABLE threads DROP COLUMN completion_pending;
-- +goose StatementEnd
