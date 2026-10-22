package adminrepo

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jayleaton/ai-patterns-go/packages/database/adminrepo/adminsql"
	"github.com/jayleaton/ai-patterns-go/packages/database/adminrepo/model"
)

// Admins is the admin database's own view of its admins (the admin auth
// instance's tables): removing an admin and reading 2FA failure counters.
// It implements services.TwoFactorAttempts and the removal half of
// services.AdminAccounts (apps/admin).
type Admins struct {
	q *adminsql.Queries
}

// NewAdmins returns the repository over the admin database pool.
func NewAdmins(adminPool *pgxpool.Pool) *Admins {
	return &Admins{q: adminsql.New(adminPool)}
}

// Remove deletes the admin; model.ErrNotFound when there is none.
func (a *Admins) Remove(ctx context.Context, id string) error {
	n, err := a.q.DeleteAdmin(ctx, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return model.ErrNotFound
	}
	return nil
}

// FailedSince lists the admins whose wrong-2FA-code counter moved at or
// after since (auth bumps it, and "updatedAt", on every wrong code and
// resets it on success).
func (a *Admins) FailedSince(ctx context.Context, since time.Time) ([]model.TwoFactorFailure, error) {
	rows, err := a.q.TwoFactorFailuresSince(ctx, since)
	if err != nil {
		return nil, err
	}
	out := make([]model.TwoFactorFailure, len(rows))
	for i, r := range rows {
		out[i] = model.TwoFactorFailure{AdminID: r.AdminID, Email: r.Email, Failures: int64(r.Failures)}
	}
	return out, nil
}
