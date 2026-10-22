package adminrepo_test

import (
	"context"
	"testing"

	"github.com/jayleaton/ai-patterns-go/packages/database/adminrepo"
	"github.com/jayleaton/ai-patterns-go/packages/database/dbtest"
)

func TestStatsOverview(t *testing.T) {
	t.Parallel()
	pool := dbtest.New(t, dbtest.App)
	ctx := context.Background()
	stats := adminrepo.NewStats(pool)

	empty, err := stats.Overview(ctx)
	if err != nil || empty.TotalUsers != 0 || empty.GeneratedAt.IsZero() {
		t.Fatalf("empty overview = %+v, %v", empty, err)
	}

	_, err = pool.Exec(ctx, `INSERT INTO "user" (id, name, email, "emailVerified", "createdAt") VALUES
		('u1', 'Ada', 'ada@example.com', true,  now()),
		('u2', 'Eve', 'eve@example.com', false, now() - interval '30 days'),
		('u3', 'Bob', 'bob@example.com', true,  now() - interval '1 day')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO "session" (id, token, "userId", "expiresAt") VALUES
		('s1', 't1', 'u1', now() + interval '1 day'),
		('s2', 't2', 'u3', now() + interval '1 day'),
		('s3', 't3', 'u2', now() - interval '1 day')`)
	if err != nil {
		t.Fatal(err)
	}

	o, err := stats.Overview(ctx)
	if err != nil || o.TotalUsers != 3 || o.VerifiedUsers != 2 || o.NewUsers7d != 2 || o.ActiveSessions != 2 {
		t.Fatalf("overview = %+v, %v", o, err)
	}
}
