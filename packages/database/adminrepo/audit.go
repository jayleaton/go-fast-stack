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
	queries *adminsql.Queries
}

// NewAuditLog returns the audit log backed by the admin database pool (a
// database.Pool, so a rotated ADMIN_DATABASE_URL swaps it, or a *pgxpool.Pool).
func NewAuditLog(adminPool adminsql.DBTX) *AuditLog {
	return &AuditLog{queries: adminsql.New(adminPool)}
}

// Append records e as a completed fact (status succeeded, no outcome row).
func (log *AuditLog) Append(ctx context.Context, entry model.NewAuditEntry) error {
	raw, err := detailsJSON(entry.Details)
	if err != nil {
		return err
	}
	return log.queries.AppendAuditEntry(ctx, adminsql.AppendAuditEntryParams{
		ActorID: entry.ActorID, ActorEmail: entry.ActorEmail, Action: entry.Action,
		TargetType: entry.TargetType, TargetID: entry.TargetID, TargetLabel: entry.TargetLabel, Details: raw,
		At: pgtype.Timestamptz{Time: entry.At, Valid: !entry.At.IsZero()}, // zero: now()
	})
}

// Begin records the intent of a mutation (status pending) and returns its id
// for Resolve. Callers must not apply the mutation when it fails.
func (log *AuditLog) Begin(ctx context.Context, entry model.NewAuditEntry) (int64, error) {
	raw, err := detailsJSON(entry.Details)
	if err != nil {
		return 0, err
	}
	return log.queries.BeginAuditEntry(ctx, adminsql.BeginAuditEntryParams{
		ActorID: entry.ActorID, ActorEmail: entry.ActorEmail, Action: entry.Action,
		TargetType: entry.TargetType, TargetID: entry.TargetID, TargetLabel: entry.TargetLabel, Details: raw,
	})
}

// Resolve appends the outcome of the pending entry id. The first outcome
// wins: resolving an entry again (or one that is not an intent) changes
// nothing and is not an error.
func (log *AuditLog) Resolve(ctx context.Context, id int64, outcome model.AuditOutcome) error {
	_, err := log.queries.ResolveAuditEntry(ctx, adminsql.ResolveAuditEntryParams{EntryID: id, Status: outcome.Status, Reason: outcome.Reason, Reconciled: outcome.Reconciled})
	return err
}

// Pending returns up to limit intents written before before that have no
// outcome, oldest first (for the reconciler).
func (log *AuditLog) Pending(ctx context.Context, before time.Time, limit int) ([]model.AuditEntry, error) {
	rows, err := log.queries.PendingAuditEntries(ctx, adminsql.PendingAuditEntriesParams{Before: before, RowLimit: int32(limit)}) //nolint:gosec // small, set by the caller
	if err != nil {
		return nil, err
	}
	out := make([]model.AuditEntry, len(rows))
	for index, row := range rows {
		details, err := parseDetails(row.ID, row.Details)
		if err != nil {
			return nil, err
		}
		out[index] = model.AuditEntry{
			ID: row.ID, ActorID: row.ActorID, ActorEmail: row.ActorEmail, Action: row.Action,
			TargetType: row.TargetType, TargetID: row.TargetID, TargetLabel: row.TargetLabel,
			Details: details, CreatedAt: row.CreatedAt, Status: model.StatusPending,
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
func (log *AuditLog) List(ctx context.Context, query model.AuditQuery) (model.AuditPage, error) {
	action, target := optional(query.Action), optional(query.TargetID)
	rows, err := log.queries.ListAuditEntries(ctx, adminsql.ListAuditEntriesParams{
		Action: action, TargetID: target, RowLimit: int32(query.Limit), RowOffset: int32(query.Offset), //nolint:gosec // limits are validated (<= 100)
	})
	if err != nil {
		return model.AuditPage{}, err
	}
	total, err := log.queries.CountAuditEntries(ctx, adminsql.CountAuditEntriesParams{Action: action, TargetID: target})
	if err != nil {
		return model.AuditPage{}, err
	}
	page := model.AuditPage{Entries: make([]model.AuditEntry, len(rows)), Total: total, Limit: query.Limit, Offset: query.Offset}
	for index, row := range rows {
		details, err := parseDetails(row.ID, row.Details)
		if err != nil {
			return model.AuditPage{}, err
		}
		entry := model.AuditEntry{
			ID: row.ID, ActorID: row.ActorID, ActorEmail: row.ActorEmail, Action: row.Action,
			TargetType: row.TargetType, TargetID: row.TargetID, TargetLabel: row.TargetLabel,
			Details: details, CreatedAt: row.CreatedAt, Status: model.StatusSucceeded,
		}
		switch {
		case row.OutcomeStatus != nil:
			entry.Status, entry.Reason, entry.Reconciled = *row.OutcomeStatus, deref(row.OutcomeReason), row.OutcomeReconciled != nil && *row.OutcomeReconciled
		case row.Intent:
			entry.Status = model.StatusPending
		}
		page.Entries[index] = entry
	}
	return page, nil
}

func deref(text *string) string {
	if text == nil {
		return ""
	}
	return *text
}

// optional maps "" (no filter) to NULL.
func optional(text string) *string {
	if text == "" {
		return nil
	}
	return &text
}
