-- +goose Up
-- +goose StatementBegin
-- Every row up to this migration is a thread (the only delegation kind
-- that has ever existed in this table), so a DEFAULT backfills existing
-- rows correctly with no data movement — unlike the 20260811000000
-- migration, this one does not need to rebuild the table.
ALTER TABLE threads ADD COLUMN kind TEXT NOT NULL DEFAULT 'thread';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE threads DROP COLUMN kind;
-- +goose StatementEnd
