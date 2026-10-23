package services

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// reconcileBatch is how many stale entries one pass reads at a time.
const reconcileBatch = 100

// Reconcile reasons (shown in the audit log next to the status).
const (
	reasonStateMatches = "Reconciled: the current state shows the change"
	reasonStateDiffers = "Reconciled: the current state does not show the change"
	reasonUndecidable  = "Reconciled: the outcome cannot be told from the current state"
)

// ReconcileResult counts what one Reconcile pass did.
type ReconcileResult struct {
	Succeeded, Failed, Unknown int // resolved, by outcome
	Skipped                    int // left pending: the state could not be read (retried next pass)
}

// AuditReconciler resolves pending audit entries whose outcome was never
// written (the request crashed, or the outcome write failed after the
// change). It asks whether the intended end state holds now: a ban entry is
// succeeded if the user is banned, failed if not. A later action may have
// changed the state since, so a reconciled outcome is marked as such and is
// the best available evidence, not proof. Entries whose effect cannot be
// read back (an emailed invite) become unknown.
type AuditReconciler struct {
	log      AuditLog
	dir      UserDirectory
	accounts AdminAccounts
	now      func() time.Time
}

// NewAuditReconciler returns a reconciler over its ports.
func NewAuditReconciler(log AuditLog, dir UserDirectory, accounts AdminAccounts) *AuditReconciler {
	return &AuditReconciler{log: log, dir: dir, accounts: accounts, now: time.Now}
}

// Reconcile resolves every entry still pending olderThan after it was
// written (younger ones may still be in flight). Entries whose state cannot
// be read stay pending and their errors are returned joined.
func (reconciler *AuditReconciler) Reconcile(ctx context.Context, olderThan time.Duration) (ReconcileResult, error) {
	var res ReconcileResult
	var errs []error
	before := reconciler.now().Add(-olderThan)
	skipped := map[int64]bool{}
	for {
		// Skipped entries stay pending and come back first; read past them.
		limit := reconcileBatch + len(skipped)
		pending, err := reconciler.log.Pending(ctx, before, limit)
		if err != nil {
			return res, err
		}
		progressed := false
		for _, entry := range pending {
			if skipped[entry.ID] {
				continue
			}
			outcome, err := reconciler.verdict(ctx, entry)
			if err != nil {
				skipped[entry.ID] = true
				res.Skipped++
				errs = append(errs, fmt.Errorf("reconcile audit entry %d (%s): %w", entry.ID, entry.Action, err))
				continue
			}
			outcome.Reconciled = true
			if err := reconciler.log.Resolve(ctx, entry.ID, outcome); err != nil {
				return res, errors.Join(append(errs, err)...)
			}
			progressed = true
			switch outcome.Status {
			case StatusSucceeded:
				res.Succeeded++
			case StatusFailed:
				res.Failed++
			default:
				res.Unknown++
			}
		}
		if !progressed || len(pending) < limit {
			return res, errors.Join(errs...)
		}
	}
}

// verdict reads the current state the entry's change should have produced.
// A read error leaves the entry pending (error); a target that no longer
// exists, or an action whose effect cannot be read, is unknown.
func (reconciler *AuditReconciler) verdict(ctx context.Context, entry AuditEntry) (AuditOutcome, error) {
	holds := func(ok bool) (AuditOutcome, error) {
		if ok {
			return AuditOutcome{Status: StatusSucceeded, Reason: reasonStateMatches}, nil
		}
		return AuditOutcome{Status: StatusFailed, Reason: reasonStateDiffers}, nil
	}
	unknown := AuditOutcome{Status: StatusUnknown, Reason: reasonUndecidable}

	switch entry.Action {
	case ActionUserBan, ActionUserUnban, ActionUserSetRole, ActionUserRevokeSession, ActionUserRevokeSessions:
		detail, err := reconciler.dir.GetUserDetail(ctx, entry.TargetID)
		if errors.Is(err, ErrUserNotFound) {
			return unknown, nil
		}
		if err != nil {
			return AuditOutcome{}, err
		}
		switch entry.Action {
		case ActionUserBan:
			return holds(detail.User.Banned)
		case ActionUserUnban:
			return holds(!detail.User.Banned)
		case ActionUserSetRole:
			return holds(detail.User.Role == entry.Details["to"])
		case ActionUserRevokeSession:
			return holds(!hasSession(detail.Sessions, func(session Session) bool { return session.ID == entry.Details["sessionId"] }))
		default: // revoke all: no session from before the attempt survives
			return holds(!hasSession(detail.Sessions, func(session Session) bool { return session.CreatedAt.Before(entry.CreatedAt) }))
		}

	case ActionAdminDisable, ActionAdminEnable, ActionAdminRemove, ActionAdminResetTwoFactor:
		admin, err := reconciler.accounts.AdminByID(ctx, entry.TargetID)
		gone := errors.Is(err, ErrRecordNotFound)
		if err != nil && !gone {
			return AuditOutcome{}, err
		}
		switch {
		case entry.Action == ActionAdminRemove:
			return holds(gone)
		case gone:
			return unknown, nil
		case entry.Action == ActionAdminDisable:
			return holds(admin.Disabled)
		case entry.Action == ActionAdminResetTwoFactor:
			return holds(!admin.TwoFactorEnabled)
		default:
			return holds(!admin.Disabled)
		}

	case ActionAdminBootstrap, ActionAdminInvite:
		// The intent targets the email (first invite, bootstrap) or the id.
		_, err := reconciler.accounts.AdminByEmail(ctx, entry.TargetLabel)
		exists := err == nil
		if err != nil && !errors.Is(err, ErrRecordNotFound) {
			return AuditOutcome{}, err
		}
		if entry.Action == ActionAdminBootstrap || !exists {
			return holds(exists) // no admin: the invite (or seeding) did not happen
		}
		return unknown, nil // the admin exists, but whether the email went out cannot be read back
	}
	return unknown, nil
}

func hasSession(sessions []Session, match func(Session) bool) bool {
	for _, session := range sessions {
		if match(session) {
			return true
		}
	}
	return false
}
