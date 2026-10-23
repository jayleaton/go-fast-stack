package services_test

import (
	"fmt"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services/mocks"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/validate"
)

var (
	invite  = validate.CreateAdminInvite{Email: "bob@example.com", Name: "Bob"}
	pending = services.Admin{ID: "a2", Name: "Bob", Email: "bob@example.com"}
	bob     = services.Admin{ID: "a2", Name: "Bob", Email: "bob@example.com", HasPassword: true}
	disBob  = services.Admin{ID: "a2", Name: "Bob", Email: "bob@example.com", HasPassword: true, Disabled: true}
	bob2fa  = services.Admin{ID: "a2", Name: "Bob", Email: "bob@example.com", HasPassword: true, TwoFactorEnabled: true}
	graceAd = services.Admin{ID: "a1", Name: "Grace", Email: "grace@example.com", HasPassword: true, TwoFactorEnabled: true}

	// priorInvite and priorEmailInvite are the audit lookups for an earlier
	// invite of bob: under his id, or under his email (a first invite).
	priorInvite      = services.AuditQuery{Action: "admin.invite", TargetID: "a2", Limit: 1}
	priorEmailInvite = services.AuditQuery{Action: "admin.invite", TargetID: "bob@example.com", Limit: 1}
)

func newAdminService(test *testing.T) (*services.AdminService, *mocks.MockAdminAccounts, *mocks.MockAuditLog) {
	ports := newPorts(test)
	return services.NewAdminService(ports.accounts, ports.log, services.Policy{RequireTwoFactor: true}), ports.accounts, ports.log
}

// adminAudit is the intent actor's action on bob writes.
func adminAudit(actor services.Actor, action string, details map[string]string) services.NewAuditEntry {
	return services.NewAuditEntry{
		ActorID: actor.AdminID, ActorEmail: actor.Email, Action: action,
		TargetType: "admin", TargetID: "a2", TargetLabel: "bob@example.com", Details: details,
	}
}

func TestListAdmins(test *testing.T) {
	test.Parallel()
	svc, accounts, _ := newAdminService(test)

	accounts.EXPECT().ListAdmins(ctx).Return([]services.Admin{pending}, nil)
	if got, err := svc.ListAdmins(ctx, grace); err != nil || len(got) != 1 || got[0] != pending {
		test.Fatalf("ListAdmins = %+v, %v", got, err)
	}

	accounts.EXPECT().ListAdmins(ctx).Return(nil, nil)
	if got, err := svc.ListAdmins(ctx, grace); err != nil || got == nil {
		test.Fatalf("no admins = %#v, %v; want an empty, non-nil list", got, err)
	}

	accounts.EXPECT().ListAdmins(ctx).Return(nil, errDB)
	_, err := svc.ListAdmins(ctx, grace)
	wantInternal(test, err, errDB)
}

// The generic fail-closed cases (intent or outcome not written, a refused or
// failing change) of every mutation are in TestMutationsFailClosed.

func TestInviteAdmin(test *testing.T) {
	test.Parallel()
	svc, accounts, log := newAdminService(test)
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
		test.Fatalf("InviteAdmin = %+v, %v", got, err)
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
		test.Fatalf("resend = %+v, %v", got, err)
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
		test.Fatalf("resend after a first invite = %+v, %v", got, err)
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
		test.Fatalf("first invite of a seeded admin = %+v, %v", got, err)
	}

	// The invite history cannot be read: nothing is written or sent.
	accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(pending, nil)
	log.EXPECT().List(ctx, priorInvite).Return(services.AuditPage{}, errDB)
	_, err := svc.InviteAdmin(ctx, grace, invite)
	wantInternal(test, err, errDB)

	accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(pending, nil)
	log.EXPECT().List(ctx, priorInvite).Return(services.AuditPage{}, nil)
	log.EXPECT().List(ctx, priorEmailInvite).Return(services.AuditPage{}, errDB)
	_, err = svc.InviteAdmin(ctx, grace, invite)
	wantInternal(test, err, errDB)

	// An admin who has set a password already is not invited.
	active := pending
	active.HasPassword = true
	accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(active, nil)
	_, err = svc.InviteAdmin(ctx, grace, invite)
	wantErr(test, err, services.ErrAdminExists)

	accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(services.Admin{}, errDB)
	_, err = svc.InviteAdmin(ctx, grace, invite)
	wantInternal(test, err, errDB)

	// Created but the link could not be sent: the invite failed as a whole.
	gomock.InOrder(
		accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(services.Admin{}, notFound),
		log.EXPECT().Begin(ctx, gomock.Any()).Return(entryID, nil),
		accounts.EXPECT().CreateAdmin(ctx, "bob@example.com", "Bob").Return(pending, nil),
		accounts.EXPECT().SendSetPasswordLink(ctx, "a2").Return(errDB),
		log.EXPECT().Resolve(ctx, entryID, internalFailure).Return(nil),
	)
	got, err := svc.InviteAdmin(ctx, grace, invite)
	wantInternal(test, err, errDB)
	if got != (services.Admin{}) {
		test.Fatalf("InviteAdmin on error = %+v, want none", got)
	}
}

