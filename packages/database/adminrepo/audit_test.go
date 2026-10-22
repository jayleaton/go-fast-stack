package adminrepo_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jayleaton/ai-patterns-go/packages/database/adminrepo"
	"github.com/jayleaton/ai-patterns-go/packages/database/adminrepo/model"
	"github.com/jayleaton/ai-patterns-go/packages/database/dbtest"
)

func TestAuditLogAppendAndList(t *testing.T) {
	t.Parallel()
	pool := dbtest.New(t, adminDB)
	log := adminrepo.NewAuditLog(pool)
	ctx := context.Background()

	entries := []model.NewAuditEntry{
		{ActorID: "a1", ActorEmail: "root@example.com", Action: "user.ban", TargetType: model.TargetUser, TargetID: "u1", TargetLabel: "ada@example.com", Details: map[string]string{"reason": "spam", "expiresIn": "permanent"}},
		{ActorID: "a1", ActorEmail: "root@example.com", Action: "user.unban", TargetType: model.TargetUser, TargetID: "u1", TargetLabel: "ada@example.com"},
		{ActorID: "system:cli", ActorEmail: "cli", Action: "admin.invite", TargetType: model.TargetAdmin, TargetID: "a2", TargetLabel: "eve@example.com"},
	}
	for _, e := range entries {
		if err := log.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	page, err := log.List(ctx, model.AuditQuery{Limit: 2})
	if err != nil || page.Total != 3 || len(page.Entries) != 2 || page.Limit != 2 {
		t.Fatalf("first page = %+v, %v", page, err)
	}
	if page.Entries[0].Action != "admin.invite" || page.Entries[1].Action != "user.unban" {
		t.Fatalf("not newest first: %s, %s", page.Entries[0].Action, page.Entries[1].Action)
	}
	if page.Entries[1].Details == nil || len(page.Entries[1].Details) != 0 {
		t.Fatalf("nil details must read back as {}: %#v", page.Entries[1].Details)
	}

	page, err = log.List(ctx, model.AuditQuery{Limit: 2, Offset: 2})
	if err != nil || len(page.Entries) != 1 || page.Total != 3 {
		t.Fatalf("second page = %+v, %v", page, err)
	}
	got := page.Entries[0]
	if got.ID == 0 || got.ActorEmail != "root@example.com" || got.TargetLabel != "ada@example.com" ||
		got.Details["reason"] != "spam" || got.Details["expiresIn"] != "permanent" || got.CreatedAt.IsZero() {
		t.Fatalf("entry = %+v", got)
	}

	for name, c := range map[string]struct {
		q     model.AuditQuery
		total int64
	}{
		"by action":          {model.AuditQuery{Action: "user.ban", Limit: 10}, 1},
		"by target":          {model.AuditQuery{TargetID: "u1", Limit: 10}, 2},
		"by action + target": {model.AuditQuery{Action: "admin.invite", TargetID: "u1", Limit: 10}, 0},
	} {
		page, err := log.List(ctx, c.q)
		if err != nil || page.Total != c.total || int64(len(page.Entries)) != c.total || page.Entries == nil {
			t.Errorf("%s: %+v, %v; want %d", name, page, err, c.total)
		}
	}
}

// The log is append-only at the database: even a direct UPDATE or DELETE fails.
func TestAuditLogIsAppendOnly(t *testing.T) {
	t.Parallel()
	pool := dbtest.New(t, adminDB)
	ctx := context.Background()
	if err := adminrepo.NewAuditLog(pool).Append(ctx, model.NewAuditEntry{ActorID: "a1", ActorEmail: "a", Action: "user.ban", TargetType: "user", TargetID: "u1"}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{`UPDATE admin_audit_log SET action = 'x'`, `DELETE FROM admin_audit_log`} {
		if _, err := pool.Exec(ctx, stmt); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: err = %v, want append-only", stmt, err)
		}
	}
}

// Migrate is safe to re-run on every deploy (pre-deploy `admin migrate`).
func TestMigrateIsIdempotent(t *testing.T) {
	t.Parallel()
	pool := dbtest.New(t, adminDB)
	if err := adminrepo.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
}
