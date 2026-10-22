// Package authstub creates the Better Auth "user" table until packages/auth
// lands. It backs sqlc, repository tests and the dev auth fake.
//
// TODO(feat/auth): delete this package; use auth.Migrate instead.
package authstub

import (
	"context"
	_ "embed"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Schema is the CREATE TABLE statement for "user".
//
//go:embed user.sql
var Schema string

// Migrate creates the "user" table if it does not exist.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, Schema)
	return err
}
