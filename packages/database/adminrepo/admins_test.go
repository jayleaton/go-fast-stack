package adminrepo_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jayleaton/ai-patterns-go/packages/database/adminrepo"
	"github.com/jayleaton/ai-patterns-go/packages/database/adminrepo/model"
	"github.com/jayleaton/ai-patterns-go/packages/database/dbtest"
)

func TestAdminsRemove(t *testing.T) {
	t.Parallel()
	pool := dbtest.New(t, adminDB)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO "user" (id, name, email) VALUES ('a1', 'Root', 'root@example.com'), ('a2', 'Eve', 'eve@example.com');
		INSERT INTO "session" (id, token, "userId", "expiresAt") VALUES ('s1', 't1', 'a2', now() + interval '1 day')`)
	if err != nil {
		t.Fatal(err)
	}
	admins := adminrepo.NewAdmins(pool)

	if err := admins.Remove(ctx, "a2"); err != nil {
		t.Fatal(err)
	}
	var users, sessions int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM "user"), (SELECT count(*) FROM "session")`).Scan(&users, &sessions); err != nil {
		t.Fatal(err)
	}
	if users != 1 || sessions != 0 {
		t.Fatalf("after remove: %d users, %d sessions; want 1, 0 (sessions cascade)", users, sessions)
	}
	if err := admins.Remove(ctx, "a2"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("remove missing: err = %v, want ErrNotFound", err)
	}
}
