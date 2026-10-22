package adminrepo_test

import (
	"context"
	"errors"
	"testing"
	"time"

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

func TestAdminsFailedSince(t *testing.T) {
	t.Parallel()
	pool := dbtest.New(t, adminDB)
	ctx := context.Background()
	since := time.Now().Add(-time.Minute)
	_, err := pool.Exec(ctx, `INSERT INTO "user" (id, name, email) VALUES
			('a1', 'Root', 'root@example.com'), ('a2', 'Eve', 'eve@example.com'), ('a3', 'Bob', 'bob@example.com');
		INSERT INTO "twoFactor" (id, secret, "backupCodes", "userId", verified, "failedVerificationCount", "updatedAt") VALUES
			('t1', 'x', '[]', 'a1', true, 2, now()),                       -- failed just now
			('t2', 'x', '[]', 'a2', true, 0, now()),                       -- just succeeded (counter reset)
			('t3', 'x', '[]', 'a3', true, 4, now() - interval '1 hour')`) // failed long ago
	if err != nil {
		t.Fatal(err)
	}

	got, err := adminrepo.NewAdmins(pool).FailedSince(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	want := []model.TwoFactorFailure{{AdminID: "a1", Email: "root@example.com", Failures: 2}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("FailedSince = %+v, want %+v", got, want)
	}
	if got, err := adminrepo.NewAdmins(pool).FailedSince(ctx, time.Now().Add(time.Minute)); err != nil || len(got) != 0 {
		t.Fatalf("nothing since the future: %+v, %v", got, err)
	}
}
