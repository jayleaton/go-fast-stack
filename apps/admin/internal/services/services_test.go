package services_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/services/mocks"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/validate"
)

var (
	ctx     = context.Background()
	grace   = services.Actor{AdminID: "a1", Email: "grace@example.com", SessionID: "s1", TwoFactorEnabled: true}
	cli     = services.SystemActor("cli")
	adaUser = services.User{ID: "u1", Name: "Ada", Email: "ada@example.com", Role: "user"}
	errDB   = errors.New("connection refused")
)

func ptr[T any](v T) *T { return &v }

// ports holds every generated port mock; a service under test uses some.
type ports struct {
	dir       *mocks.MockUserDirectory
	accounts  *mocks.MockAdminAccounts
	log       *mocks.MockAuditLog
	analytics *mocks.MockAnalytics
}

func newPorts(t *testing.T) ports {
	ctrl := gomock.NewController(t)
	return ports{
		dir:       mocks.NewMockUserDirectory(ctrl),
		accounts:  mocks.NewMockAdminAccounts(ctrl),
		log:       mocks.NewMockAuditLog(ctrl),
		analytics: mocks.NewMockAnalytics(ctrl),
	}
}

// wantErr fails unless errors.Is(err, target).
func wantErr(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("err = %v, want %v", err, target)
	}
}

// wantInternal fails unless err is cause passed through as an internal error.
func wantInternal(t *testing.T, err, cause error) {
	t.Helper()
	if services.KindOf(err) != services.KindInternal || !errors.Is(err, cause) {
		t.Fatalf("err = %v, want internal wrapping %v", err, cause)
	}
}

// policyCalls calls every method of every service with actor, over ports p.
func policyCalls(p ports, policy services.Policy) map[string]func(services.Actor) error {
	users := services.NewUserService(p.dir, p.log, policy)
	admins := services.NewAdminService(p.accounts, p.log, policy)
	auditSvc := services.NewAuditService(p.log, policy)
	analytics := services.NewAnalyticsService(p.analytics, policy)
	return map[string]func(services.Actor) error{
		"UserService.ListUsers": func(a services.Actor) error {
			_, err := users.ListUsers(ctx, a, validate.ListUsers{Limit: 25})
			return err
		},
		"UserService.GetUser": func(a services.Actor) error { _, err := users.GetUser(ctx, a, "u1"); return err },
		"UserService.BanUser": func(a services.Actor) error {
			return users.BanUser(ctx, a, "u1", validate.BanUser{BanReason: "spam"})
		},
		"UserService.UnbanUser": func(a services.Actor) error { return users.UnbanUser(ctx, a, "u1") },
		"UserService.SetRole": func(a services.Actor) error {
			return users.SetRole(ctx, a, "u1", validate.SetUserRole{Role: "admin"})
		},
		"UserService.RevokeSession":  func(a services.Actor) error { return users.RevokeSession(ctx, a, "u1", "s9") },
		"UserService.RevokeSessions": func(a services.Actor) error { return users.RevokeSessions(ctx, a, "u1") },
		"AdminService.ListAdmins":    func(a services.Actor) error { _, err := admins.ListAdmins(ctx, a); return err },
		"AdminService.InviteAdmin": func(a services.Actor) error {
			_, err := admins.InviteAdmin(ctx, a, invite)
			return err
		},
		"AdminService.Bootstrap": func(a services.Actor) error {
			_, _, err := admins.Bootstrap(ctx, a, invite)
			return err
		},
		"AuditService.List": func(a services.Actor) error {
			_, err := auditSvc.List(ctx, a, validate.ListAuditLog{Limit: 25})
			return err
		},
		"AnalyticsService.Overview": func(a services.Actor) error { _, err := analytics.Overview(ctx, a); return err },
	}
}

// Every method refuses before touching a port: the mocks have no
// expectations, so any call fails the test.
func TestServicesRefuseUnauthorizedActors(t *testing.T) {
	policy := services.Policy{RequireTwoFactor: true}
	for name := range policyCalls(newPorts(t), policy) {
		t.Run(name, func(t *testing.T) {
			call := policyCalls(newPorts(t), policy)[name]            // mocks bound to this subtest
			wantErr(t, call(services.Actor{}), services.ErrForbidden) // no actor is nobody
			noTwoFactor := grace
			noTwoFactor.TwoFactorEnabled = false
			err := call(noTwoFactor)
			if name == "AdminService.Bootstrap" {
				wantErr(t, err, services.ErrForbidden) // only the operator bootstraps
				wantErr(t, call(grace), services.ErrForbidden)
				return
			}
			wantErr(t, err, services.ErrTwoFactorRequired)
		})
	}
}

// Without the 2FA requirement any admin may act, and the operator always may.
func TestPolicyAllowsAdminsAndSystem(t *testing.T) {
	for _, policy := range []services.Policy{{}, {RequireTwoFactor: true}} {
		for _, actor := range []services.Actor{cli, {AdminID: "a2", Email: "bob@example.com"}} {
			if policy.RequireTwoFactor && !actor.System {
				continue // covered by TestServicesRefuseUnauthorizedActors
			}
			p := newPorts(t)
			p.analytics.EXPECT().Overview(ctx).Return(services.Overview{TotalUsers: 3}, nil)
			got, err := services.NewAnalyticsService(p.analytics, policy).Overview(ctx, actor)
			if err != nil || got.TotalUsers != 3 {
				t.Fatalf("policy %+v, actor %+v: Overview = %+v, %v", policy, actor, got, err)
			}
		}
	}
}

func TestSystemActor(t *testing.T) {
	want := services.Actor{AdminID: "system:cli", Email: "cli", System: true}
	if cli != want {
		t.Fatalf("SystemActor = %+v, want %+v", cli, want)
	}
}

// Kind sentinels match every error of their kind; specific errors only themselves.
func TestErrorKinds(t *testing.T) {
	cases := []struct {
		err, target error
		want        bool
	}{
		{services.ErrAdminExists, services.ErrConflict, true},
		{services.ErrRoleUnchanged, services.ErrValidation, true},
		{fmt.Errorf("wrapped: %w", services.ErrSessionNotFound), services.ErrNotFound, true},
		{services.ErrTwoFactorRequired, services.ErrForbidden, true},
		{services.ErrForbidden, services.ErrTwoFactorRequired, false},
		{services.ErrAdminExists, services.ErrNotFound, false},
		{services.ErrSessionNotFound, services.ErrUserNotFound, false},
		{services.ErrNotFound, services.ErrUserNotFound, false},
		{errDB, services.ErrNotFound, false},
	}
	for _, c := range cases {
		if got := errors.Is(c.err, c.target); got != c.want {
			t.Errorf("errors.Is(%v, %v) = %v, want %v", c.err, c.target, got, c.want)
		}
	}
	if services.KindOf(services.ErrAdminExists) != services.KindConflict || services.KindOf(errDB) != services.KindInternal {
		t.Error("KindOf")
	}
	if services.ErrUserNotFound.Error() != "User not found" {
		t.Errorf("Error() = %q", services.ErrUserNotFound.Error())
	}
}
