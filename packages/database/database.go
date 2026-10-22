// Package database owns the Postgres pool, app migrations and repositories.
// Transports never import it; services reach it through repository interfaces.
package database

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrations holds the embedded app migrations.
var Migrations = mustSub(migrationFiles, "migrations")

// Open connects a pool and verifies the connection.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("database: %w", redactConnErr(err))
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: ping: %w", err)
	}
	return pool, nil
}

// MigrationTable is the goose version table for app migrations.
const MigrationTable = "goose_app"

// Migrate applies app migrations (version table goose_app). Run it after the
// auth migrations: app tables may reference "user"(id).
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return RunMigrations(ctx, pool, MigrationTable, Migrations)
}

// RunMigrations applies the goose migrations in fsys, tracking them in table.
// packages/auth and app migrations each use their own table.
func RunMigrations(ctx context.Context, pool *pgxpool.Pool, table string, fsys fs.FS) error {
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()
	p, err := goose.NewProvider(goose.DialectPostgres, db, fsys, goose.WithTableName(table))
	if err != nil {
		return fmt.Errorf("migrate %s: %w", table, err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("migrate %s: %w", table, err)
	}
	return nil
}

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}
