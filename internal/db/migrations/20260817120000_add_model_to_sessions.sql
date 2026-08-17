-- +goose Up
-- The model a session runs on. Until now the model was purely a property
-- of the running instance (config.RuntimeOverrides.Model), so restoring an
-- older session silently ran it on whatever model happened to be selected
-- now — the session's own history said one thing and its next turn did
-- another.
--
-- Two columns rather than one, mirroring config.SelectedModel: a model id
-- is only unique within its provider, and the same id can be served by
-- more than one (openrouter and a first-party provider, say).
--
-- Empty is the "not pinned" sentinel, not a model. A session with no
-- recorded model falls back to the instance's selected model, which is
-- exactly the pre-column behaviour.
-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN model_provider TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN model_id TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- Backfill from what each session demonstrably ran on: the newest message
-- that recorded both a provider and a model. That is the assistant's own
-- record of the model that produced it, so it needs no guessing.
--
-- Sessions with no such message keep the empty sentinel: one that never
-- ran has no model to restore, and one whose messages predate the provider
-- column (added 20250627) cannot be attributed to a provider with
-- confidence. Both fall back to the instance's selection, as before.
-- +goose StatementBegin
UPDATE sessions
SET (model_provider, model_id) = (
    SELECT m.provider, m.model
    FROM messages m
    WHERE m.session_id = sessions.id
      AND m.provider IS NOT NULL AND m.provider != ''
      AND m.model IS NOT NULL AND m.model != ''
    ORDER BY m.created_at DESC, m.id DESC
    LIMIT 1
)
WHERE EXISTS (
    SELECT 1
    FROM messages m
    WHERE m.session_id = sessions.id
      AND m.provider IS NOT NULL AND m.provider != ''
      AND m.model IS NOT NULL AND m.model != ''
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN model_provider;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN model_id;
-- +goose StatementEnd