func TestAdminActive(test *testing.T) {
	test.Parallel()
	for _, testCase := range []struct {
		admin services.Admin
		want  bool
	}{{bob, true}, {pending, false}, {disBob, false}, {services.Admin{Disabled: true}, false}} {
		if got := testCase.admin.Active(); got != testCase.want {
			test.Errorf("%+v.Active() = %v, want %v", testCase.admin, got, testCase.want)
		}
	}
}

func TestDisableAdmin(test *testing.T) {
	test.Parallel()
	svc, accounts, log := newAdminService(test)

	gomock.InOrder(
		accounts.EXPECT().AdminByID(ctx, "a2").Return(bob, nil),
		accounts.EXPECT().ListAdmins(ctx).Return([]services.Admin{graceAd, bob}, nil),
		log.EXPECT().Begin(ctx, adminAudit(grace, "admin.disable", nil)).Return(entryID, nil),
		accounts.EXPECT().DisableAdmin(ctx, "a2").Return(nil),
		log.EXPECT().Resolve(ctx, entryID, succeeded).Return(nil),
	)
	if err := svc.DisableAdmin(ctx, grace, "a2"); err != nil {
		test.Fatal(err)
	}

	// A pending invite never counts toward the last active admin.
	gomock.InOrder(
		accounts.EXPECT().AdminByID(ctx, "a2").Return(pending, nil),
		log.EXPECT().Begin(ctx, adminAudit(grace, "admin.disable", nil)).Return(entryID, nil),
		accounts.EXPECT().DisableAdmin(ctx, "a2").Return(nil),
		log.EXPECT().Resolve(ctx, entryID, succeeded).Return(nil),
	)
	if err := svc.DisableAdmin(ctx, grace, "a2"); err != nil {
		test.Fatal(err)
	}

	// Rules and lookups write nothing. Nobody disables themselves; it is
	// refused before any port call.
	wantErr(test, svc.DisableAdmin(ctx, grace, "a1"), services.ErrAdminSelf)

	accounts.EXPECT().AdminByID(ctx, "a2").Return(services.Admin{}, fmt.Errorf("get: %w", services.ErrRecordNotFound))
	wantErr(test, svc.DisableAdmin(ctx, grace, "a2"), services.ErrAdminNotFound)

	accounts.EXPECT().AdminByID(ctx, "a2").Return(services.Admin{}, errDB)
	wantInternal(test, svc.DisableAdmin(ctx, grace, "a2"), errDB)

	// The operator may act on bob, but never on the last active admin:
	// pending and disabled admins do not count.
	accounts.EXPECT().AdminByID(ctx, "a2").Return(bob, nil)
	accounts.EXPECT().ListAdmins(ctx).Return([]services.Admin{bob, pending, {ID: "a3", HasPassword: true, Disabled: true}}, nil)
	wantErr(test, svc.DisableAdmin(ctx, cli, "a2"), services.ErrLastAdmin)

	accounts.EXPECT().AdminByID(ctx, "a2").Return(bob, nil)
	accounts.EXPECT().ListAdmins(ctx).Return(nil, errDB)
	wantInternal(test, svc.DisableAdmin(ctx, grace, "a2"), errDB)

	// Already disabled: no count, no change.
	accounts.EXPECT().AdminByID(ctx, "a2").Return(disBob, nil)
	wantErr(test, svc.DisableAdmin(ctx, grace, "a2"), services.ErrAdminDisabled)
}

