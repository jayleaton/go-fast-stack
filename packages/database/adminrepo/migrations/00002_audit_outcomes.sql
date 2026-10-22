-- Admin mutations fail closed: an intent row (intent = true) is written
-- BEFORE the change, its outcome AFTER, in a second append-only table. Rows
-- written before this migration recorded completed actions (intent = false).
-- An intent without an outcome is an attempt whose result was not recorded;
-- the reconciler resolves stale ones.

-- +goose Up
ALTER TABLE admin_audit_log ADD COLUMN intent boolean NOT NULL DEFAULT false;
CREATE INDEX admin_audit_log_intent_idx ON admin_audit_log (created_at) WHERE intent;

CREATE TABLE admin_audit_outcome (
    entry_id   bigint PRIMARY KEY REFERENCES admin_audit_log (id), -- one outcome per intent
    status     text NOT NULL CHECK (status IN ('succeeded', 'failed', 'unknown')),
    reason     text NOT NULL DEFAULT '',  -- safe to show: a service message, never a raw error
    reconciled boolean NOT NULL DEFAULT false, -- written by the reconciler, not the request
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Same guarantee as the log: never edited or deleted.
CREATE TRIGGER admin_audit_outcome_append_only
    BEFORE UPDATE OR DELETE ON admin_audit_outcome
    FOR EACH ROW EXECUTE FUNCTION admin_audit_log_append_only();

-- +goose Down
DROP TABLE admin_audit_outcome;
DROP INDEX admin_audit_log_intent_idx;
ALTER TABLE admin_audit_log DROP COLUMN intent;
