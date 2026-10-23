package adminrepo

import (
	"context"

	"github.com/jayleaton/go-fast-stack/packages/database/adminrepo/adminsql"
	"github.com/jayleaton/go-fast-stack/packages/database/adminrepo/model"
)

// Admins is the admin database's own view of its admins (the admin auth
// instance's tables): removing an admin. It implements the removal half of
// services.AdminAccounts (apps/admin).
type Admins struct {
	queries *adminsql.Queries
}

// NewAdmins returns the repository over the admin database pool.
func NewAdmins(adminPool adminsql.DBTX) *Admins {
	return &Admins{queries: adminsql.New(adminPool)}
}

// Remove deletes the admin; model.ErrNotFound when there is none.
func (admins *Admins) Remove(ctx context.Context, id string) error {
	admin, err := admins.queries.DeleteAdmin(ctx, id)
	if err != nil {
		return err
	}
	if admin == 0 {
		return model.ErrNotFound
	}
	return nil
}
