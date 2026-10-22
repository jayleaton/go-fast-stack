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
	bob     = services.Admin{ID: "a2", Name: "Bob", Email: "bob@example.com", HasPassword: true}
	disBob  = services.Admin{ID: "a2", Name: "Bob", Email: "bob@example.com", HasPassword: true, Disabled: true}
	graceAd = services.Admin{ID: "a1", Name: "Grace", Email: "grace@example.com", HasPassword: true, TwoFactorEnabled: true}

	// priorInvite and priorEmailInvite are the audit lookups for an earlier
	// invite of bob: under his id, or under his email (a first invite).
	priorInvite      = services.AuditQuery{Action: "admin.invite", TargetID: "a2", Limit: 1}
	priorEmailInvite = services.AuditQuery{Action: "admin.invite", TargetID: "bob@example.com", Limit: 1}
)

func newAdminService(t *testing.T) (*services.AdminService, *mocks.MockAdminAccounts, *mocks.MockAuditLog) {
	p := newPorts(t)
	return services.NewAdminService(p.accounts, p.log, services.Policy{RequireTwoFactor: true}), p.accounts, p.log
}

// adminAudit is the intent actor's action on bob writes.
func adminAudit(actor services.Actor, action string, details map[string]string) services.NewAuditEntry {
	return services.NewAuditEntry{
		ActorID: actor.AdminID, ActorEmail: actor.Email, Action: action,
		TargetType: "admin", TargetID: "a2", TargetLabel: "bob@example.com", Details: details,
	}
}

func TestListAdmins(t *testing.T) {
	t.Parallel()
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

// The generic fail-closed cases (intent or outcome not written, a refused or
// failing change) of every mutation are in TestMutationsFailClosed.

func TestInviteAdmin(t *testing.T) {
	t.Parallel()
	svc, accounts, log := newAdminService(t)
	notFound := fmt.Errorf("get: %w", services.ErrRecordNotFound)

	// A new admin is created and sent the link inside the audited change; the
	// intent targets the email, since there is no id yet.
	gomock.InOrder(
		accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(services.Admin{}, notFound),
		log.EXPECT().Begin(ctx, services.NewAuditEntry{
			ActorID: "a1", ActorEmail: "grace@example.com", Action: "admin.invite",
			TargetType: "admin", TargetID: "bob@example.com", TargetLabel: "bob@example.com", Details: map[string]string{},
		}).Return(entryID, nil),
		accounts.EXPECT().CreateAdmin(ctx, "bob@example.com", "Bob").Return(pending, nil),
		accounts.EXPECT().SendSetPasswordLink(ctx, "a2").Return(nil),
		log.EXPECT().Resolve(ctx, entryID, succeeded).Return(nil),
	)
	if got, err := svc.InviteAdmin(ctx, grace, invite); err != nil || got != pending {
		t.Fatalf("InviteAdmin = %+v, %v", got, err)
	}

	// A pending invite is sent again, not created again, and targets the id.
	// It is a resend if an earlier invite is on record under the id...
	gomock.InOrder(
		accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(pending, nil),
		log.EXPECT().List(ctx, priorInvite).Return(services.AuditPage{Total: 1}, nil),
		log.EXPECT().Begin(ctx, adminAudit(grace, "admin.invite", map[string]string{"resent": "true"})).Return(entryID, nil),
		accounts.EXPECT().SendSetPasswordLink(ctx, "a2").Return(nil),
		log.EXPECT().Resolve(ctx, entryID, succeeded).Return(nil),
	)
	if got, err := svc.InviteAdmin(ctx, grace, invite); err != nil || got != pending {
		t.Fatalf("resend = %+v, %v", got, err)
	}

	// ...or under the email (the first invite, written before the id existed).
	gomock.InOrder(
		accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(pending, nil),
		log.EXPECT().List(ctx, priorInvite).Return(services.AuditPage{}, nil),
		log.EXPECT().List(ctx, priorEmailInvite).Return(services.AuditPage{Total: 1}, nil),
		log.EXPECT().Begin(ctx, adminAudit(grace, "admin.invite", map[string]string{"resent": "true"})).Return(entryID, nil),
		accounts.EXPECT().SendSetPasswordLink(ctx, "a2").Return(nil),
		log.EXPECT().Resolve(ctx, entryID, succeeded).Return(nil),
	)
	if got, err := svc.InviteAdmin(ctx, grace, invite); err != nil || got != pending {
		t.Fatalf("resend after a first invite = %+v, %v", got, err)
	}

	// An admin seeded by bootstrap was never sent a link: the first invite is
	// not a resend.
	gomock.InOrder(
		accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(pending, nil),
		log.EXPECT().List(ctx, priorInvite).Return(services.AuditPage{}, nil),
		log.EXPECT().List(ctx, priorEmailInvite).Return(services.AuditPage{}, nil),
		log.EXPECT().Begin(ctx, adminAudit(grace, "admin.invite", map[string]string{})).Return(entryID, nil),
		accounts.EXPECT().SendSetPasswordLink(ctx, "a2").Return(nil),
		log.EXPECT().Resolve(ctx, entryID, succeeded).Return(nil),
	)
	if got, err := svc.InviteAdmin(ctx, grace, invite); err != nil || got != pending {
		t.Fatalf("first invite of a seeded admin = %+v, %v", got, err)
	}

	// The invite history cannot be read: nothing is written or sent.
	accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(pending, nil)
	log.EXPECT().List(ctx, priorInvite).Return(services.AuditPage{}, errDB)
	_, err := svc.InviteAdmin(ctx, grace, invite)
	wantInternal(t, err, errDB)

	accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(pending, nil)
	log.EXPECT().List(ctx, priorInvite).Return(services.AuditPage{}, nil)
	log.EXPECT().List(ctx, priorEmailInvite).Return(services.AuditPage{}, errDB)
	_, err = svc.InviteAdmin(ctx, grace, invite)
	wantInternal(t, err, errDB)

	// An admin who has set a password already is not invited.
	active := pending
	active.HasPassword = true
	accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(active, nil)
	_, err = svc.InviteAdmin(ctx, grace, invite)
	wantErr(t, err, services.ErrAdminExists)

	accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(services.Admin{}, errDB)
	_, err = svc.InviteAdmin(ctx, grace, invite)
	wantInternal(t, err, errDB)

	// Created but the link could not be sent: the invite failed as a whole.
	gomock.InOrder(
		accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(services.Admin{}, notFound),
		log.EXPECT().Begin(ctx, gomock.Any()).Return(entryID, nil),
		accounts.EXPECT().CreateAdmin(ctx, "bob@example.com", "Bob").Return(pending, nil),
		accounts.EXPECT().SendSetPasswordLink(ctx, "a2").Return(errDB),
		log.EXPECT().Resolve(ctx, entryID, internalFailure).Return(nil),
	)
	got, err := svc.InviteAdmin(ctx, grace, invite)
	wantInternal(t, err, errDB)
	if got != (services.Admin{}) {
		t.Fatalf("InviteAdmin on error = %+v, want none", got)
	}
}

func TestAdminActive(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		a    services.Admin
		want bool
	}{{bob, true}, {pending, false}, {disBob, false}, {services.Admin{Disabled: true}, false}} {
		if got := c.a.Active(); got != c.want {
			t.Errorf("%+v.Active() = %v, want %v", c.a, got, c.want)
		}
	}
}

