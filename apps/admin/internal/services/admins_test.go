package services_test

import (
	"fmt"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/services/mocks"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/validate"
)

var (
	invite  = validate.CreateAdminInvite{Email: "bob@example.com", Name: "Bob"}
	pending = services.Admin{ID: "a2", Name: "Bob", Email: "bob@example.com"}
)

func newAdminService(t *testing.T) (*services.AdminService, *mocks.MockAdminAccounts, *mocks.MockAuditLog) {
	p := newPorts(t)
	return services.NewAdminService(p.accounts, p.log, services.Policy{RequireTwoFactor: true}), p.accounts, p.log
}

// adminAudit is the entry actor's action on bob writes.
func adminAudit(actor services.Actor, action string, details map[string]string) services.NewAuditEntry {
	return services.NewAuditEntry{
		ActorID: actor.AdminID, ActorEmail: actor.Email, Action: action,
		TargetType: "admin", TargetID: "a2", TargetLabel: "bob@example.com", Details: details,
	}
}

func TestListAdmins(t *testing.T) {
	svc, accounts, _ := newAdminService(t)

	accounts.EXPECT().ListAdmins(ctx).Return([]services.Admin{pending}, nil)
	if got, err := svc.ListAdmins(ctx, grace); err != nil || len(got) != 1 || got[0] != pending {
		t.Fatalf("ListAdmins = %+v, %v", got, err)
	}

	accounts.EXPECT().ListAdmins(ctx).Return(nil, nil)
	if got, err := svc.ListAdmins(ctx, grace); err != nil || got == nil {
		t.Fatalf("no admins = %#v, %v; want an empty, non-nil list", got, err)
	}

	accounts.EXPECT().ListAdmins(ctx).Return(nil, errDB)
	_, err := svc.ListAdmins(ctx, grace)
	wantInternal(t, err, errDB)
}

func TestInviteAdmin(t *testing.T) {
	svc, accounts, log := newAdminService(t)

	// A new admin is created, sent the link, and the invite is audited.
	gomock.InOrder(
		accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(services.Admin{}, fmt.Errorf("get: %w", services.ErrRecordNotFound)),
		accounts.EXPECT().CreateAdmin(ctx, "bob@example.com", "Bob").Return(pending, nil),
		accounts.EXPECT().SendSetPasswordLink(ctx, "a2").Return(nil),
		log.EXPECT().Append(ctx, adminAudit(grace, "admin.invite", map[string]string{})).Return(nil),
	)
	if got, err := svc.InviteAdmin(ctx, grace, invite); err != nil || got != pending {
		t.Fatalf("InviteAdmin = %+v, %v", got, err)
	}

	// A pending invite is sent again, not created again.
	gomock.InOrder(
		accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(pending, nil),
		accounts.EXPECT().SendSetPasswordLink(ctx, "a2").Return(nil),
		log.EXPECT().Append(ctx, adminAudit(grace, "admin.invite", map[string]string{"resent": "true"})).Return(nil),
	)
	if got, err := svc.InviteAdmin(ctx, grace, invite); err != nil || got != pending {
		t.Fatalf("resend = %+v, %v", got, err)
	}

	// An admin who has set a password already is not invited.
	active := pending
	active.HasPassword = true
	accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(active, nil)
	_, err := svc.InviteAdmin(ctx, grace, invite)
	wantErr(t, err, services.ErrAdminExists)

	accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(services.Admin{}, errDB)
	_, err = svc.InviteAdmin(ctx, grace, invite)
	wantInternal(t, err, errDB)

	for _, portErr := range []error{services.ErrAdminExists, errDB} {
		accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(services.Admin{}, services.ErrRecordNotFound)
		accounts.EXPECT().CreateAdmin(ctx, "bob@example.com", "Bob").Return(services.Admin{}, portErr)
		_, err = svc.InviteAdmin(ctx, grace, invite)
		wantErr(t, err, portErr)
	}

	accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(pending, nil)
	accounts.EXPECT().SendSetPasswordLink(ctx, "a2").Return(errDB)
	_, err = svc.InviteAdmin(ctx, grace, invite)
	wantInternal(t, err, errDB)

	accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(pending, nil)
	accounts.EXPECT().SendSetPasswordLink(ctx, "a2").Return(nil)
	log.EXPECT().Append(ctx, gomock.Any()).Return(errDB)
	_, err = svc.InviteAdmin(ctx, grace, invite)
	wantInternal(t, err, errDB)
}

func TestBootstrap(t *testing.T) {
	svc, accounts, log := newAdminService(t)

	gomock.InOrder(
		accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(services.Admin{}, services.ErrRecordNotFound),
		accounts.EXPECT().CreateAdmin(ctx, "bob@example.com", "Bob").Return(pending, nil),
		log.EXPECT().Append(ctx, adminAudit(cli, "admin.bootstrap", nil)).Return(nil),
	)
	if got, created, err := svc.Bootstrap(ctx, cli, invite); err != nil || !created || got != pending {
		t.Fatalf("Bootstrap = %+v, %v, %v", got, created, err)
	}

	// Idempotent: an existing admin is returned, nothing is created or audited.
	accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(pending, nil)
	if got, created, err := svc.Bootstrap(ctx, cli, invite); err != nil || created || got != pending {
		t.Fatalf("existing = %+v, %v, %v", got, created, err)
	}

	accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(services.Admin{}, errDB)
	_, created, err := svc.Bootstrap(ctx, cli, invite)
	wantInternal(t, err, errDB)
	if created {
		t.Fatal("created on error")
	}

	accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(services.Admin{}, services.ErrRecordNotFound)
	accounts.EXPECT().CreateAdmin(ctx, "bob@example.com", "Bob").Return(services.Admin{}, errDB)
	_, _, err = svc.Bootstrap(ctx, cli, invite)
	wantInternal(t, err, errDB)

	accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(services.Admin{}, services.ErrRecordNotFound)
	accounts.EXPECT().CreateAdmin(ctx, "bob@example.com", "Bob").Return(pending, nil)
	log.EXPECT().Append(ctx, gomock.Any()).Return(errDB)
	_, _, err = svc.Bootstrap(ctx, cli, invite)
	wantInternal(t, err, errDB)
}
