package dbtest

import (
	"context"
	"io/fs"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jayleaton/ai-patterns-go/packages/auth"
	"github.com/jayleaton/ai-patterns-go/packages/database"
)

// App is the full app schema: auth tables, then app migrations.
var App = Template{
	Name:        "app",
	Fingerprint: fingerprint(auth.Migrations) + fingerprint(database.Migrations),
	Setup: func(ctx context.Context, pool *pgxpool.Pool) error {
		if err := auth.Migrate(ctx, pool); err != nil {
			return err
		}
		return database.Migrate(ctx, pool)
	},
}

func fingerprint(fsys fs.FS) string {
	var b strings.Builder
	_ = fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(fsys, path)
		b.WriteString(path)
		b.Write(data)
		return err
	})
	return b.String()
}
