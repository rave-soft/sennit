-- +goose Up
ALTER TABLE messages ADD COLUMN origin TEXT DEFAULT 'person' NOT NULL;

-- +goose Down
ALTER TABLE messages DROP COLUMN origin;
