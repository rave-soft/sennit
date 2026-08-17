-- +goose Up
-- +goose StatementBegin
-- How long an internal handoff waited before it actually reached the
-- model. Two kinds are recorded (see internal/latency): a steering
-- message from submit to the instant it was folded into a step, and a
-- background delegation from its terminal status to the instant its
-- completion was delivered to the parent. Both were already emitted as
-- structured logs; the table exists so a regression shows up in
-- `sennit stat --by latency` instead of only under a human reading logs.
--
-- One row per event, never updated: percentiles need the distribution,
-- and a running mean would hide exactly the tail this is meant to catch.
-- Rows die with their session (ON DELETE CASCADE), which is also what
-- keeps `sennit gc` from needing to know about this table.
CREATE TABLE IF NOT EXISTS latency_events (
    session_id TEXT NOT NULL CHECK (session_id != ''),
    kind TEXT NOT NULL CHECK (kind != ''),
    waited_ms INTEGER NOT NULL CHECK (waited_ms >= 0),
    created_at INTEGER NOT NULL,  -- Unix timestamp in seconds
    FOREIGN KEY (session_id) REFERENCES sessions (id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_latency_events_created_at ON latency_events (created_at);
CREATE INDEX IF NOT EXISTS idx_latency_events_session_id ON latency_events (session_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_latency_events_session_id;
DROP INDEX IF EXISTS idx_latency_events_created_at;
DROP TABLE IF EXISTS latency_events;
-- +goose StatementEnd
