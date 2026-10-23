package auth

import (
	"context"
	"embed"
	"fmt"
	"hash/fnv"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrations holds the embedded auth migrations (goose SQL). sqlc reads the
// same files as schema; test templates hash them to detect schema changes.
var Migrations = func() fs.FS {
	sub, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		panic(err)
	}
	return sub
}()

// MigrationTable is the goose version table for auth migrations. App
// migrations use their own table and run after these.
const MigrationTable = "goose_auth"

// migrationLockID is the Postgres advisory lock held while auth migrations
// tracked in table run: distinct from goose's default so app migrations never
// contend with it, and per table so separate auth instances do not either.
func migrationLockID(table string) int64 {
	hasher := fnv.New64a()
	_, _ = hasher.Write([]byte("auth_mig:" + table))
	return int64(hasher.Sum64() >> 1) //nolint:gosec // shifted into int64 range
}

// Migrate applies the embedded auth migrations (tables user, session, account,
// verification, twoFactor), tracked in MigrationTable. Safe to call from several
// instances at once: an advisory lock serialises them.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return MigrateTable(ctx, pool, MigrationTable)
}

// MigrateTable is Migrate with its own goose version table, for a second auth
// instance (e.g. an admin app) that should not share the default name.
func MigrateTable(ctx context.Context, pool *pgxpool.Pool, table string) error {
	if table == "" {
		table = MigrationTable
	}
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()

	locker, err := lock.NewPostgresSessionLocker(lock.WithLockID(migrationLockID(table)))
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, Migrations,
		goose.WithTableName(table),
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
