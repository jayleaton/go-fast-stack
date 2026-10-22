package database_test

import (
	"context"
	"testing"

	"github.com/jayleaton/go-fast-stack/packages/database"
	"github.com/jayleaton/go-fast-stack/packages/database/dbtest"
)

func TestMigrateIsIdempotent(t *testing.T) {
	t.Parallel()
	pool := dbtest.New(t, dbtest.App)
	if err := database.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
}

func TestOpen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool, err := database.Open(ctx, dbtest.New(t, dbtest.App).Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()

	// Boot fails fast on a bad URL or an unreachable server, not on the first query.
	for _, url := range []string{"postgres://x:y@127.0.0.1:1/app?connect_timeout=1", "not a url ::"} {
		if _, err := database.Open(ctx, url); err == nil {
			t.Errorf("Open(%q) succeeded", url)
		}
	}
}
