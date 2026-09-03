-- +goose Up
-- read_at now holds a Unix epoch timestamp in milliseconds. Scale
-- existing rows, which were written in seconds, up to match.
UPDATE read_files SET read_at = read_at * 1000;

-- +goose Down
-- Reverts read_at to seconds. Integer division loses the millisecond
-- component; goose.Down is only ever called from this package's own
-- tests to reset a fixture mid-test, never from application code
-- (internal/db/connect.go and internal/db/template.go call goose.Up
-- only), so the lossy round trip has no production exposure.
UPDATE read_files SET read_at = read_at / 1000;
