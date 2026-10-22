-- +goose Up
-- App tables go here and may reference "user"(id) ON DELETE CASCADE.
-- Auth migrations (version table goose_auth) always run before these (goose_app).
SELECT 1;

-- +goose Down
SELECT 1;
