-- Admin database: the audit log of every mutating admin action. Admins
-- themselves live in this database's auth tables (packages/auth, goose_auth).

-- +goose Up
CREATE TABLE admin_audit_log (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    actor_id     text NOT NULL,             -- admin user id, or "system:<name>" for the CLI
    actor_email  text NOT NULL,
    action       text NOT NULL,             -- "user.ban", "admin.invite", ...
    target_type  text NOT NULL,             -- "user" (main app) or "admin"
    target_id    text NOT NULL,
    target_label text NOT NULL DEFAULT '',  -- the target's email at the time
    details      jsonb NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX admin_audit_log_created_idx ON admin_audit_log (created_at DESC, id DESC);
CREATE INDEX admin_audit_log_target_idx ON admin_audit_log (target_id, created_at DESC);
CREATE INDEX admin_audit_log_action_idx ON admin_audit_log (action, created_at DESC);

-- Append-only: rows are never edited or deleted by the app. Drop the trigger
-- in a migration if you add a retention policy.
-- +goose StatementBegin
CREATE FUNCTION admin_audit_log_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'admin_audit_log is append-only';
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER admin_audit_log_append_only
    BEFORE UPDATE OR DELETE ON admin_audit_log
    FOR EACH ROW EXECUTE FUNCTION admin_audit_log_append_only();

-- +goose Down
DROP TABLE admin_audit_log;
DROP FUNCTION admin_audit_log_append_only();
