// Package adminrepo is the admin app's higher-privilege data layer: the audit
// log in the admin database and read-only statistics over the main app's
// database. Only apps/admin may import it; depguard keeps apps/web (and
// packages/database's own repositories) out.
//
// User administration (list, ban, roles, sessions) is not here: packages/auth
// owns those tables and rules, and apps/admin reaches it through
// auth.UserAdmin.
package adminrepo

import (
	"context"
	"embed"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jayleaton/go-fast-stack/packages/database"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrations holds the embedded admin-database migrations.
var Migrations = func() fs.FS {
	sub, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		panic(err)
	}
	return sub
}()

// MigrationTable is the goose version table of the admin migrations.
const MigrationTable = "goose_admin"

// Migrate applies the admin migrations to the admin database. Run it after
// the admin auth instance's migrations (goose_auth on the same database).
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return database.RunMigrations(ctx, pool, MigrationTable, Migrations)
}
