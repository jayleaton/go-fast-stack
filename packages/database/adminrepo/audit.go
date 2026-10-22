package adminrepo

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jayleaton/ai-patterns-go/packages/database/adminrepo/adminsql"
	"github.com/jayleaton/ai-patterns-go/packages/database/adminrepo/model"
)

// AuditLog appends to and pages admin_audit_log in the admin database. It
// implements services.AuditLog (apps/admin): the method set is the port.
type AuditLog struct {
	q *adminsql.Queries
}

// NewAuditLog returns the audit log backed by the admin database pool (a
// database.Pool, so a rotated ADMIN_DATABASE_URL swaps it, or a *pgxpool.Pool).
func NewAuditLog(adminPool adminsql.DBTX) *AuditLog {
	return &AuditLog{q: adminsql.New(adminPool)}
}

// Append records e.
func (l *AuditLog) Append(ctx context.Context, e model.NewAuditEntry) error {
	details := e.Details
	if details == nil {
		details = map[string]string{}
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("audit details: %w", err)
	}
	return l.q.AppendAuditEntry(ctx, adminsql.AppendAuditEntryParams{
		ActorID: e.ActorID, ActorEmail: e.ActorEmail, Action: e.Action,
		TargetType: e.TargetType, TargetID: e.TargetID, TargetLabel: e.TargetLabel, Details: raw,
	})
}

// List returns one page of entries matching q, newest first, and the total.
func (l *AuditLog) List(ctx context.Context, q model.AuditQuery) (model.AuditPage, error) {
	action, target := optional(q.Action), optional(q.TargetID)
	rows, err := l.q.ListAuditEntries(ctx, adminsql.ListAuditEntriesParams{
		Action: action, TargetID: target, RowLimit: int32(q.Limit), RowOffset: int32(q.Offset), //nolint:gosec // limits are validated (<= 100)
	})
	if err != nil {
		return model.AuditPage{}, err
	}
	total, err := l.q.CountAuditEntries(ctx, adminsql.CountAuditEntriesParams{Action: action, TargetID: target})
	if err != nil {
		return model.AuditPage{}, err
	}
	page := model.AuditPage{Entries: make([]model.AuditEntry, len(rows)), Total: total, Limit: q.Limit, Offset: q.Offset}
	for i, r := range rows {
		var details map[string]string
		if err := json.Unmarshal(r.Details, &details); err != nil {
			return model.AuditPage{}, fmt.Errorf("audit entry %d details: %w", r.ID, err)
		}
		page.Entries[i] = model.AuditEntry{
			ID: r.ID, ActorID: r.ActorID, ActorEmail: r.ActorEmail, Action: r.Action,
			TargetType: r.TargetType, TargetID: r.TargetID, TargetLabel: r.TargetLabel,
			Details: details, CreatedAt: r.CreatedAt,
		}
	}
	return page, nil
}

// optional maps "" (no filter) to NULL.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
