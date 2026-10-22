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
func (r *AuditReconciler) Reconcile(ctx context.Context, olderThan time.Duration) (ReconcileResult, error) {
	var res ReconcileResult
	var errs []error
	before := r.now().Add(-olderThan)
	skipped := map[int64]bool{}
	for {
		// Skipped entries stay pending and come back first; read past them.
		limit := reconcileBatch + len(skipped)
		pending, err := r.log.Pending(ctx, before, limit)
		if err != nil {
			return res, err
		}
		progressed := false
		for _, e := range pending {
			if skipped[e.ID] {
				continue
			}
			outcome, err := r.verdict(ctx, e)
			if err != nil {
				skipped[e.ID] = true
				res.Skipped++
				errs = append(errs, fmt.Errorf("reconcile audit entry %d (%s): %w", e.ID, e.Action, err))
				continue
			}
			outcome.Reconciled = true
			if err := r.log.Resolve(ctx, e.ID, outcome); err != nil {
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
func (r *AuditReconciler) verdict(ctx context.Context, e AuditEntry) (AuditOutcome, error) {
	holds := func(ok bool) (AuditOutcome, error) {
		if ok {
			return AuditOutcome{Status: StatusSucceeded, Reason: reasonStateMatches}, nil
		}
		return AuditOutcome{Status: StatusFailed, Reason: reasonStateDiffers}, nil
	}
	unknown := AuditOutcome{Status: StatusUnknown, Reason: reasonUndecidable}

	switch e.Action {
	case ActionUserBan, ActionUserUnban, ActionUserSetRole, ActionUserRevokeSession, ActionUserRevokeSessions:
		d, err := r.dir.GetUserDetail(ctx, e.TargetID)
		if errors.Is(err, ErrUserNotFound) {
			return unknown, nil
		}
		if err != nil {
			return AuditOutcome{}, err
		}
		switch e.Action {
		case ActionUserBan:
			return holds(d.User.Banned)
		case ActionUserUnban:
			return holds(!d.User.Banned)
		case ActionUserSetRole:
			return holds(d.User.Role == e.Details["to"])
		case ActionUserRevokeSession:
			return holds(!hasSession(d.Sessions, func(s Session) bool { return s.ID == e.Details["sessionId"] }))
		default: // revoke all: no session from before the attempt survives
			return holds(!hasSession(d.Sessions, func(s Session) bool { return s.CreatedAt.Before(e.CreatedAt) }))
		}

	case ActionAdminDisable, ActionAdminEnable, ActionAdminRemove:
		a, err := r.accounts.AdminByID(ctx, e.TargetID)
		gone := errors.Is(err, ErrRecordNotFound)
		if err != nil && !gone {
			return AuditOutcome{}, err
		}
		switch {
		case e.Action == ActionAdminRemove:
			return holds(gone)
		case gone:
			return unknown, nil
		case e.Action == ActionAdminDisable:
			return holds(a.Disabled)
		default:
			return holds(!a.Disabled)
		}

	case ActionAdminBootstrap, ActionAdminInvite:
		// The intent targets the email (first invite, bootstrap) or the id.
		_, err := r.accounts.AdminByEmail(ctx, e.TargetLabel)
		exists := err == nil
		if err != nil && !errors.Is(err, ErrRecordNotFound) {
			return AuditOutcome{}, err
		}
		if e.Action == ActionAdminBootstrap || !exists {
			return holds(exists) // no admin: the invite (or seeding) did not happen
		}
		return unknown, nil // the admin exists, but whether the email went out cannot be read back
	}
	return unknown, nil
}

func hasSession(sessions []Session, match func(Session) bool) bool {
	for _, s := range sessions {
		if match(s) {
			return true
		}
	}
	return false
}
