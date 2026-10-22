package adminrepo

import (
	"context"

	"github.com/jayleaton/ai-patterns-go/packages/database/adminrepo/adminsql"
	"github.com/jayleaton/ai-patterns-go/packages/database/adminrepo/model"
)

// Admins is the admin database's own view of its admins (the admin auth
// instance's tables): removing an admin. It implements the removal half of
// services.AdminAccounts (apps/admin).
type Admins struct {
	q *adminsql.Queries
}

// NewAdmins returns the repository over the admin database pool.
func NewAdmins(adminPool adminsql.DBTX) *Admins {
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
