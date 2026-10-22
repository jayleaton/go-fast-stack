-- name: AppendAuditEntry :exec
-- A completed fact (sign-ins, ...): no outcome follows. created_at is when it
-- happened (facts can arrive late, from auth's event worker), now() if unset.
INSERT INTO admin_audit_log (actor_id, actor_email, action, target_type, target_id, target_label, details, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, coalesce(sqlc.narg(at)::timestamptz, now()));

-- name: BeginAuditEntry :one
-- The intent of a mutation, written before it runs.
INSERT INTO admin_audit_log (actor_id, actor_email, action, target_type, target_id, target_label, details, intent)
VALUES ($1, $2, $3, $4, $5, $6, $7, true)
RETURNING id;

-- name: ResolveAuditEntry :execrows
-- The first outcome wins (a late request result after the reconciler, or the
-- reverse, is a no-op). Only intents take outcomes.
INSERT INTO admin_audit_outcome (entry_id, status, reason, reconciled)
SELECT l.id, sqlc.arg(status), sqlc.arg(reason), sqlc.arg(reconciled)
FROM admin_audit_log l
WHERE l.id = sqlc.arg(entry_id) AND l.intent
ON CONFLICT (entry_id) DO NOTHING;

-- name: ListAuditEntries :many
SELECT l.id, l.actor_id, l.actor_email, l.action, l.target_type, l.target_id, l.target_label, l.details, l.created_at, l.intent,
       o.status AS outcome_status, o.reason AS outcome_reason, o.reconciled AS outcome_reconciled
FROM admin_audit_log l
LEFT JOIN admin_audit_outcome o ON o.entry_id = l.id
WHERE (sqlc.narg(action)::text IS NULL OR l.action = sqlc.narg(action))
  AND (sqlc.narg(target_id)::text IS NULL OR l.target_id = sqlc.narg(target_id))
ORDER BY l.created_at DESC, l.id DESC
LIMIT sqlc.arg(row_limit) OFFSET sqlc.arg(row_offset);

-- name: CountAuditEntries :one
SELECT count(*) FROM admin_audit_log
WHERE (sqlc.narg(action)::text IS NULL OR action = sqlc.narg(action))
  AND (sqlc.narg(target_id)::text IS NULL OR target_id = sqlc.narg(target_id));

-- name: PendingAuditEntries :many
-- Intents older than the cut-off that never got an outcome, oldest first.
SELECT l.id, l.actor_id, l.actor_email, l.action, l.target_type, l.target_id, l.target_label, l.details, l.created_at
FROM admin_audit_log l
WHERE l.intent AND l.created_at < sqlc.arg(before)
  AND NOT EXISTS (SELECT 1 FROM admin_audit_outcome o WHERE o.entry_id = l.id)
ORDER BY l.created_at, l.id
LIMIT sqlc.arg(row_limit);