func TestEnableAdmin(test *testing.T) {
	test.Parallel()
	svc, accounts, log := newAdminService(test)

	gomock.InOrder(
		accounts.EXPECT().AdminByID(ctx, "a2").Return(disBob, nil),
		log.EXPECT().Begin(ctx, adminAudit(grace, "admin.enable", nil)).Return(entryID, nil),
		accounts.EXPECT().EnableAdmin(ctx, "a2").Return(nil),
		log.EXPECT().Resolve(ctx, entryID, succeeded).Return(nil),
	)
	if err := svc.EnableAdmin(ctx, grace, "a2"); err != nil {
		test.Fatal(err)
	}

	// Rules and lookups write nothing.
	for _, testCase := range []services.Admin{bob, pending} {
		accounts.EXPECT().AdminByID(ctx, "a2").Return(testCase, nil)
		wantErr(test, svc.EnableAdmin(ctx, grace, "a2"), services.ErrAdminNotDisabled)
	}

	accounts.EXPECT().AdminByID(ctx, "a2").Return(services.Admin{}, services.ErrRecordNotFound)
	wantErr(test, svc.EnableAdmin(ctx, grace, "a2"), services.ErrAdminNotFound)

	accounts.EXPECT().AdminByID(ctx, "a2").Return(services.Admin{}, errDB)
	wantInternal(test, svc.EnableAdmin(ctx, grace, "a2"), errDB)
}

func TestRemoveAdmin(test *testing.T) {
	test.Parallel()
	svc, accounts, log := newAdminService(test)

	gomock.InOrder(
		accounts.EXPECT().AdminByID(ctx, "a2").Return(bob, nil),
		accounts.EXPECT().ListAdmins(ctx).Return([]services.Admin{graceAd, bob}, nil),
		log.EXPECT().Begin(ctx, adminAudit(grace, "admin.remove", nil)).Return(entryID, nil),
		accounts.EXPECT().RemoveAdmin(ctx, "a2").Return(nil),
		log.EXPECT().Resolve(ctx, entryID, succeeded).Return(nil),
	)
	if err := svc.RemoveAdmin(ctx, grace, "a2"); err != nil {
		test.Fatal(err)
	}

	// Cancelling a pending invite or removing a disabled admin needs no count.
	for _, testCase := range []services.Admin{pending, disBob} {
		gomock.InOrder(
			accounts.EXPECT().AdminByID(ctx, "a2").Return(testCase, nil),
			log.EXPECT().Begin(ctx, adminAudit(grace, "admin.remove", nil)).Return(entryID, nil),
			accounts.EXPECT().RemoveAdmin(ctx, "a2").Return(nil),
			log.EXPECT().Resolve(ctx, entryID, succeeded).Return(nil),
		)
		if err := svc.RemoveAdmin(ctx, grace, "a2"); err != nil {
			test.Fatalf("remove %+v: %v", testCase, err)
		}
	}

	// Rules and lookups write nothing.
	wantErr(test, svc.RemoveAdmin(ctx, grace, "a1"), services.ErrAdminSelf)

	accounts.EXPECT().AdminByID(ctx, "a2").Return(services.Admin{}, services.ErrRecordNotFound)
	wantErr(test, svc.RemoveAdmin(ctx, grace, "a2"), services.ErrAdminNotFound)

	accounts.EXPECT().AdminByID(ctx, "a2").Return(services.Admin{}, errDB)
	wantInternal(test, svc.RemoveAdmin(ctx, grace, "a2"), errDB)

	accounts.EXPECT().AdminByID(ctx, "a2").Return(bob, nil)
	accounts.EXPECT().ListAdmins(ctx).Return([]services.Admin{bob}, nil)
	wantErr(test, svc.RemoveAdmin(ctx, cli, "a2"), services.ErrLastAdmin)

	accounts.EXPECT().AdminByID(ctx, "a2").Return(bob, nil)
	accounts.EXPECT().ListAdmins(ctx).Return(nil, errDB)
	wantInternal(test, svc.RemoveAdmin(ctx, grace, "a2"), errDB)
}

