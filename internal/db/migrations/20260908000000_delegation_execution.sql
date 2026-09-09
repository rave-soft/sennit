-- +goose Up
ALTER TABLE threads ADD COLUMN execution TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE threads DROP COLUMN execution;
