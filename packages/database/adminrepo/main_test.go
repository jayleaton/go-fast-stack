package adminrepo_test

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jayleaton/ai-patterns-go/packages/auth"
	"github.com/jayleaton/ai-patterns-go/packages/database/adminrepo"
	"github.com/jayleaton/ai-patterns-go/packages/database/dbtest"
)

// adminDB is the admin database: the admin auth instance's tables, then the
// admin migrations. App-database tests use dbtest.App.
var adminDB = dbtest.Template{
	Name:        "admin",
	Fingerprint: dbtest.Fingerprint(auth.Migrations, adminrepo.Migrations),
	Setup: func(ctx context.Context, pool *pgxpool.Pool) error {
		if err := auth.Migrate(ctx, pool); err != nil {
			return err
		}
		return adminrepo.Migrate(ctx, pool)
	},
}
