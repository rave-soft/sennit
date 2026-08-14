-- +goose Up
-- +goose StatementBegin
-- Every row up to this migration recorded its parent session (if any)
-- only in memory (threadControl.parentSessionID), lost across a process
-- restart. A DEFAULT of '' backfills existing rows correctly: they
-- genuinely have no recorded parent, and empty is already how this
-- column's absence is treated everywhere it's read.
ALTER TABLE threads ADD COLUMN parent_session_id TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE threads DROP COLUMN parent_session_id;
-- +goose StatementEnd
