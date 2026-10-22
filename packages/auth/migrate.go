package auth

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// MigrationTable is the goose version table for auth migrations. App
// migrations use their own table and run after these.
const MigrationTable = "goose_auth"

// migrationLockID is the Postgres advisory lock held while auth migrations
// run, distinct from goose's default so app migrations never contend with it.
const migrationLockID int64 = 0x617574685f6d6967 // "auth_mig"

// Migrate applies the embedded auth migrations (tables user, session, account,
// verification). Safe to call from several instances at once: an advisory lock
// serialises them.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()

	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return err
	}
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockID(migrationLockID))
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations,
		goose.WithTableName(MigrationTable),
		goose.WithSessionLocker(locker),
		goose.WithDisableGlobalRegistry(true),
	)
	if err != nil {
		return fmt.Errorf("auth: migrations: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("auth: migrations: %w", err)
	}
	return nil
}
