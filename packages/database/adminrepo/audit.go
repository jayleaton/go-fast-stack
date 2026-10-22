package adminrepo

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jayleaton/go-fast-stack/packages/database/adminrepo/adminsql"
	"github.com/jayleaton/go-fast-stack/packages/database/adminrepo/model"
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

// Append records e as a completed fact (status succeeded, no outcome row).
func (l *AuditLog) Append(ctx context.Context, e model.NewAuditEntry) error {
	raw, err := detailsJSON(e.Details)
	if err != nil {
		return err
	}
	return l.q.AppendAuditEntry(ctx, adminsql.AppendAuditEntryParams{
		ActorID: e.ActorID, ActorEmail: e.ActorEmail, Action: e.Action,
		TargetType: e.TargetType, TargetID: e.TargetID, TargetLabel: e.TargetLabel, Details: raw,
		At: pgtype.Timestamptz{Time: e.At, Valid: !e.At.IsZero()}, // zero: now()
	})
}

// Begin records the intent of a mutation (status pending) and returns its id
// for Resolve. Callers must not apply the mutation when it fails.
func (l *AuditLog) Begin(ctx context.Context, e model.NewAuditEntry) (int64, error) {
	raw, err := detailsJSON(e.Details)
	if err != nil {
		return 0, err
	}
	return l.q.BeginAuditEntry(ctx, adminsql.BeginAuditEntryParams{
		ActorID: e.ActorID, ActorEmail: e.ActorEmail, Action: e.Action,
		TargetType: e.TargetType, TargetID: e.TargetID, TargetLabel: e.TargetLabel, Details: raw,
	})
}

// Resolve appends the outcome of the pending entry id. The first outcome
// wins: resolving an entry again (or one that is not an intent) changes
// nothing and is not an error.
func (l *AuditLog) Resolve(ctx context.Context, id int64, o model.AuditOutcome) error {
	_, err := l.q.ResolveAuditEntry(ctx, adminsql.ResolveAuditEntryParams{EntryID: id, Status: o.Status, Reason: o.Reason, Reconciled: o.Reconciled})
	return err
}

// Pending returns up to limit intents written before before that have no
// outcome, oldest first (for the reconciler).
func (l *AuditLog) Pending(ctx context.Context, before time.Time, limit int) ([]model.AuditEntry, error) {
	rows, err := l.q.PendingAuditEntries(ctx, adminsql.PendingAuditEntriesParams{Before: before, RowLimit: int32(limit)}) //nolint:gosec // small, set by the caller
	if err != nil {
		return nil, err
	}
	out := make([]model.AuditEntry, len(rows))
	for i, r := range rows {
		details, err := parseDetails(r.ID, r.Details)
		if err != nil {
			return nil, err
		}
		out[i] = model.AuditEntry{
			ID: r.ID, ActorID: r.ActorID, ActorEmail: r.ActorEmail, Action: r.Action,
			TargetType: r.TargetType, TargetID: r.TargetID, TargetLabel: r.TargetLabel,
			Details: details, CreatedAt: r.CreatedAt, Status: model.StatusPending,
		}
	}
	return out, nil
}

func detailsJSON(details map[string]string) ([]byte, error) {
	if details == nil {
		details = map[string]string{}
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return nil, fmt.Errorf("audit details: %w", err)
	}
	return raw, nil
}

func parseDetails(id int64, raw []byte) (map[string]string, error) {
	var details map[string]string
	if err := json.Unmarshal(raw, &details); err != nil {
		return nil, fmt.Errorf("audit entry %d details: %w", id, err)
	}
	return details, nil
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
		details, err := parseDetails(r.ID, r.Details)
		if err != nil {
			return model.AuditPage{}, err
		}
		e := model.AuditEntry{
			ID: r.ID, ActorID: r.ActorID, ActorEmail: r.ActorEmail, Action: r.Action,
			TargetType: r.TargetType, TargetID: r.TargetID, TargetLabel: r.TargetLabel,
			Details: details, CreatedAt: r.CreatedAt, Status: model.StatusSucceeded,
		}
		switch {
		case r.OutcomeStatus != nil:
			e.Status, e.Reason, e.Reconciled = *r.OutcomeStatus, deref(r.OutcomeReason), r.OutcomeReconciled != nil && *r.OutcomeReconciled
		case r.Intent:
			e.Status = model.StatusPending
		}
		page.Entries[i] = e
	}
	return page, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// optional maps "" (no filter) to NULL.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