func TestBootstrap(test *testing.T) {
	test.Parallel()
	svc, accounts, log := newAdminService(test)

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
		test.Fatalf("Bootstrap = %+v, %v, %v", got, created, err)
	}

	// Idempotent: an existing admin is returned, nothing is created or audited.
	accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(pending, nil)
	if got, created, err := svc.Bootstrap(ctx, cli, invite); err != nil || created || got != pending {
		test.Fatalf("existing = %+v, %v, %v", got, created, err)
	}

	accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(services.Admin{}, errDB)
	_, created, err := svc.Bootstrap(ctx, cli, invite)
	wantInternal(test, err, errDB)
	if created {
		test.Fatal("created on error")
	}
}

// The generic fail-closed cases are in TestMutationsFailClosed.
func TestSendPasswordReset(test *testing.T) {
	test.Parallel()
	svc, accounts, log := newAdminService(test)

	// A disabled admin may get one too: they still cannot sign in.
	for _, admin := range []services.Admin{bob, disBob} {
		gomock.InOrder(
			accounts.EXPECT().AdminByID(ctx, "a2").Return(admin, nil),
			log.EXPECT().Begin(ctx, adminAudit(grace, "admin.password_reset", nil)).Return(entryID, nil),
			accounts.EXPECT().SendSetPasswordLink(ctx, "a2").Return(nil),
			log.EXPECT().Resolve(ctx, entryID, succeeded).Return(nil),
		)
		if err := svc.SendPasswordReset(ctx, grace, "a2"); err != nil {
			test.Fatalf("%+v: %v", admin, err)
		}
	}

	// Rules and lookups write nothing. Nobody resets themselves here.
	wantErr(test, svc.SendPasswordReset(ctx, grace, "a1"), services.ErrAdminSelfReset)

	accounts.EXPECT().AdminByID(ctx, "a2").Return(pending, nil)
	wantErr(test, svc.SendPasswordReset(ctx, grace, "a2"), services.ErrAdminPending)

	accounts.EXPECT().AdminByID(ctx, "a2").Return(services.Admin{}, services.ErrRecordNotFound)
	wantErr(test, svc.SendPasswordReset(ctx, grace, "a2"), services.ErrAdminNotFound)

	accounts.EXPECT().AdminByID(ctx, "a2").Return(services.Admin{}, errDB)
	wantInternal(test, svc.SendPasswordReset(ctx, grace, "a2"), errDB)
}

func TestResetTwoFactor(test *testing.T) {
	test.Parallel()
	svc, accounts, log := newAdminService(test)

	// Another admin, or the operator when nobody else can sign in.
	for _, actor := range []services.Actor{grace, cli} {
		gomock.InOrder(
			accounts.EXPECT().AdminByID(ctx, "a2").Return(bob2fa, nil),
			log.EXPECT().Begin(ctx, adminAudit(actor, "admin.reset_two_factor", nil)).Return(entryID, nil),
			accounts.EXPECT().ResetTwoFactor(ctx, "a2").Return(nil),
			log.EXPECT().Resolve(ctx, entryID, succeeded).Return(nil),
		)
		if err := svc.ResetTwoFactor(ctx, actor, "a2"); err != nil {
			test.Fatalf("%+v: %v", actor, err)
		}
	}

	// Rules and lookups write nothing.
	wantErr(test, svc.ResetTwoFactor(ctx, grace, "a1"), services.ErrAdminSelfReset)

	accounts.EXPECT().AdminByID(ctx, "a2").Return(bob, nil)
	wantErr(test, svc.ResetTwoFactor(ctx, grace, "a2"), services.ErrAdminNoTwoFactor)

	accounts.EXPECT().AdminByID(ctx, "a2").Return(pending, nil)
	wantErr(test, svc.ResetTwoFactor(ctx, grace, "a2"), services.ErrAdminPending)

	accounts.EXPECT().AdminByID(ctx, "a2").Return(services.Admin{}, fmt.Errorf("get: %w", services.ErrRecordNotFound))
	wantErr(test, svc.ResetTwoFactor(ctx, grace, "a2"), services.ErrAdminNotFound)

	accounts.EXPECT().AdminByID(ctx, "a2").Return(services.Admin{}, errDB)
	wantInternal(test, svc.ResetTwoFactor(ctx, grace, "a2"), errDB)
}

