-- +goose Up
-- The two numbers a compacted context is worth showing: how large the
-- conversation was when it was fed to the summarize pass, and how large
-- the summary that replaced it turned out to be. They are known only
-- once the summarize stream has finished, and nothing else in the schema
-- records them: a session's own token counters are overwritten by the
-- very next turn, so a summary message reopened days later has no way to
-- say what it saved. Zero on every row that is not a finished summary,
-- which is also what rows written before this migration read as.
ALTER TABLE messages ADD COLUMN summary_before_tokens INTEGER DEFAULT 0 NOT NULL;
ALTER TABLE messages ADD COLUMN summary_after_tokens INTEGER DEFAULT 0 NOT NULL;

-- +goose Down
ALTER TABLE messages DROP COLUMN summary_before_tokens;
ALTER TABLE messages DROP COLUMN summary_after_tokens;