func TestDisableAdmin(t *testing.T) {
	t.Parallel()
	svc, accounts, log := newAdminService(t)

	gomock.InOrder(
		accounts.EXPECT().AdminByID(ctx, "a2").Return(bob, nil),
		accounts.EXPECT().ListAdmins(ctx).Return([]services.Admin{graceAd, bob}, nil),
		log.EXPECT().Begin(ctx, adminAudit(grace, "admin.disable", nil)).Return(entryID, nil),
		accounts.EXPECT().DisableAdmin(ctx, "a2").Return(nil),
		log.EXPECT().Resolve(ctx, entryID, succeeded).Return(nil),
	)
	if err := svc.DisableAdmin(ctx, grace, "a2"); err != nil {
		t.Fatal(err)
	}

	// A pending invite never counts toward the last active admin.
	gomock.InOrder(
		accounts.EXPECT().AdminByID(ctx, "a2").Return(pending, nil),
		log.EXPECT().Begin(ctx, adminAudit(grace, "admin.disable", nil)).Return(entryID, nil),
		accounts.EXPECT().DisableAdmin(ctx, "a2").Return(nil),
		log.EXPECT().Resolve(ctx, entryID, succeeded).Return(nil),
	)
	if err := svc.DisableAdmin(ctx, grace, "a2"); err != nil {
		t.Fatal(err)
	}

	// Rules and lookups write nothing. Nobody disables themselves; it is
	// refused before any port call.
	wantErr(t, svc.DisableAdmin(ctx, grace, "a1"), services.ErrAdminSelf)

	accounts.EXPECT().AdminByID(ctx, "a2").Return(services.Admin{}, fmt.Errorf("get: %w", services.ErrRecordNotFound))
	wantErr(t, svc.DisableAdmin(ctx, grace, "a2"), services.ErrAdminNotFound)

	accounts.EXPECT().AdminByID(ctx, "a2").Return(services.Admin{}, errDB)
	wantInternal(t, svc.DisableAdmin(ctx, grace, "a2"), errDB)

	// The operator may act on bob, but never on the last active admin:
	// pending and disabled admins do not count.
	accounts.EXPECT().AdminByID(ctx, "a2").Return(bob, nil)
	accounts.EXPECT().ListAdmins(ctx).Return([]services.Admin{bob, pending, {ID: "a3", HasPassword: true, Disabled: true}}, nil)
	wantErr(t, svc.DisableAdmin(ctx, cli, "a2"), services.ErrLastAdmin)

	accounts.EXPECT().AdminByID(ctx, "a2").Return(bob, nil)
	accounts.EXPECT().ListAdmins(ctx).Return(nil, errDB)
	wantInternal(t, svc.DisableAdmin(ctx, grace, "a2"), errDB)

	// Already disabled: no count, no change.
	accounts.EXPECT().AdminByID(ctx, "a2").Return(disBob, nil)
	wantErr(t, svc.DisableAdmin(ctx, grace, "a2"), services.ErrAdminDisabled)
}