func TestResetTwoFactorByEmail(test *testing.T) {
	test.Parallel()
	svc, accounts, log := newAdminService(test)
	in := validate.ResetAdminTwoFactor{Email: "bob@example.com"}

	gomock.InOrder(
		accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(bob2fa, nil),
		accounts.EXPECT().AdminByID(ctx, "a2").Return(bob2fa, nil),
		log.EXPECT().Begin(ctx, adminAudit(cli, "admin.reset_two_factor", nil)).Return(entryID, nil),
		accounts.EXPECT().ResetTwoFactor(ctx, "a2").Return(nil),
		log.EXPECT().Resolve(ctx, entryID, succeeded).Return(nil),
	)
	if got, err := svc.ResetTwoFactorByEmail(ctx, cli, in); err != nil || got.ID != "a2" || got.TwoFactorEnabled {
		test.Fatalf("ResetTwoFactorByEmail = %+v, %v", got, err)
	}

	// Lookups and rules write nothing.
	accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(services.Admin{}, services.ErrRecordNotFound)
	_, err := svc.ResetTwoFactorByEmail(ctx, cli, in)
	wantErr(test, err, services.ErrAdminNotFound)

	accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(services.Admin{}, errDB)
	_, err = svc.ResetTwoFactorByEmail(ctx, cli, in)
	wantInternal(test, err, errDB)

	accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(bob, nil)
	accounts.EXPECT().AdminByID(ctx, "a2").Return(bob, nil)
	_, err = svc.ResetTwoFactorByEmail(ctx, cli, in)
	wantErr(test, err, services.ErrAdminNoTwoFactor)
}

func TestInviteBootstrapAdmin(test *testing.T) {
	test.Parallel()
	svc, accounts, log := newAdminService(test)

	// A pending admin with no invite on record (under the id or the email)
	// gets the link once, by the operator.
	bootstrap := services.SystemActor("bootstrap")
	gomock.InOrder(
		log.EXPECT().List(ctx, priorInvite).Return(services.AuditPage{}, nil),
		log.EXPECT().List(ctx, priorEmailInvite).Return(services.AuditPage{}, nil),
		log.EXPECT().Begin(ctx, adminAudit(bootstrap, "admin.invite", map[string]string{})).Return(entryID, nil),
		accounts.EXPECT().SendSetPasswordLink(ctx, "a2").Return(nil),
		log.EXPECT().Resolve(ctx, entryID, succeeded).Return(nil),
	)
	if sent, err := svc.InviteBootstrapAdmin(ctx, bootstrap, pending); err != nil || !sent {
		test.Fatalf("InviteBootstrapAdmin = %v, %v", sent, err)
	}

	// Later deploys send nothing: an invite is on record, or it was accepted.
	log.EXPECT().List(ctx, priorInvite).Return(services.AuditPage{Total: 1}, nil)
	if sent, err := svc.InviteBootstrapAdmin(ctx, bootstrap, pending); err != nil || sent {
		test.Fatalf("invited before = %v, %v", sent, err)
	}
	if sent, err := svc.InviteBootstrapAdmin(ctx, bootstrap, bob); err != nil || sent {
		test.Fatalf("accepted = %v, %v", sent, err)
	}

	log.EXPECT().List(ctx, priorInvite).Return(services.AuditPage{}, errDB)
	sent, err := svc.InviteBootstrapAdmin(ctx, bootstrap, pending)
	wantInternal(test, err, errDB)
	if sent {
		test.Fatal("sent on error")
	}
}
