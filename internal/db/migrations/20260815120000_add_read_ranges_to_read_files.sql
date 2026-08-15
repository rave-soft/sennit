-- +goose Up
-- Which lines of the file were actually read, as a JSON array of [start,
-- end] pairs (1-based, inclusive) — e.g. "[[1,50],[400,460]]". The empty
-- string means the whole file was read, which is also what every row
-- written before this column existed meant, so existing sessions keep
-- working without a backfill.
ALTER TABLE read_files ADD COLUMN read_ranges TEXT DEFAULT '' NOT NULL;

-- +goose Down
ALTER TABLE read_files DROP COLUMN read_ranges;
