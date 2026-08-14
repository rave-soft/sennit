-- +goose Up
ALTER TABLE sessions ADD COLUMN agent_id TEXT DEFAULT '' NOT NULL;

-- +goose Down
ALTER TABLE sessions DROP COLUMN agent_id;