func TestEnableAdmin(t *testing.T) {
	t.Parallel()
	svc, accounts, log := newAdminService(t)

	gomock.InOrder(
		accounts.EXPECT().AdminByID(ctx, "a2").Return(disBob, nil),
		log.EXPECT().Begin(ctx, adminAudit(grace, "admin.enable", nil)).Return(entryID, nil),
		accounts.EXPECT().EnableAdmin(ctx, "a2").Return(nil),
		log.EXPECT().Resolve(ctx, entryID, succeeded).Return(nil),
	)
	if err := svc.EnableAdmin(ctx, grace, "a2"); err != nil {
		t.Fatal(err)
	}

	// Rules and lookups write nothing.
	for _, a := range []services.Admin{bob, pending} {
		accounts.EXPECT().AdminByID(ctx, "a2").Return(a, nil)
		wantErr(t, svc.EnableAdmin(ctx, grace, "a2"), services.ErrAdminNotDisabled)
	}

	accounts.EXPECT().AdminByID(ctx, "a2").Return(services.Admin{}, services.ErrRecordNotFound)
	wantErr(t, svc.EnableAdmin(ctx, grace, "a2"), services.ErrAdminNotFound)

	accounts.EXPECT().AdminByID(ctx, "a2").Return(services.Admin{}, errDB)
	wantInternal(t, svc.EnableAdmin(ctx, grace, "a2"), errDB)
}

func TestRemoveAdmin(t *testing.T) {
	t.Parallel()
	svc, accounts, log := newAdminService(t)

	gomock.InOrder(
		accounts.EXPECT().AdminByID(ctx, "a2").Return(bob, nil),
		accounts.EXPECT().ListAdmins(ctx).Return([]services.Admin{graceAd, bob}, nil),
		log.EXPECT().Begin(ctx, adminAudit(grace, "admin.remove", nil)).Return(entryID, nil),
		accounts.EXPECT().RemoveAdmin(ctx, "a2").Return(nil),
		log.EXPECT().Resolve(ctx, entryID, succeeded).Return(nil),
	)
	if err := svc.RemoveAdmin(ctx, grace, "a2"); err != nil {
		t.Fatal(err)
	}

	// Cancelling a pending invite or removing a disabled admin needs no count.
	for _, a := range []services.Admin{pending, disBob} {
		gomock.InOrder(
			accounts.EXPECT().AdminByID(ctx, "a2").Return(a, nil),
			log.EXPECT().Begin(ctx, adminAudit(grace, "admin.remove", nil)).Return(entryID, nil),
			accounts.EXPECT().RemoveAdmin(ctx, "a2").Return(nil),
			log.EXPECT().Resolve(ctx, entryID, succeeded).Return(nil),
		)
		if err := svc.RemoveAdmin(ctx, grace, "a2"); err != nil {
			t.Fatalf("remove %+v: %v", a, err)
		}
	}

	// Rules and lookups write nothing.
	wantErr(t, svc.RemoveAdmin(ctx, grace, "a1"), services.ErrAdminSelf)

	accounts.EXPECT().AdminByID(ctx, "a2").Return(services.Admin{}, services.ErrRecordNotFound)
	wantErr(t, svc.RemoveAdmin(ctx, grace, "a2"), services.ErrAdminNotFound)

	accounts.EXPECT().AdminByID(ctx, "a2").Return(services.Admin{}, errDB)
	wantInternal(t, svc.RemoveAdmin(ctx, grace, "a2"), errDB)

	accounts.EXPECT().AdminByID(ctx, "a2").Return(bob, nil)
	accounts.EXPECT().ListAdmins(ctx).Return([]services.Admin{bob}, nil)
	wantErr(t, svc.RemoveAdmin(ctx, cli, "a2"), services.ErrLastAdmin)

	accounts.EXPECT().AdminByID(ctx, "a2").Return(bob, nil)
	accounts.EXPECT().ListAdmins(ctx).Return(nil, errDB)
	wantInternal(t, svc.RemoveAdmin(ctx, grace, "a2"), errDB)
}

func TestBootstrap(t *testing.T) {
	t.Parallel()
	svc, accounts, log := newAdminService(t)

	// The intent targets the email: the admin is created inside the change.
	gomock.InOrder(
		accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(services.Admin{}, services.ErrRecordNotFound),
		log.EXPECT().Begin(ctx, services.NewAuditEntry{
			ActorID: "system:cli", ActorEmail: "cli", Action: "admin.bootstrap",
			TargetType: "admin", TargetID: "bob@example.com", TargetLabel: "bob@example.com",
		}).Return(entryID, nil),
		accounts.EXPECT().CreateAdmin(ctx, "bob@example.com", "Bob").Return(pending, nil),
		log.EXPECT().Resolve(ctx, entryID, succeeded).Return(nil),
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
}
