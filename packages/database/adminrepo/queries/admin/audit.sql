-- name: AppendAuditEntry :exec
INSERT INTO admin_audit_log (actor_id, actor_email, action, target_type, target_id, target_label, details)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: ListAuditEntries :many
SELECT * FROM admin_audit_log
WHERE (sqlc.narg(action)::text IS NULL OR action = sqlc.narg(action))
  AND (sqlc.narg(target_id)::text IS NULL OR target_id = sqlc.narg(target_id))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit) OFFSET sqlc.arg(row_offset);

-- name: CountAuditEntries :one
SELECT count(*) FROM admin_audit_log
WHERE (sqlc.narg(action)::text IS NULL OR action = sqlc.narg(action))
  AND (sqlc.narg(target_id)::text IS NULL OR target_id = sqlc.narg(target_id));
